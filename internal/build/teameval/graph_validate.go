package teameval

// ValidateEmployeeGraph is the employee internal-graph validator (plan
// §10.2.2 / §10.2 validator-as-first-class-contract), shared with the
// teamforge tool layer. registry.GraphDefinition.Validate only checks
// worker-step config and knows nothing about reachability or cycles, so the
// validator implements the full check list here:
//
//   - entry exists and reaches every step; no unreachable steps;
//   - every step except an explicitly ended (terminal) step has an out-edge;
//   - cycles are allowed only when they contain a condition edge (self loops
//     and condition-free cycles are rejected);
//   - step config structure per internal/declarative/factory.go (types,
//     required fields, enums);
//   - condition keys must be provably produced upstream (llm_check
//     output_key or llm_call extract.key);
//   - worker steps are forbidden in employee internal graphs.
//
// Every finding carries a stable code plus a model-facing hint; warnings
// (missing L1 capability points) are reported but never block a commit.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/declarative/schema"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// Stable validator codes. The codes are the machine contract between the
// validator and the model: messages and hints may evolve, codes must not.
const (
	GraphCodeEntryMissing           = "graph_entry_missing"
	GraphCodeEntryTargetMissing     = "graph_entry_target_missing"
	GraphCodeStepNameRequired       = "graph_step_name_required"
	GraphCodeDuplicateStepName      = "graph_duplicate_step_name"
	GraphCodeUnknownStepType        = "graph_unknown_step_type"
	GraphCodeWorkerForbidden        = "graph_worker_forbidden"
	GraphCodeNextConditionExclusive = "graph_next_condition_exclusive"
	GraphCodeNextTargetMissing      = "graph_next_target_missing"
	GraphCodeConditionTargetMissing = "graph_condition_target_missing"
	GraphCodeStepUnreachable        = "graph_step_unreachable"
	GraphCodeStepMissingOutEdge     = "graph_step_missing_out_edge"
	GraphCodeSelfLoop               = "graph_self_loop"
	GraphCodeCycleWithoutCondition  = "graph_cycle_without_condition"
	GraphCodeConditionKeyUnprovable = "graph_condition_key_unprovable"
	GraphCodeStepConfigInvalid      = "graph_step_config_invalid"

	GraphCodeWarningNoInputExtraction = "graph_warning_no_input_extraction"
	GraphCodeWarningNoOutput          = "graph_warning_no_output"
	GraphCodeWarningNoSelfCheck       = "graph_warning_no_self_check"
)

// GraphProblem is one structured validation finding. Path locates the field
// (graph.entry, graph.steps[<name>].config.<key>, ...), Step names the owning
// step when there is one, and Hint is the repair guidance for the model.
type GraphProblem struct {
	Path    string `json:"path"`
	Step    string `json:"step,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// GraphValidation is the full validator result: errors block a commit,
// warnings only enter the report.
type GraphValidation struct {
	Errors   []GraphProblem `json:"errors"`
	Warnings []GraphProblem `json:"warnings"`
	Valid    bool           `json:"valid"`
}

// ValidateEmployeeGraph runs the complete employee internal-graph check list
// over one graph definition. The result is always fully populated: no error
// short-circuits the scan, so the model receives every fixable problem in one
// report.
func ValidateEmployeeGraph(def *registry.GraphDefinition) GraphValidation {
	var out GraphValidation
	if def == nil {
		def = &registry.GraphDefinition{}
	}

	// Structural name and type checks (pass 1: build the full name table
	// before any target lookups so wiring can reference later steps).
	names := make(map[string]int, len(def.Steps))
	if def.Entry == "" {
		out.Errors = append(out.Errors, GraphProblem{
			Path:    "graph.entry",
			Code:    GraphCodeEntryMissing,
			Message: "entry is required",
			Hint:    "use set_entry to choose the graph's first step",
		})
	}
	for i := range def.Steps {
		step := &def.Steps[i]
		path := "graph.steps[" + step.Name + "]"
		if step.Name == "" {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    "graph.steps",
				Step:    step.Name,
				Code:    GraphCodeStepNameRequired,
				Message: "step name is required",
				Hint:    "add_step requires a non-empty name",
			})
			continue
		}
		if _, dup := names[step.Name]; dup {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    path,
				Step:    step.Name,
				Code:    GraphCodeDuplicateStepName,
				Message: fmt.Sprintf("duplicate step name %q", step.Name),
				Hint:    "remove the duplicate step or rename it",
			})
		} else {
			names[step.Name] = i
		}

		switch step.Type {
		case "worker":
			out.Errors = append(out.Errors, GraphProblem{
				Path:    path + ".type",
				Step:    step.Name,
				Code:    GraphCodeWorkerForbidden,
				Message: "worker steps are forbidden in employee internal graphs",
				Hint:    "replace it with chat/llm_call/llm_check/transform/builtin/yield; cross-employee orchestration belongs to team workflows",
			})
		case "chat", "llm_call", "llm_check", "transform", "builtin", "yield":
			out.Errors = append(out.Errors, validateStepConfig(path, step.Name, step.Type, step.Config)...)
		default:
			out.Errors = append(out.Errors, GraphProblem{
				Path:    path + ".type",
				Step:    step.Name,
				Code:    GraphCodeUnknownStepType,
				Message: fmt.Sprintf("unknown step type %q", step.Type),
				Hint:    "allowed types: chat, llm_call, llm_check, transform, builtin, yield",
			})
		}
	}

	// Wiring contract (pass 2): next and condition are mutually exclusive,
	// and every non-empty target must name an existing step.
	for i := range def.Steps {
		step := &def.Steps[i]
		if step.Name == "" {
			continue // already reported in pass 1
		}
		path := "graph.steps[" + step.Name + "]"
		if step.Next != nil && step.Condition != nil {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    path,
				Step:    step.Name,
				Code:    GraphCodeNextConditionExclusive,
				Message: "next and condition are mutually exclusive",
				Hint:    "route the step with connect_next OR connect_condition, never both",
			})
		}
		if step.Next != nil && *step.Next != "" {
			if _, ok := names[*step.Next]; !ok {
				out.Errors = append(out.Errors, GraphProblem{
					Path:    path + ".next",
					Step:    step.Name,
					Code:    GraphCodeNextTargetMissing,
					Message: fmt.Sprintf("next target %q not found", *step.Next),
					Hint:    "connect_next to an existing step, or pass an empty next to end the graph",
				})
			}
		}
		if step.Condition != nil {
			for _, branch := range []struct {
				name   string
				target *string
			}{
				{name: "true", target: step.Condition.TrueStep},
				{name: "false", target: step.Condition.FalseStep},
			} {
				if branch.target != nil && *branch.target != "" {
					if _, ok := names[*branch.target]; !ok {
						out.Errors = append(out.Errors, GraphProblem{
							Path:    path + ".condition." + branch.name,
							Step:    step.Name,
							Code:    GraphCodeConditionTargetMissing,
							Message: fmt.Sprintf("condition %s target %q not found", branch.name, *branch.target),
							Hint:    "connect_condition to an existing step, or leave the branch empty to end the graph",
						})
					}
				}
			}
		}
	}

	// Entry target must exist before any reachability scan can run.
	_, entryOK := names[def.Entry]
	if !entryOK {
		if def.Entry != "" {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    "graph.entry",
				Code:    GraphCodeEntryTargetMissing,
				Message: fmt.Sprintf("entry step %q not found in steps", def.Entry),
				Hint:    "set_entry to one of the existing steps",
			})
		}
		out.Valid = len(out.Errors) == 0
		return out
	}

	// Every step must have an out-edge: a step with no next and no condition
	// is not automatically a terminal here — the model must explicitly end a
	// branch (empty next, or an empty condition branch) so dangling steps are
	// caught instead of silently becoming sinks.
	for i := range def.Steps {
		step := &def.Steps[i]
		if step.Next == nil && step.Condition == nil {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    "graph.steps[" + step.Name + "]",
				Step:    step.Name,
				Code:    GraphCodeStepMissingOutEdge,
				Message: fmt.Sprintf("step %q has no out-edge and is not explicitly ended", step.Name),
				Hint:    "connect it with connect_next (an empty next explicitly ends the graph) or route it with connect_condition",
			})
		}
	}

	adjacency := BuildGraphAdjacency(def)
	reachable := reachableFrom(def.Entry, adjacency)

	for i := range def.Steps {
		step := &def.Steps[i]
		if reachable[step.Name] {
			continue
		}
		out.Errors = append(out.Errors, GraphProblem{
			Path:    "graph.steps[" + step.Name + "]",
			Step:    step.Name,
			Code:    GraphCodeStepUnreachable,
			Message: fmt.Sprintf("step %q is unreachable from the entry", step.Name),
			Hint:    "remove it, or connect it to the reachable subgraph with connect_next/connect_condition",
		})
	}

	// A graph where every reachable step routes onward never ends. This is
	// the reachability side of "除终止步外每步有出边": the entry-reachable
	// subgraph must contain at least one explicitly ended step.
	reachableEnds := false
	for i := range def.Steps {
		step := &def.Steps[i]
		if !reachable[step.Name] {
			continue
		}
		if step.Next != nil && *step.Next == "" {
			reachableEnds = true
			break
		}
		if step.Condition != nil && (branchEnds(step.Condition.TrueStep) || branchEnds(step.Condition.FalseStep)) {
			reachableEnds = true
			break
		}
	}
	if !reachableEnds {
		out.Errors = append(out.Errors, GraphProblem{
			Path:    "graph",
			Code:    GraphCodeStepMissingOutEdge,
			Message: "no reachable step ends the graph; every reachable step routes onward",
			Hint:    "end a step with connect_next(next=\"\") or leave one condition branch empty",
		})
	}

	out.Errors = append(out.Errors, findGraphCycles(def, adjacency)...)

	// Condition keys must be produced upstream (the producing step itself
	// counts: the step runs before its router reads the produced bool).
	for i := range def.Steps {
		step := &def.Steps[i]
		if step.Condition == nil {
			continue
		}
		key := strings.TrimSpace(step.Condition.Key)
		if key == "" {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    "graph.steps[" + step.Name + "].condition.key",
				Step:    step.Name,
				Code:    GraphCodeConditionKeyUnprovable,
				Message: "condition key is empty",
				Hint:    "connect_condition requires the state key produced by an llm_check output_key or an llm_call extract.key",
			})
			continue
		}
		if !GraphKeyProvable(def, adjacency, step.Name, key) && !isInjectedStateKey(key) {
			out.Errors = append(out.Errors, GraphProblem{
				Path:    "graph.steps[" + step.Name + "].condition.key",
				Step:    step.Name,
				Code:    GraphCodeConditionKeyUnprovable,
				Message: fmt.Sprintf("condition key %q is not provably produced by an upstream step", key),
				Hint:    fmt.Sprintf("ensure an llm_check output_key or an llm_call extract.key produces %q on a step that reaches this condition", key),
			})
		}
	}

	out.Warnings = capabilityWarnings(def)
	out.Valid = len(out.Errors) == 0
	return out
}

func branchEnds(target *string) bool {
	return target == nil || *target == ""
}

// --- graph topology helpers ---

type graphEdge struct {
	to          string
	isCondition bool
}

// BuildGraphAdjacency builds the directed adjacency table of a graph
// definition, shared by the validator and the teamforge graph write tools.
func BuildGraphAdjacency(def *registry.GraphDefinition) map[string][]graphEdge {
	adjacency := make(map[string][]graphEdge, len(def.Steps))
	for _, step := range def.Steps {
		if step.Next != nil && *step.Next != "" {
			adjacency[step.Name] = append(adjacency[step.Name], graphEdge{to: *step.Next})
		}
		if step.Condition != nil {
			if step.Condition.TrueStep != nil && *step.Condition.TrueStep != "" {
				adjacency[step.Name] = append(adjacency[step.Name], graphEdge{to: *step.Condition.TrueStep, isCondition: true})
			}
			if step.Condition.FalseStep != nil && *step.Condition.FalseStep != "" {
				adjacency[step.Name] = append(adjacency[step.Name], graphEdge{to: *step.Condition.FalseStep, isCondition: true})
			}
		}
	}
	return adjacency
}

func reachableFrom(entry string, adjacency map[string][]graphEdge) map[string]bool {
	seen := make(map[string]bool)
	stack := []string{entry}
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, edge := range adjacency[name] {
			if !seen[edge.to] {
				stack = append(stack, edge.to)
			}
		}
	}
	return seen
}

// isInjectedStateKey reports whether a condition key names platform-injected
// runtime state (the "meta_" prefix) rather than a step-produced value. The
// meta team planning assembly injects these keys into the run state before
// the graph starts, so they cannot be traced to an upstream step; requiring
// one would force a fabricate-the-key transform. Restricted to the prefix so
// arbitrary unproven keys on employee graphs still fail validation.
func isInjectedStateKey(key string) bool {
	return strings.HasPrefix(key, "meta_")
}

// GraphKeyProvable reports whether the condition key is produced by a step
// that can reach the condition step. The producing step itself counts
// (factory.go routes a step's own llm_check output_key), matching the
// "自检" reference skeleton.
func GraphKeyProvable(def *registry.GraphDefinition, adjacency map[string][]graphEdge, conditionName, key string) bool {
	for _, producer := range def.Steps {
		if !stepProducesKey(producer, key) {
			continue
		}
		if producer.Name == conditionName {
			return true
		}
		if reachableFrom(producer.Name, adjacency)[conditionName] {
			return true
		}
	}
	return false
}

// stepProducesKey reports whether one step writes the given bool state key:
// llm_check output_key or llm_call extract.key.
func stepProducesKey(step registry.StepDefinition, key string) bool {
	switch step.Type {
	case "llm_check":
		return configString(step.Config, "output_key", "check_result") == key
	case "llm_call":
		extract, ok := step.Config["extract"].(map[string]any)
		if !ok {
			return false
		}
		return configString(extract, "key", "") == key
	default:
		return false
	}
}

// findGraphCycles reports self loops and condition-free cycles. A cycle is
// allowed only when at least one of its edges is a condition edge ("环只允许
// 经 condition 回边形成"): the parenthetical rejects self loops and
// unconditional cycles, and every non-self cycle containing a condition edge
// passes through a self-check that can exit it. Reported cycles are
// deduplicated by their sorted node set.
func findGraphCycles(def *registry.GraphDefinition, adjacency map[string][]graphEdge) []GraphProblem {
	const (
		white = iota
		gray
		black
	)
	state := make(map[string]int, len(def.Steps))
	stack := make([]string, 0, len(def.Steps))
	reported := make(map[string]bool)
	var problems []GraphProblem

	var dfs func(name string)
	dfs = func(name string) {
		state[name] = gray
		stack = append(stack, name)
		for _, edge := range adjacency[name] {
			switch state[edge.to] {
			case white:
				dfs(edge.to)
			case gray:
				// Back edge name -> edge.to closes a cycle.
				cycleStart := 0
				for i, node := range stack {
					if node == edge.to {
						cycleStart = i
						break
					}
				}
				cycle := append([]string(nil), stack[cycleStart:]...)
				cycle = append(cycle, name)

				if isSelfLoopCycle(cycle) {
					if reported["self:"+name] {
						continue
					}
					reported["self:"+name] = true
					problems = append(problems, GraphProblem{
						Path:    "graph.steps[" + name + "]",
						Step:    name,
						Code:    GraphCodeSelfLoop,
						Message: fmt.Sprintf("step %q routes to itself; self loops are forbidden", name),
						Hint:    "route the step to another step, or use a condition back-edge to an earlier step",
					})
					continue
				}

				hasConditionEdge := edge.isCondition
				for i := cycleStart; i < len(stack)-1; i++ {
					if edgeIsCondition(adjacency, stack[i], stack[i+1]) {
						hasConditionEdge = true
						break
					}
				}
				key := cycleKey(cycle)
				if reported[key] {
					continue
				}
				reported[key] = true
				if !hasConditionEdge {
					problems = append(problems, GraphProblem{
						Path:    "graph.steps[" + name + "]",
						Step:    name,
						Code:    GraphCodeCycleWithoutCondition,
						Message: fmt.Sprintf("cycle %s contains no condition edge; unconditional cycles are forbidden", strings.Join(cycle, " -> ")),
						Hint:    "route at least one edge of the cycle through a condition so a self-check can exit the loop",
					})
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = black
	}

	for _, step := range def.Steps {
		if state[step.Name] == white {
			dfs(step.Name)
		}
	}
	return problems
}

func isSelfLoopCycle(cycle []string) bool {
	return len(cycle) == 2 && cycle[0] == cycle[1]
}

func edgeIsCondition(adjacency map[string][]graphEdge, from, to string) bool {
	for _, edge := range adjacency[from] {
		if edge.to == to {
			return edge.isCondition
		}
	}
	return false
}

func cycleKey(cycle []string) string {
	nodes := append([]string(nil), cycle[:len(cycle)-1]...)
	sort.Strings(nodes)
	return strings.Join(nodes, ">")
}

// --- L1 capability-point warnings (non-blocking) ---

func capabilityWarnings(def *registry.GraphDefinition) []GraphProblem {
	var hasTransform, hasOutput, hasSelfCheck bool
	for _, step := range def.Steps {
		switch step.Type {
		case "transform":
			hasTransform = true
		case "chat", "llm_call":
			hasOutput = true
			if step.Type == "llm_call" {
				if extract, ok := step.Config["extract"].(map[string]any); ok {
					if key, _ := extract["key"].(string); key != "" {
						hasSelfCheck = true
					}
				}
			}
		case "llm_check":
			hasSelfCheck = true
		}
	}
	var warnings []GraphProblem
	if !hasTransform {
		warnings = append(warnings, GraphProblem{
			Path:    "graph",
			Code:    GraphCodeWarningNoInputExtraction,
			Message: "graph has no input-extraction capability point (no transform step)",
			Hint:    "add a transform step that extracts task fields from the run input (reference: transform → llm_call → llm_check)",
		})
	}
	if !hasOutput {
		warnings = append(warnings, GraphProblem{
			Path:    "graph",
			Code:    GraphCodeWarningNoOutput,
			Message: "graph has no output-producing capability point (no llm_call/chat step)",
			Hint:    "add an llm_call step that produces the deliverable",
		})
	}
	if !hasSelfCheck {
		warnings = append(warnings, GraphProblem{
			Path:    "graph",
			Code:    GraphCodeWarningNoSelfCheck,
			Message: "graph has no self-check capability point (no llm_check step and no llm_call extract)",
			Hint:    "add an llm_check step and route failures back through a condition",
		})
	}
	return warnings
}

// --- per-type config validation (schema-driven; single source in
// internal/declarative/schema, consumed by the freeze-time encoder too) ---

// validateStepConfig validates one step's config map against the shared
// declarative schema: unknown fields are rejected (the freezer rejects them
// at freeze time), required fields are enforced, and per-type cross-field
// rules (extract/keyword mode) run on top.
func validateStepConfig(path, stepName, stepType string, config map[string]any) []GraphProblem {
	step, ok := schema.StepFor(stepType)
	if !ok {
		return nil // worker/unknown step types are reported by the caller
	}
	problems := validateConfigFields(path+".config", stepName, stepType, step, config)
	switch stepType {
	case "llm_call":
		problems = append(problems, validateLLMCallExtract(path, stepName, config)...)
	case "llm_check":
		if configString(config, "extract_mode", "json") == "keyword" && len(configStrings(config, "keywords_true")) == 0 {
			problems = append(problems, configProblem(path+".config.keywords_true", stepName,
				GraphCodeStepConfigInvalid, "extract_mode keyword requires non-empty keywords_true",
				"list the keywords that count as passing the check"))
		}
	case "transform":
		problems = append(problems, validateTransformConfig(path, stepName, config)...)
	}
	return problems
}

// validateConfigFields checks one config container (a step config, the
// nested extract object, or a transform operation) against its schema.
// basePath is the container's JSON path; field paths are
// basePath.<key>.
func validateConfigFields(basePath, stepName, stepType string, step schema.Step, config map[string]any) []GraphProblem {
	var problems []GraphProblem
	for _, field := range step.Fields {
		value, ok := config[field.Key]
		if !ok {
			if field.Required {
				problems = append(problems, configProblem(basePath+"."+field.Key, stepName,
					GraphCodeStepConfigInvalid,
					fmt.Sprintf("config.%s is required", field.Key),
					requiredFieldHint(field.Key)))
			}
			continue
		}
		problems = append(problems, validateFieldValue(basePath, stepName, field, value)...)
	}
	for key := range config {
		if step.HasField(key) {
			continue
		}
		problems = append(problems, unknownFieldProblem(basePath, stepName, stepType, key, step))
	}
	return problems
}

func validateFieldValue(basePath, stepName string, field schema.Field, value any) []GraphProblem {
	fieldPath := basePath + "." + field.Key
	switch field.Kind {
	case schema.KindString:
		if _, ok := value.(string); !ok {
			return []GraphProblem{configTypeProblem(fieldPath, stepName, field.Key, "a string")}
		}
	case schema.KindNumber:
		if !configNumber(value) {
			return []GraphProblem{configTypeProblem(fieldPath, stepName, field.Key, "a number")}
		}
	case schema.KindBool:
		if _, ok := value.(bool); !ok {
			return []GraphProblem{configTypeProblem(fieldPath, stepName, field.Key, "a boolean")}
		}
	case schema.KindStringArray:
		if _, err := configStringSlice(value); err != nil {
			return []GraphProblem{configTypeProblem(fieldPath, stepName, field.Key, "an array of strings")}
		}
	case schema.KindEnum:
		str, ok := value.(string)
		if !ok || !stringIn(str, field.Enum) {
			return []GraphProblem{configProblem(fieldPath, stepName,
				GraphCodeStepConfigInvalid,
				fmt.Sprintf("config.%s must be one of %s", field.Key, strings.Join(field.Enum, ", ")),
				"use one of the allowed values")}
		}
	case schema.KindExtract:
		if _, ok := value.(map[string]any); !ok {
			return []GraphProblem{configProblem(fieldPath, stepName,
				GraphCodeStepConfigInvalid, "config.extract must be an object",
				"configure extract with key/mode/keywords_true")}
		}
	case schema.KindOperations:
		if _, ok := value.([]any); !ok {
			return []GraphProblem{configTypeProblem(fieldPath, stepName, field.Key, "an array of operation objects")}
		}
	}
	return nil
}

// validateLLMCallExtract validates the optional llm_call extract object:
// allowed fields key/mode/keywords_true, required key+mode, and the
// keyword-mode keyword list contract.
func validateLLMCallExtract(path, stepName string, config map[string]any) []GraphProblem {
	raw, ok := config["extract"]
	if !ok {
		return nil
	}
	extract, isMap := raw.(map[string]any)
	if !isMap {
		return []GraphProblem{configProblem(path+".config.extract", stepName,
			GraphCodeStepConfigInvalid, "config.extract must be an object",
			"configure extract with key/mode/keywords_true")}
	}
	problems := validateConfigFields(path+".config.extract", stepName, "extract", schema.Extract(), extract)
	if configString(extract, "mode", "") == "keyword" && len(configStrings(extract, "keywords_true")) == 0 {
		problems = append(problems, configProblem(path+".config.extract.keywords_true", stepName,
			GraphCodeStepConfigInvalid, "extract mode keyword requires non-empty keywords_true",
			"list the keywords that make the extraction true"))
	}
	return problems
}

func validateTransformConfig(path, stepName string, config map[string]any) []GraphProblem {
	raw, ok := config["operations"]
	if !ok {
		return []GraphProblem{configProblem(path+".config.operations", stepName,
			GraphCodeStepConfigInvalid, "transform requires config.operations",
			"list at least one set/concat/copy operation")}
	}
	ops, ok := raw.([]any)
	if !ok {
		return []GraphProblem{configProblem(path+".config.operations", stepName,
			GraphCodeStepConfigInvalid, "config.operations must be an array of operation objects",
			"each operation is an object with an op field (set, concat, copy)")}
	}
	if len(ops) == 0 {
		return []GraphProblem{configProblem(path+".config.operations", stepName,
			GraphCodeStepConfigInvalid, "config.operations must be non-empty",
			"add at least one set/concat/copy operation")}
	}
	var problems []GraphProblem
	for i, rawOp := range ops {
		op, isMap := rawOp.(map[string]any)
		opPath := fmt.Sprintf("%s.config.operations[%d]", path, i)
		if !isMap {
			problems = append(problems, configProblem(opPath, stepName,
				GraphCodeStepConfigInvalid, "each operation must be an object",
				"an operation object has an op field (set, concat, copy)"))
			continue
		}
		opKind, _ := op["op"].(string)
		opSchema, ok := schema.OperationFor(opKind)
		if !ok {
			problems = append(problems, configProblem(opPath+".op", stepName,
				GraphCodeStepConfigInvalid,
				fmt.Sprintf("operation op must be one of set, concat, copy (got %q)", opKind),
				"use set to write a literal, concat to join keys, or copy to duplicate a state value"))
			continue
		}
		problems = append(problems, validateConfigFields(opPath, stepName, opKind, opSchema, op)...)
		if opKind == "concat" {
			if keys, err := configStringSlice(op["keys"]); err != nil || len(keys) == 0 {
				problems = append(problems, configProblem(opPath+".keys", stepName,
					GraphCodeStepConfigInvalid, "concat requires non-empty keys",
					"list the state keys to join into the target"))
			}
		}
	}
	return problems
}

func unknownFieldProblem(basePath, stepName, stepType, key string, step schema.Step) GraphProblem {
	hint := fmt.Sprintf("allowed fields: %s", strings.Join(step.AllowedKeys(), ", "))
	if key == "prompt" {
		hint = "the declarative factory field is prompt_template; use prompt_template (not prompt)"
	}
	return configProblem(basePath+"."+key, stepName,
		GraphCodeStepConfigInvalid,
		fmt.Sprintf("config.%s is not a known field for %s steps", key, stepType),
		hint)
}

func requiredFieldHint(key string) string {
	switch key {
	case "prompt_template":
		return "provide config.prompt_template; the factory field is prompt_template, not prompt"
	case "output_key":
		return "provide config.output_key; it names the state key the step writes"
	case "builtin_type":
		return "provide config.builtin_type: guard, prompt_assemble, or memory_retrieve"
	case "operations":
		return "provide config.operations with at least one set/concat/copy operation"
	case "key":
		return "provide extract.key; it produces the bool state key used by condition routing"
	case "mode":
		return "provide extract.mode: keyword (scan keywords_true) or yes_no (scan YES/true)"
	case "target":
		return "provide the target field in the operation object"
	case "value":
		return "provide the value field in the set operation object (must not be null)"
	case "source":
		return "provide the source field in the copy operation object"
	case "keys":
		return "provide the keys field in the concat operation object"
	default:
		return "provide config." + key
	}
}

func configNumber(value any) bool {
	switch value.(type) {
	case float64, int, int64:
		return true
	default:
		return false
	}
}

func configTypeProblem(fieldPath, stepName, field, want string) GraphProblem {
	return configProblem(fieldPath, stepName,
		GraphCodeStepConfigInvalid,
		fmt.Sprintf("config.%s must be %s", field, want),
		"fix the field type to match the step contract")
}

func configProblem(path, stepName, code, message, hint string) GraphProblem {
	return GraphProblem{
		Path:    path,
		Step:    stepName,
		Code:    code,
		Message: message,
		Hint:    hint,
	}
}

func stringIn(value string, options []string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

// configString and configStrings tolerate both JSON-decoded ([]any) and
// hand-built ([]string) config maps so unit tests can construct configs
// directly.
func configString(config map[string]any, key, fallback string) string {
	if value, ok := config[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func configStrings(config map[string]any, key string) []string {
	value, ok := config[key]
	if !ok {
		return nil
	}
	strings_, err := configStringSlice(value)
	if err != nil {
		return nil
	}
	return strings_
}

func configStringSlice(value any) ([]string, error) {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("element is not a string")
			}
			out = append(out, str)
		}
		return out, nil
	case []string:
		return typed, nil
	default:
		return nil, fmt.Errorf("value is not an array")
	}
}
