package teamforge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const DeclarativeWorkflowSpecSchemaVersionV1 = 1

var declarativeStableRefPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// DeclarativeWorkflowSpecV1 is the closed, platform-owned workflow document
// accepted from a planner. Worker identity is expressed only through a
// Blueprint stable_ref; database Agent IDs and versions do not exist in this
// contract.
type DeclarativeWorkflowSpecV1 struct {
	SchemaVersion  int                         `json:"schema_version"`
	EntryNodeID    string                      `json:"entry_node_id"`
	InputContract  machine.OutputContract      `json:"input_contract"`
	OutputContract machine.OutputContract      `json:"output_contract"`
	Nodes          []DeclarativeWorkflowNodeV1 `json:"nodes"`
	Edges          []DeclarativeWorkflowEdgeV1 `json:"edges"`
}

type DeclarativeWorkflowNodeV1 struct {
	ID        string                          `json:"id"`
	Type      machine.NodeType                `json:"type"`
	Label     string                          `json:"label,omitempty"`
	StableRef string                          `json:"stable_ref,omitempty"`
	Inputs    map[string]machine.InputBinding `json:"inputs,omitempty"`
	Output    *machine.OutputContract         `json:"output,omitempty"`
	Config    json.RawMessage                 `json:"config"`
}

type DeclarativeWorkflowEdgeV1 struct {
	From  string            `json:"from"`
	To    string            `json:"to"`
	Route machine.EdgeRoute `json:"route"`
}

type DeclarativeWorkerConfigV1 struct {
	Kind              machine.WorkerKind `json:"kind"`
	ResultRequirement string             `json:"result_requirement"`
}

type DeclarativeConditionCaseV1 struct {
	ToNodeID  string            `json:"to_node_id"`
	Priority  int64             `json:"priority"`
	Predicate machine.Predicate `json:"predicate"`
}

type DeclarativeConditionConfigV1 struct {
	Cases         []DeclarativeConditionCaseV1 `json:"cases"`
	DefaultNodeID string                       `json:"default_node_id"`
}

// DeclarativeWorkerBindingV1 is platform output. It is never accepted inside
// a model-authored node config.
type DeclarativeWorkerBindingV1 struct {
	StableRef    string `json:"stable_ref"`
	AgentID      string `json:"agent_id"`
	AgentVersion int64  `json:"agent_version"`
}

// DeclarativeBuildBindingV1 prevents a valid spec from being replayed under a
// different task, contract, authorization scope, or frozen baseline.
type DeclarativeBuildBindingV1 struct {
	BuildRunID   string               `json:"build_run_id"`
	BriefHash    string               `json:"brief_hash"`
	ContractHash string               `json:"contract_hash"`
	AssetScope   teambuild.AssetScope `json:"asset_scope"`
	BaselineHash string               `json:"baseline_hash"`
}

// FrozenDeclarativeWorkflowSpecV1 is the immutable compiler input embedded in
// the workflow_compile ChangeSet operation. SpecHash content-addresses every
// other field in this document.
type FrozenDeclarativeWorkflowSpecV1 struct {
	SchemaVersion   int                          `json:"schema_version"`
	SpecHash        string                       `json:"spec_hash"`
	BuildBinding    DeclarativeBuildBindingV1    `json:"build_binding"`
	WorkerBindings  []DeclarativeWorkerBindingV1 `json:"worker_bindings"`
	Spec            DeclarativeWorkflowSpecV1    `json:"spec"`
	TriggerConfig   json.RawMessage              `json:"trigger_config"`
	GraphDefinition json.RawMessage              `json:"graph_definition"`
}

type DeclarativeWorkflowProblem struct {
	Path    string `json:"path"`
	NodeID  string `json:"node_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DeclarativeWorkflowValidationError struct {
	Problems []DeclarativeWorkflowProblem `json:"problems"`
}

func (e *DeclarativeWorkflowValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "declarative_v1 workflow validation failed"
	}
	return fmt.Sprintf("declarative_v1 workflow validation failed with %d problem(s): %s: %s",
		len(e.Problems), e.Problems[0].Path, e.Problems[0].Message)
}

// DeclarativeMachineValidator is supplied by the platform tool after it has
// assembled the real team/roster/AgentVersion proof snapshot. Production uses
// the shared validator bridge, whose terminal operation is machine.Validate.
type DeclarativeMachineValidator func(
	trigger machine.TriggerConfig,
	graph machine.GraphDefinition,
) (machine.Report, error)

type CompiledDeclarativeWorkflowV1 struct {
	Trigger machine.TriggerConfig
	Graph   machine.GraphDefinition
}

// DecodeDeclarativeWorkflowSpecV1 accepts exactly one JSON object and rejects
// unknown fields at every typed level. Node config unions are decoded by
// CompileDeclarativeWorkflowSpecV1, where their discriminator is available.
func DecodeDeclarativeWorkflowSpecV1(raw json.RawMessage) (DeclarativeWorkflowSpecV1, error) {
	var spec DeclarativeWorkflowSpecV1
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return DeclarativeWorkflowSpecV1{}, fmt.Errorf("decode declarative_v1 spec: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return DeclarativeWorkflowSpecV1{}, errors.New("decode declarative_v1 spec: multiple JSON values")
		}
		return DeclarativeWorkflowSpecV1{}, fmt.Errorf("decode declarative_v1 spec: %w", err)
	}
	return spec, nil
}

// CompileDeclarativeWorkflowSpecV1 deterministically binds stable refs and
// lowers the closed spec into the machine v1 graph. It performs no I/O.
func CompileDeclarativeWorkflowSpecV1(
	spec DeclarativeWorkflowSpecV1,
	bindings []DeclarativeWorkerBindingV1,
) (CompiledDeclarativeWorkflowV1, error) {
	collector := &declarativeProblemCollector{}
	if spec.SchemaVersion != DeclarativeWorkflowSpecSchemaVersionV1 {
		collector.add("/schema_version", "", "declarative_schema_version_invalid", "schema_version must be 1")
	}
	if strings.TrimSpace(spec.EntryNodeID) == "" {
		collector.add("/entry_node_id", "", "declarative_entry_required", "entry_node_id is required")
	}
	if len(spec.Nodes) == 0 {
		collector.add("/nodes", "", "declarative_nodes_required", "at least one node is required")
	}

	bound := make(map[string]DeclarativeWorkerBindingV1, len(bindings))
	for index, binding := range bindings {
		path := fmt.Sprintf("/worker_bindings/%d", index)
		ref := strings.TrimSpace(binding.StableRef)
		if !declarativeStableRefPattern.MatchString(ref) || strings.TrimSpace(binding.AgentID) == "" || binding.AgentVersion < 1 {
			collector.add(path, "", "declarative_worker_binding_invalid", "binding requires stable_ref, agent_id, and a positive agent_version")
			continue
		}
		if _, exists := bound[ref]; exists {
			collector.add(path+"/stable_ref", "", "declarative_worker_binding_duplicate", "stable_ref binding must be unique")
			continue
		}
		binding.StableRef = ref
		binding.AgentID = strings.TrimSpace(binding.AgentID)
		bound[ref] = binding
	}

	compiled := CompiledDeclarativeWorkflowV1{
		Trigger: machine.TriggerConfig{
			SchemaVersion: machine.SchemaVersionV1,
			Type:          machine.TriggerConversationExplicit,
			Config:        machine.ConversationExplicitConfig{},
		},
		Graph: machine.GraphDefinition{
			SchemaVersion:  spec.SchemaVersion,
			EntryNodeID:    strings.TrimSpace(spec.EntryNodeID),
			InputContract:  spec.InputContract,
			OutputContract: spec.OutputContract,
			Nodes:          make([]machine.Node, 0, len(spec.Nodes)),
			Edges:          make([]machine.Edge, 0, len(spec.Edges)),
		},
	}

	usedBindings := make(map[string]bool)
	conditionConfigs := make(map[string]DeclarativeConditionConfigV1)
	for index, declarativeNode := range spec.Nodes {
		path := fmt.Sprintf("/nodes/%d", index)
		nodeID := strings.TrimSpace(declarativeNode.ID)
		config, condition, err := compileDeclarativeNodeConfig(declarativeNode, bound)
		if err != nil {
			collector.add(path+"/config", nodeID, "declarative_node_config_invalid", err.Error())
		}
		if declarativeNode.Type == machine.NodeWorker {
			ref := strings.TrimSpace(declarativeNode.StableRef)
			if !declarativeStableRefPattern.MatchString(ref) {
				collector.add(path+"/stable_ref", nodeID, "declarative_worker_stable_ref_required", "worker stable_ref is required")
			} else if _, exists := bound[ref]; !exists {
				collector.add(path+"/stable_ref", nodeID, "declarative_worker_stable_ref_unknown", "worker stable_ref is not bound to an enabled roster member")
			} else {
				usedBindings[ref] = true
			}
		} else if strings.TrimSpace(declarativeNode.StableRef) != "" {
			collector.add(path+"/stable_ref", nodeID, "declarative_stable_ref_forbidden", "stable_ref is only allowed on worker nodes")
		}
		if condition != nil {
			conditionConfigs[nodeID] = *condition
		}
		compiled.Graph.Nodes = append(compiled.Graph.Nodes, machine.Node{
			ID: nodeID, Type: declarativeNode.Type, Label: strings.TrimSpace(declarativeNode.Label),
			Inputs: declarativeNode.Inputs, Output: declarativeNode.Output, Config: config,
		})
	}
	for ref := range bound {
		if !usedBindings[ref] {
			collector.add("/worker_bindings", "", "declarative_worker_binding_unused", fmt.Sprintf("binding %q is not referenced by a worker node", ref))
		}
	}

	for index, declarativeEdge := range spec.Edges {
		edge := machine.Edge{
			ID:         fmt.Sprintf("declarative-edge-%03d", index+1),
			FromNodeID: strings.TrimSpace(declarativeEdge.From),
			ToNodeID:   strings.TrimSpace(declarativeEdge.To),
			Route:      declarativeEdge.Route,
		}
		if condition, ok := conditionConfigs[edge.FromNodeID]; ok {
			switch edge.Route {
			case machine.RouteCase:
				matches := 0
				for caseIndex := range condition.Cases {
					conditionCase := condition.Cases[caseIndex]
					if strings.TrimSpace(conditionCase.ToNodeID) == edge.ToNodeID {
						priority := conditionCase.Priority
						edge.Priority = &priority
						predicate := conditionCase.Predicate
						edge.Predicate = &predicate
						matches++
					}
				}
				if matches != 1 {
					collector.add(fmt.Sprintf("/edges/%d", index), edge.FromNodeID, "declarative_condition_case_mismatch", "case edge must match exactly one config case by to_node_id")
				}
			case machine.RouteDefault:
				if strings.TrimSpace(condition.DefaultNodeID) != edge.ToNodeID {
					collector.add(fmt.Sprintf("/edges/%d", index), edge.FromNodeID, "declarative_condition_default_mismatch", "default edge target must equal config.default_node_id")
				}
			}
		}
		compiled.Graph.Edges = append(compiled.Graph.Edges, edge)
	}
	for nodeID, condition := range conditionConfigs {
		for caseIndex, conditionCase := range condition.Cases {
			matches := 0
			for _, edge := range compiled.Graph.Edges {
				if edge.FromNodeID == nodeID && edge.ToNodeID == strings.TrimSpace(conditionCase.ToNodeID) && edge.Route == machine.RouteCase {
					matches++
				}
			}
			if matches != 1 {
				collector.add(fmt.Sprintf("/nodes/%s/config/cases/%d", nodeID, caseIndex), nodeID, "declarative_condition_case_mismatch", "each config case must have exactly one matching case edge")
			}
		}
		defaultMatches := 0
		for _, edge := range compiled.Graph.Edges {
			if edge.FromNodeID == nodeID && edge.ToNodeID == strings.TrimSpace(condition.DefaultNodeID) && edge.Route == machine.RouteDefault {
				defaultMatches++
			}
		}
		if defaultMatches != 1 {
			collector.add("/nodes/"+nodeID+"/config/default_node_id", nodeID, "declarative_condition_default_mismatch", "config.default_node_id must have exactly one matching default edge")
		}
	}
	if err := collector.err(); err != nil {
		return CompiledDeclarativeWorkflowV1{}, err
	}
	return compiled, nil
}

func compileDeclarativeNodeConfig(
	node DeclarativeWorkflowNodeV1,
	bindings map[string]DeclarativeWorkerBindingV1,
) (machine.NodeConfig, *DeclarativeConditionConfigV1, error) {
	if len(bytes.TrimSpace(node.Config)) == 0 {
		return nil, nil, errors.New("config is required")
	}
	switch node.Type {
	case machine.NodeLead:
		var config machine.LeadConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeWorker:
		var config DeclarativeWorkerConfigV1
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		binding, exists := bindings[strings.TrimSpace(node.StableRef)]
		if !exists {
			return machine.WorkerConfig{Kind: config.Kind, ResultRequirement: config.ResultRequirement}, nil, nil
		}
		return machine.WorkerConfig{
			AgentID: binding.AgentID, AgentVersion: binding.AgentVersion,
			Kind: config.Kind, ResultRequirement: strings.TrimSpace(config.ResultRequirement),
		}, nil, nil
	case machine.NodeTransform:
		var config machine.TransformConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeCondition:
		var config DeclarativeConditionConfigV1
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return machine.ConditionConfig{}, &config, nil
	case machine.NodeParallel:
		var config machine.ParallelConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeJoin:
		var config machine.JoinConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeWait:
		var config machine.WaitConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		if config.Kind != machine.WaitKindHuman {
			return nil, nil, errors.New("declarative_v1 wait requires kind=human")
		}
		return config, nil, nil
	case machine.NodeLoop:
		var config machine.LoopConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeDeliver:
		var config machine.DeliverConfig
		if err := strictRaw(node.Config, &config); err != nil {
			return nil, nil, err
		}
		return config, nil, nil
	case machine.NodeHandoff:
		return nil, nil, fmt.Errorf("node type %q is not supported by declarative_v1", node.Type)
	default:
		return nil, nil, fmt.Errorf("unknown node type %q", node.Type)
	}
}

// ResolveDeclarativeWorkerBindingsV1 binds spec stable refs through the
// Blueprint roster identities captured in the platform-owned baseline.
func ResolveDeclarativeWorkerBindingsV1(
	spec DeclarativeWorkflowSpecV1,
	blueprint teambuild.TeamBlueprintV1,
	snapshot teambuild.BaselineSnapshot,
) ([]DeclarativeWorkerBindingV1, error) {
	members := make(map[string]teambuild.BlueprintMemberV1, len(blueprint.Members))
	for _, member := range blueprint.Members {
		members[strings.TrimSpace(member.StableRef)] = member
	}
	pinsByName := make(map[string]teambuild.BaselineAgentPin, len(snapshot.AgentPins))
	for _, pin := range snapshot.AgentPins {
		pinsByName[strings.TrimSpace(pin.Name)] = pin
	}
	enabledRoster := make(map[string]bool, len(snapshot.Roster))
	for _, entry := range snapshot.Roster {
		if entry.Role == "worker" && entry.Enabled {
			enabledRoster[strings.TrimSpace(entry.AgentID)] = true
		}
	}

	requested := make(map[string]bool)
	for _, node := range spec.Nodes {
		if node.Type == machine.NodeWorker {
			requested[strings.TrimSpace(node.StableRef)] = true
		}
	}
	refs := make([]string, 0, len(requested))
	for ref := range requested {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	bindings := make([]DeclarativeWorkerBindingV1, 0, len(refs))
	for _, ref := range refs {
		member, exists := members[ref]
		if !exists || member.Role != teambuild.BlueprintMemberRoleWorker {
			return nil, fmt.Errorf("worker stable_ref %q does not identify a Blueprint roster worker", ref)
		}
		pin, exists := pinsByName[strings.TrimSpace(member.Name)]
		if !exists || pin.AgentID == "" || pin.Version < 1 || !enabledRoster[pin.AgentID] {
			return nil, fmt.Errorf("worker stable_ref %q has no enabled, exact roster AgentVersion binding", ref)
		}
		bindings = append(bindings, DeclarativeWorkerBindingV1{
			StableRef: ref, AgentID: pin.AgentID, AgentVersion: int64(pin.Version),
		})
	}
	return bindings, nil
}

// ResolveCreateDeclarativeWorkerBindingsV1 derives the exact first versions
// that the create-mode ChangeSet will materialize. Agent identity and version
// remain platform output: the planner supplies only stable_ref values, while
// the frozen Blueprint supplies the validated, namespace-scoped Agent names.
func ResolveCreateDeclarativeWorkerBindingsV1(
	spec DeclarativeWorkflowSpecV1,
	blueprint teambuild.TeamBlueprintV1,
) ([]DeclarativeWorkerBindingV1, error) {
	if blueprint.Mode != teambuild.ModeCreate {
		return nil, errors.New("create declarative bindings require a create-mode Blueprint")
	}
	members := make(map[string]teambuild.BlueprintMemberV1, len(blueprint.Members))
	for _, member := range blueprint.Members {
		members[strings.TrimSpace(member.StableRef)] = member
	}
	requested := make(map[string]bool)
	for _, node := range spec.Nodes {
		if node.Type == machine.NodeWorker {
			requested[strings.TrimSpace(node.StableRef)] = true
		}
	}
	refs := make([]string, 0, len(requested))
	for ref := range requested {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	bindings := make([]DeclarativeWorkerBindingV1, 0, len(refs))
	for _, ref := range refs {
		member, exists := members[ref]
		if !exists || member.Role != teambuild.BlueprintMemberRoleWorker {
			return nil, fmt.Errorf("worker stable_ref %q does not identify a Blueprint roster worker", ref)
		}
		if member.ManagementMode != teambuild.BlueprintManagementManaged || strings.TrimSpace(member.Name) == "" {
			return nil, fmt.Errorf("create worker stable_ref %q has no managed Agent target", ref)
		}
		bindings = append(bindings, DeclarativeWorkerBindingV1{
			StableRef: ref, AgentID: strings.TrimSpace(member.Name), AgentVersion: 1,
		})
	}
	return bindings, nil
}

// FreezeDeclarativeWorkflowSpecV1 runs the caller-supplied shared machine
// validator and content-addresses the complete, BuildRun-bound compiler input.
func FreezeDeclarativeWorkflowSpecV1(
	spec DeclarativeWorkflowSpecV1,
	bindings []DeclarativeWorkerBindingV1,
	buildBinding DeclarativeBuildBindingV1,
	validator DeclarativeMachineValidator,
) (FrozenDeclarativeWorkflowSpecV1, error) {
	if strings.TrimSpace(buildBinding.BuildRunID) == "" ||
		!isSHA256(strings.TrimSpace(buildBinding.BriefHash)) ||
		!isSHA256(strings.TrimSpace(buildBinding.ContractHash)) ||
		!isSHA256(strings.TrimSpace(buildBinding.BaselineHash)) {
		return FrozenDeclarativeWorkflowSpecV1{}, errors.New("declarative build binding requires build_run_id and canonical brief/contract/baseline hashes")
	}
	if validator == nil {
		return FrozenDeclarativeWorkflowSpecV1{}, errors.New("declarative machine validator is required")
	}
	compiled, err := CompileDeclarativeWorkflowSpecV1(spec, bindings)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	report, err := validator(compiled.Trigger, compiled.Graph)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	if len(report.Issues) != 0 {
		problems := make([]DeclarativeWorkflowProblem, 0, len(report.Issues))
		for _, issue := range report.Issues {
			problems = append(problems, DeclarativeWorkflowProblem{
				Path: issue.Path, NodeID: issue.NodeID, Code: issue.Code, Message: issue.Message,
			})
		}
		return FrozenDeclarativeWorkflowSpecV1{}, &DeclarativeWorkflowValidationError{Problems: problems}
	}
	triggerJSON, graphJSON, err := encodeWorkflowDraftJSON(compiled.Trigger, compiled.Graph)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("encode declarative_v1 machine graph: %w", err)
	}
	sortedBindings := append([]DeclarativeWorkerBindingV1(nil), bindings...)
	sort.Slice(sortedBindings, func(i, j int) bool { return sortedBindings[i].StableRef < sortedBindings[j].StableRef })
	frozen := FrozenDeclarativeWorkflowSpecV1{
		SchemaVersion: DeclarativeWorkflowSpecSchemaVersionV1,
		BuildBinding:  buildBinding, WorkerBindings: sortedBindings, Spec: spec,
		TriggerConfig: triggerJSON, GraphDefinition: graphJSON,
	}
	hash, err := frozenDeclarativeSpecHashV1(frozen)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	frozen.SpecHash = hash
	return frozen, nil
}

func validateFrozenDeclarativeWorkflowSpecV1(frozen FrozenDeclarativeWorkflowSpecV1) error {
	if frozen.SchemaVersion != DeclarativeWorkflowSpecSchemaVersionV1 || !isSHA256(frozen.SpecHash) {
		return errors.New("frozen declarative_v1 spec header is invalid")
	}
	wantHash, err := frozenDeclarativeSpecHashV1(frozen)
	if err != nil || wantHash != frozen.SpecHash {
		return errors.New("frozen declarative_v1 spec hash mismatch")
	}
	compiled, err := CompileDeclarativeWorkflowSpecV1(frozen.Spec, frozen.WorkerBindings)
	if err != nil {
		return fmt.Errorf("recompile frozen declarative_v1 spec: %w", err)
	}
	triggerJSON, graphJSON, err := encodeWorkflowDraftJSON(compiled.Trigger, compiled.Graph)
	if err != nil {
		return err
	}
	triggerMatches, err := semanticEqual(triggerJSON, frozen.TriggerConfig)
	if err != nil {
		return fmt.Errorf("compare frozen declarative_v1 trigger_config: %w", err)
	}
	if !triggerMatches {
		return errors.New("frozen declarative_v1 trigger_config does not match its spec and bindings")
	}
	graphMatches, err := semanticEqual(graphJSON, frozen.GraphDefinition)
	if err != nil {
		return fmt.Errorf("compare frozen declarative_v1 graph_definition: %w", err)
	}
	if !graphMatches {
		return errors.New("frozen declarative_v1 graph_definition does not match its spec and bindings")
	}
	return nil
}

// ValidateFrozenDeclarativeWorkflowSpecV1 revalidates one persisted compiler
// input at the execution boundary. Planning and storage validate the same
// document before persistence; the executor calls this exported seam so it
// never materializes a graph from an unchecked frozen envelope.
func ValidateFrozenDeclarativeWorkflowSpecV1(frozen FrozenDeclarativeWorkflowSpecV1) error {
	return validateFrozenDeclarativeWorkflowSpecV1(frozen)
}

// RebindFrozenDeclarativeWorkerBindingsV1 replaces only the platform-owned
// AgentVersion identities in an already validated frozen spec. The stable-ref
// set and every other planning field remain immutable; the returned spec is
// recompiled and content-addressed from the replacement bindings.
func RebindFrozenDeclarativeWorkerBindingsV1(
	frozen FrozenDeclarativeWorkflowSpecV1,
	bindings []DeclarativeWorkerBindingV1,
) (FrozenDeclarativeWorkflowSpecV1, error) {
	if err := validateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	if len(bindings) != len(frozen.WorkerBindings) {
		return FrozenDeclarativeWorkflowSpecV1{}, errors.New("declarative worker binding set changed during materialization")
	}
	wantRefs := make(map[string]bool, len(frozen.WorkerBindings))
	for _, binding := range frozen.WorkerBindings {
		wantRefs[strings.TrimSpace(binding.StableRef)] = true
	}
	rebound := append([]DeclarativeWorkerBindingV1(nil), bindings...)
	for _, binding := range rebound {
		ref := strings.TrimSpace(binding.StableRef)
		if !wantRefs[ref] {
			return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("declarative worker stable_ref %q changed during materialization", ref)
		}
		delete(wantRefs, ref)
	}
	if len(wantRefs) != 0 {
		return FrozenDeclarativeWorkflowSpecV1{}, errors.New("declarative worker binding set changed during materialization")
	}
	sort.Slice(rebound, func(i, j int) bool { return rebound[i].StableRef < rebound[j].StableRef })
	compiled, err := CompileDeclarativeWorkflowSpecV1(frozen.Spec, rebound)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("rebind declarative worker identities: %w", err)
	}
	frozen.WorkerBindings = rebound
	frozen.TriggerConfig, frozen.GraphDefinition, err = encodeWorkflowDraftJSON(compiled.Trigger, compiled.Graph)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("encode rebound declarative workflow: %w", err)
	}
	frozen.SpecHash, err = frozenDeclarativeSpecHashV1(frozen)
	if err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, fmt.Errorf("hash rebound declarative workflow: %w", err)
	}
	if err := validateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		return FrozenDeclarativeWorkflowSpecV1{}, err
	}
	return frozen, nil
}

func frozenDeclarativeSpecHashV1(frozen FrozenDeclarativeWorkflowSpecV1) (string, error) {
	identity := frozen
	identity.SpecHash = ""
	return hashCanonical(identity)
}

type declarativeProblemCollector struct {
	problems []DeclarativeWorkflowProblem
}

func (c *declarativeProblemCollector) add(path, nodeID, code, message string) {
	c.problems = append(c.problems, DeclarativeWorkflowProblem{
		Path: path, NodeID: nodeID, Code: code, Message: message,
	})
}

func (c *declarativeProblemCollector) err() error {
	if len(c.problems) == 0 {
		return nil
	}
	sort.SliceStable(c.problems, func(i, j int) bool {
		if c.problems[i].Path != c.problems[j].Path {
			return c.problems[i].Path < c.problems[j].Path
		}
		if c.problems[i].Code != c.problems[j].Code {
			return c.problems[i].Code < c.problems[j].Code
		}
		return c.problems[i].NodeID < c.problems[j].NodeID
	})
	return &DeclarativeWorkflowValidationError{Problems: append([]DeclarativeWorkflowProblem(nil), c.problems...)}
}

func declarativeFrozenEquivalent(left, right FrozenDeclarativeWorkflowSpecV1) bool {
	leftHash, leftErr := hashCanonical(left)
	rightHash, rightErr := hashCanonical(right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}
