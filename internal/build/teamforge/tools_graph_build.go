package teamforge

// tf_graph_build — the one-call employee internal-graph build tool (ticket
// T18). The model submits the complete graph structure once: a steps array
// whose config fields are enumerated per step type by the tool's JSON
// Schema (chat / llm_call / llm_check / transform / builtin / yield), an
// entry step, and an output contract. The tool then runs
// begin → apply → validate → commit atomically: any failure leaves no
// version in the database and no draft in dispatcher memory, and errors are
// located per step with the correct field names. The submitted steps array
// IS the complete graph — an existing graph is replaced wholesale, so the
// call is idempotent and never needs incremental state. Results carry
// non-blocking D9 (output-contract reachability) and D10 (transform set op
// literal shape) warnings.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/declarative/schema"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// ToolGraphBuild submits a complete employee internal graph in one call.
const ToolGraphBuild = "tf_graph_build"

// Failure phases of tf_graph_build inside the structured error JSON.
const (
	graphBuildPhaseSchema   = "schema"
	graphBuildPhaseApply    = "apply"
	graphBuildPhaseValidate = "validate"
)

// graphBuildStepInput is one declared step: type + per-type config plus the
// wiring intent (next XOR condition). Wiring stays intent-only: the tools
// own the raw Next/Condition fields exactly like tf_graph_apply.
type graphBuildStepInput struct {
	Name      string                    `json:"name"`
	Type      string                    `json:"type"`
	Display   string                    `json:"display,omitempty"`
	Config    json.RawMessage           `json:"config"`
	Next      *string                   `json:"next,omitempty"`
	Condition *graphBuildConditionInput `json:"condition,omitempty"`
}

type graphBuildConditionInput struct {
	Key   string `json:"key"`
	True  string `json:"true"`
	False string `json:"false"`
}

type graphBuildInput struct {
	BuildRunID     string                `json:"build_run_id"`
	Agent          string                `json:"agent"`
	Steps          []graphBuildStepInput `json:"steps"`
	Entry          string                `json:"entry"`
	OutputContract json.RawMessage       `json:"output_contract,omitempty"`
}

type graphBuildResultJSON struct {
	BuildRunID     string           `json:"build_run_id"`
	Agent          string           `json:"agent"`
	Tool           string           `json:"tool"`
	Committed      bool             `json:"committed"`
	Version        int              `json:"version"`
	GraphType      string           `json:"graph_type"`
	StepCount      int              `json:"step_count"`
	Entry          string           `json:"entry"`
	Warnings       []GraphProblem   `json:"warnings"`
	OutputContract *json.RawMessage `json:"output_contract,omitempty"`
}

type graphBuildFailureJSON struct {
	BuildRunID string         `json:"build_run_id"`
	Agent      string         `json:"agent"`
	Tool       string         `json:"tool"`
	Committed  bool           `json:"committed"`
	Phase      string         `json:"phase"`
	Errors     []GraphProblem `json:"errors"`
	Warnings   []GraphProblem `json:"warnings"`
}

// --- tf_graph_build handler ---

func (d *GraphWriteToolsDispatcher) graphBuild(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, problems, warnings := parseGraphBuildInput(call.Args)
	if len(problems) > 0 {
		return toolError(call.ID, graphBuildFailureContent(input, graphBuildPhaseSchema, problems, warnings)), nil
	}
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}
	input.Agent = record.Name

	// begin: the submitted steps are the complete graph. The macro never
	// seeds from the existing record (a full rebuild replaces it) and never
	// touches the in-memory draft table, so a failure leaves nothing behind.
	draft := &graphDraft{
		BuildRunID: input.BuildRunID,
		AgentName:  record.Name,
		Graph:      registry.GraphDefinition{},
	}
	if len(input.OutputContract) > 0 {
		draft.OutputContract = append(json.RawMessage(nil), input.OutputContract...)
	}

	// apply (full): all steps first so wiring can reference forward, then
	// wiring/entry/contract. Every op runs on the local draft; a failure
	// aborts before validation or any database write.
	ops := graphBuildOps(&input)
	for i := range ops {
		if err := applyGraphOp(draft, &ops[i]); err != nil {
			problem := GraphProblem{
				Path:    graphBuildOpPath(&ops[i]),
				Message: err.Error(),
				Hint:    "按该字段的 schema 修复后重新 tf_graph_build",
			}
			if ops[i].Name != "" {
				problem.Step = ops[i].Name
			}
			return toolError(call.ID, graphBuildFailureContent(
				input, graphBuildPhaseApply, []GraphProblem{problem}, nil)), nil
		}
	}

	validation := validateEmployeeGraph(&draft.Graph)
	if !validation.Valid {
		failureWarnings := append([]GraphProblem(nil), warnings...)
		failureWarnings = append(failureWarnings, validation.Warnings...)
		failureWarnings = append(failureWarnings, collectGraphBuildWarnings(draft)...)
		return toolError(call.ID, graphBuildFailureContent(
			input, graphBuildPhaseValidate, validation.Errors, failureWarnings)), nil
	}
	warnings = append(warnings, validation.Warnings...)
	warnings = append(warnings, collectGraphBuildWarnings(draft)...)

	// commit: the exact tf_graph_commit write path (F2 model resolution +
	// F13 engine gate inside one write tx).
	result := d.commitGraphDraft(ctx, call, &graphCallInput{
		BuildRunID: input.BuildRunID,
		Agent:      record.Name,
	}, record, draft)
	if result.IsError {
		return result, nil
	}
	var committed graphCommitJSON
	if err := json.Unmarshal([]byte(result.Content), &committed); err != nil {
		return toolError(call.ID, "tf_graph_build commit result decode failed: "+err.Error()), nil
	}
	buildResult, _ := toolJSON(call.ID, graphBuildResultJSON{
		BuildRunID:     input.BuildRunID,
		Agent:          record.Name,
		Tool:           ToolGraphBuild,
		Committed:      true,
		Version:        committed.Version,
		GraphType:      committed.GraphType,
		StepCount:      len(draft.Graph.Steps),
		Entry:          draft.Graph.Entry,
		Warnings:       warnings,
		OutputContract: committed.OutputContract,
	})
	return buildResult, nil
}

// graphBuildOps converts one complete graph build input into the tf_graph
// apply op vocabulary. Step addition precedes wiring so forward references
// resolve; the returned ops preserve input order within each group.
func graphBuildOps(input *graphBuildInput) []graphApplyOp {
	var ops []graphApplyOp
	seq := 0
	nextSeq := func() int {
		seq++
		return seq
	}
	for i := range input.Steps {
		step := &input.Steps[i]
		var config map[string]any
		if len(step.Config) > 0 {
			_ = json.Unmarshal(step.Config, &config)
		}
		ops = append(ops, graphApplyOp{
			Seq: nextSeq(), Op: graphOpAddStep, Name: step.Name,
			Type: step.Type, Display: step.Display, Config: config,
		})
	}
	for i := range input.Steps {
		step := &input.Steps[i]
		if step.Next != nil {
			next := *step.Next
			ops = append(ops, graphApplyOp{
				Seq: nextSeq(), Op: graphOpConnectNext, Name: step.Name, Next: next,
			})
		}
		if step.Condition != nil {
			ops = append(ops, graphApplyOp{
				Seq: nextSeq(), Op: graphOpConnectCondition, Name: step.Name,
				Key: step.Condition.Key, True: step.Condition.True, False: step.Condition.False,
			})
		}
	}
	ops = append(ops, graphApplyOp{Seq: nextSeq(), Op: graphOpSetEntry, Entry: input.Entry})
	if len(input.OutputContract) > 0 {
		ops = append(ops, graphApplyOp{
			Seq: nextSeq(), Op: graphOpSetOutputContract, OutputContract: input.OutputContract,
		})
	}
	return ops
}

// graphBuildOpPath locates one failing op for the structured error payload.
func graphBuildOpPath(op *graphApplyOp) string {
	switch op.Op {
	case graphOpAddStep, graphOpUpdateStepConfig, graphOpRemoveStep,
		graphOpConnectNext, graphOpConnectCondition:
		if op.Name != "" {
			return "steps[" + op.Name + "]"
		}
	}
	return fmt.Sprintf("ops[%d]", op.Seq)
}

// --- parameter validation (schema layer) ---

// parseGraphBuildInput strictly parses the tool arguments and validates
// every step against its per-type config field set before any state is
// touched. Unknown fields are rejected here with the allowed field list —
// the D2/D7 54-try failure mode is pushed to the parameter layer.
func parseGraphBuildInput(raw string) (graphBuildInput, []GraphProblem, []GraphProblem) {
	var input graphBuildInput
	if err := strictRaw(json.RawMessage(raw), &input); err != nil {
		return input, []GraphProblem{{
			Message: "invalid input: " + err.Error(),
			Hint:    "按 tf_graph_build 的 JSON Schema 提交参数",
		}}, nil
	}
	var problems, warnings []GraphProblem
	input.Agent = strings.TrimSpace(input.Agent)
	input.Entry = strings.TrimSpace(input.Entry)
	if input.Agent == "" {
		problems = append(problems, GraphProblem{Path: "agent", Message: "agent is required"})
	}
	if len(input.Steps) == 0 {
		problems = append(problems, GraphProblem{Path: "steps", Message: "steps is required and must be non-empty"})
	}
	if input.Entry == "" {
		problems = append(problems, GraphProblem{Path: "entry", Message: "entry is required"})
	}
	for i := range input.Steps {
		stepProblems, stepWarnings := validateGraphBuildStep(i, &input.Steps[i])
		problems = append(problems, stepProblems...)
		warnings = append(warnings, stepWarnings...)
	}
	return input, problems, warnings
}

func validateGraphBuildStep(index int, step *graphBuildStepInput) ([]GraphProblem, []GraphProblem) {
	basePath := fmt.Sprintf("steps[%d]", index)
	stepName := strings.TrimSpace(step.Name)
	stepType := strings.TrimSpace(step.Type)
	var problems, warnings []GraphProblem
	if stepName == "" {
		problems = append(problems, GraphProblem{Path: basePath + ".name", Message: "step name is required"})
	}
	if stepType == "" {
		problems = append(problems, GraphProblem{
			Path: basePath + ".type", Message: "step type is required",
			Hint: "allowed: chat, llm_call, llm_check, transform, builtin, yield",
		})
	} else if !validEmployeeStepType(stepType) {
		problems = append(problems, GraphProblem{
			Path: basePath + ".type", Message: fmt.Sprintf("invalid step type %q", stepType),
			Hint: "allowed: chat, llm_call, llm_check, transform, builtin, yield",
		})
	}
	if step.Next != nil && step.Condition != nil {
		problems = append(problems, GraphProblem{
			Path: basePath, Message: "next and condition are mutually exclusive; declare one wiring intent per step",
		})
	}
	if step.Condition != nil {
		if strings.TrimSpace(step.Condition.Key) == "" {
			problems = append(problems, GraphProblem{Path: basePath + ".condition.key", Message: "condition.key is required"})
		}
		if strings.TrimSpace(step.Condition.True) == "" {
			problems = append(problems, GraphProblem{Path: basePath + ".condition.true", Message: "condition.true is required"})
		}
		if strings.TrimSpace(step.Condition.False) == "" {
			problems = append(problems, GraphProblem{Path: basePath + ".condition.false", Message: "condition.false is required"})
		}
	}
	if len(step.Config) == 0 {
		problems = append(problems, GraphProblem{
			Path: basePath + ".config", Message: "config is required (may be an empty object)",
		})
	} else if stepType != "" {
		configProblems, configWarnings := validateGraphStepConfig(basePath+".config", stepName, stepType, step.Config)
		problems = append(problems, configProblems...)
		warnings = append(warnings, configWarnings...)
	}
	return problems, warnings
}

// validateGraphStepConfig validates one step's config object against the
// shared declarative schema (internal/declarative/schema): unknown fields
// are rejected with the allowed list, required fields and per-kind types
// are checked, and the nested extract/operations shapes are validated.
func validateGraphStepConfig(basePath, stepName, stepType string, raw json.RawMessage) ([]GraphProblem, []GraphProblem) {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return []GraphProblem{{Path: basePath, Step: stepName, Message: "config must be a JSON object"}}, nil
	}
	step, ok := schema.StepFor(stepType)
	if !ok {
		return nil, nil
	}
	problems, warnings := validateGraphConfigFields(basePath, stepName, step, fields)
	if stepType == "llm_call" {
		if rawExtract, ok := fields["extract"]; ok {
			extractFields, err := decodeRawFields(rawExtract)
			if err != nil {
				problems = append(problems, GraphProblem{
					Path: basePath + ".extract", Step: stepName, Message: "config.extract must be an object",
				})
			} else {
				extractProblems, _ := validateGraphConfigFields(basePath+".extract", stepName, schema.Extract(), extractFields)
				problems = append(problems, extractProblems...)
			}
		}
	}
	if stepType == "transform" {
		if rawOps, ok := fields["operations"]; ok {
			opProblems, opWarnings := validateGraphOperations(basePath+".operations", stepName, rawOps)
			problems = append(problems, opProblems...)
			warnings = append(warnings, opWarnings...)
		}
	}
	return problems, warnings
}

// validateGraphConfigFields checks one config container (step config,
// llm_call extract, or a transform operation) against its schema.Step.
func validateGraphConfigFields(
	basePath, stepName string,
	step schema.Step,
	fields map[string]json.RawMessage,
) ([]GraphProblem, []GraphProblem) {
	var problems []GraphProblem
	allowed := step.AllowedKeys()
	for key := range fields {
		if step.HasField(key) {
			continue
		}
		problems = append(problems, GraphProblem{
			Path: basePath + "." + key, Step: stepName,
			Message: fmt.Sprintf("unknown field %q", key),
			Hint:    fmt.Sprintf("%s config allows: %s", step.Type, strings.Join(allowed, ", ")),
		})
	}
	for _, field := range step.Fields {
		value, ok := fields[field.Key]
		if !ok {
			if field.Required {
				problems = append(problems, GraphProblem{
					Path: basePath + "." + field.Key, Step: stepName,
					Message: fmt.Sprintf("config.%s is required", field.Key),
				})
			}
			continue
		}
		if message := checkGraphConfigValueKind(field, value); message != "" {
			problems = append(problems, GraphProblem{
				Path: basePath + "." + field.Key, Step: stepName,
				Message: fmt.Sprintf("config.%s %s", field.Key, message),
			})
		}
	}
	return problems, nil
}

func checkGraphConfigValueKind(field schema.Field, raw json.RawMessage) string {
	switch field.Kind {
	case schema.KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a string"
		}
	case schema.KindNumber:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != math.Trunc(n) {
			return "must be an integer"
		}
	case schema.KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "must be a boolean"
		}
	case schema.KindStringArray:
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			return "must be an array of strings"
		}
	case schema.KindEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !stringInSlice(s, field.Enum) {
			return fmt.Sprintf("must be one of %s", strings.Join(field.Enum, ", "))
		}
	case schema.KindExtract:
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			return "must be an object"
		}
	case schema.KindOperations:
		var probe []json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			return "must be an array of operation objects"
		}
	}
	return ""
}

// validateGraphOperations validates the transform operations array and
// collects the D10 warning: a set op value shaped like {path, source} is a
// literal, never a reference.
func validateGraphOperations(basePath, stepName string, raw json.RawMessage) ([]GraphProblem, []GraphProblem) {
	var rawOps []json.RawMessage
	if err := json.Unmarshal(raw, &rawOps); err != nil || rawOps == nil {
		return []GraphProblem{{
			Path: basePath, Step: stepName, Message: "operations must be an array of operation objects",
		}}, nil
	}
	if len(rawOps) == 0 {
		return []GraphProblem{{
			Path: basePath, Step: stepName, Message: "operations must be non-empty",
		}}, nil
	}
	var problems, warnings []GraphProblem
	for i, rawOp := range rawOps {
		opPath := fmt.Sprintf("%s[%d]", basePath, i)
		opFields, err := decodeRawFields(rawOp)
		if err != nil {
			problems = append(problems, GraphProblem{Path: opPath, Step: stepName, Message: "operation must be an object"})
			continue
		}
		opKind := ""
		_ = json.Unmarshal(opFields["op"], &opKind)
		opSchema, ok := schema.OperationFor(opKind)
		if !ok {
			problems = append(problems, GraphProblem{
				Path: opPath + ".op", Step: stepName,
				Message: fmt.Sprintf("operation op must be one of set, concat, copy (got %q)", opKind),
				Hint:    "set 写字面量，concat 拼接状态键，copy 复制状态值",
			})
			continue
		}
		opProblems, _ := validateGraphConfigFields(opPath, stepName, opSchema, opFields)
		problems = append(problems, opProblems...)
		if opKind == "set" {
			if rawValue, ok := opFields["value"]; ok {
				var value map[string]json.RawMessage
				if err := json.Unmarshal(rawValue, &value); err == nil && value != nil {
					_, hasPath := value["path"]
					_, hasSource := value["source"]
					if hasPath && hasSource {
						warnings = append(warnings, GraphProblem{
							Path: opPath + ".value", Step: stepName,
							Code:    "graph_warning_transform_literal_object",
							Message: "transform set op 的 value 是 {path, source} 形状，这会被当作字面量 JSON 对象写入状态，不会解析为引用",
							Hint:    "若想引用上游状态值，用 copy op（source=状态键名）；set op 的 value 永远是字面量",
						})
					}
				}
			}
		}
	}
	return problems, warnings
}

// --- D9/D10 result warnings ---

// collectGraphBuildWarnings derives the non-blocking build warnings from the
// assembled draft: an output contract that is a strict object schema usually
// cannot be produced by a ToolLoop employee's free-text output (D9).
func collectGraphBuildWarnings(draft *graphDraft) []GraphProblem {
	var warnings []GraphProblem
	if isStrictObjectSchema(draft.OutputContract) {
		warnings = append(warnings, GraphProblem{
			Path:    "output_contract",
			Code:    "graph_warning_employee_object_unreachable",
			Message: "output_contract 是 object schema：员工自由文本输出通常不可达（ToolLoop 很难稳定产出符合严格 object schema 的 JSON）",
			Hint:    "建议 json 无约束或 text（output_contract 用 {\"type\":\"object\"} 等宽松 JSON 或 text）；若必须约束结构，给员工配置声明式内部图并让图直接产出该结构",
		})
	}
	return warnings
}

func isStrictObjectSchema(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	if rawType, ok := obj["type"]; ok {
		var schemaType string
		if err := json.Unmarshal(rawType, &schemaType); err == nil && schemaType == "object" {
			return true
		}
	}
	for _, key := range []string{"properties", "required", "additionalProperties", "$schema"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

func graphBuildFailureContent(
	input graphBuildInput,
	phase string,
	problems, warnings []GraphProblem,
) string {
	payload := graphBuildFailureJSON{
		BuildRunID: input.BuildRunID,
		Agent:      input.Agent,
		Tool:       ToolGraphBuild,
		Committed:  false,
		Phase:      phase,
		Errors:     problems,
		Warnings:   warnings,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("%s failed (%s): %v", ToolGraphBuild, phase, err)
	}
	return string(data)
}

// --- tool input schema (generated from the declarative schema facts) ---

// T22 (DeepSeek schema compat): the wire InputSchema must stay inside
// DeepSeek's function-calling JSON Schema subset — no oneOf/anyOf/$ref and
// no other out-of-subset constraint keywords (const, minItems). The step
// config is a flat object whose property set is the union of every step
// type's fields, all optional at the schema layer; the type discriminator
// keeps its enum, every field description names the owning types, and the
// per-type required/mutex/enum rules run in the server-side validation
// (parse layer + teameval validator) with field-level errors.

var graphBuildInputSchema = mustSchemaJSON("tf_graph_build", graphBuildInputSchemaDoc())

// graphStepTypeOrder is the stable enumeration order of employee
// internal-graph step types (mirrors schema.StepFor).
var graphStepTypeOrder = []string{"chat", "llm_call", "llm_check", "transform", "builtin", "yield"}

func graphBuildInputSchemaDoc() map[string]any {
	configProperties := make(map[string]any, 16)
	configTypeNames := make([]string, 0, len(graphStepTypeOrder))
	for _, stepType := range graphStepTypeOrder {
		configTypeNames = append(configTypeNames, stepType+" config")
	}
	union, fieldTypes := graphStepConfigUnion()
	for _, field := range union {
		doc := graphStepFieldSchemaDoc(field)
		doc["description"] = "仅 " + strings.Join(fieldTypes[field.Key], "/") + "；" +
			graphConfigFieldDescriptions[field.Key]
		configProperties[field.Key] = doc
	}
	configDoc := schemaObject(configProperties, nil)
	configDoc["description"] = "Step config：字段集为 " + strings.Join(configTypeNames, " / ") +
		" 的并集，schema 层均非必填；类型专属必填/互斥/枚举由服务端校验，错误保持字段级定位"
	stepsItem := schemaObject(map[string]any{
		"name": map[string]any{"type": "string", "description": "Step name; 图内唯一"},
		"type": map[string]any{
			"type": "string", "enum": graphStepTypeOrder,
			"description": "Step type；config 字段为各类型并集（未知字段在 schema 层即拒）；类型专属字段适用性由服务端校验",
		},
		"display": map[string]any{"type": "string", "description": "可选显示标签"},
		"config":  configDoc,
		"next": map[string]any{
			"type":        "string",
			"description": "接线意图：下一步 step 名；空串显式终止图。与 condition 互斥（服务端校验）",
		},
		"condition": schemaObject(map[string]any{
			"key":   map[string]any{"type": "string", "description": "llm_check output_key 或 llm_call extract.key 产出的布尔状态键"},
			"true":  map[string]any{"type": "string", "description": "true 分支 step 名（必须非空）"},
			"false": map[string]any{"type": "string", "description": "false 分支 step 名（必须非空）"},
		}, []string{"key", "true", "false"}),
	}, []string{"name", "type", "config"})
	return schemaObject(map[string]any{
		"build_run_id": buildRunIDSchema("可选；省略时自动绑定当前授权 run"),
		"agent": map[string]any{
			"type": "string", "description": "目标员工 Agent；必须已存在且在本次建设任务授权范围内",
		},
		"steps": map[string]any{
			"type": "array", "items": stepsItem,
			"description": "完整图结构：提交后整体替换目标 Agent 的既有内部图；非空由服务端校验",
		},
		"entry": map[string]any{"type": "string", "description": "入口 step 名；必须存在于 steps"},
		"output_contract": map[string]any{
			"type": "object",
			"description": "员工输出合同（ToolLoop OutputSchema JSON Schema）。员工自由文本输出通常无法匹配严格 object schema，" +
				"建议宽松 JSON 或 text；object schema 会触发可达性 warning（不阻断）",
		},
	}, []string{"agent", "steps", "entry"})
}

// graphStepConfigUnion merges the per-type step config field sets into one
// flat union. Every union member is non-required at the schema layer; the
// owning step types are recorded so the model-facing description can point
// at the right type. Per-type required/unknown-field/cross-field rules stay
// in the server-side validation (parse layer + teameval validator).
func graphStepConfigUnion() ([]schema.Field, map[string][]string) {
	var union []schema.Field
	fieldTypes := make(map[string][]string)
	seen := make(map[string]bool)
	for _, stepType := range graphStepTypeOrder {
		step, ok := schema.StepFor(stepType)
		if !ok {
			continue
		}
		for _, field := range step.Fields {
			fieldTypes[field.Key] = append(fieldTypes[field.Key], stepType)
			if seen[field.Key] {
				continue
			}
			seen[field.Key] = true
			union = append(union, schema.Field{Key: field.Key, Kind: field.Kind, Enum: field.Enum})
		}
	}
	return union, fieldTypes
}

func graphStepFieldSchemaDoc(field schema.Field) map[string]any {
	description := graphConfigFieldDescriptions[field.Key]
	doc := map[string]any{"description": description}
	switch field.Kind {
	case schema.KindString:
		doc["type"] = "string"
	case schema.KindNumber:
		doc["type"] = "integer"
	case schema.KindBool:
		doc["type"] = "boolean"
	case schema.KindStringArray:
		doc["type"] = "array"
		doc["items"] = map[string]any{"type": "string"}
	case schema.KindEnum:
		doc["type"] = "string"
		doc["enum"] = field.Enum
	case schema.KindAny:
		// any JSON literal
	case schema.KindExtract:
		// Flat nested object (no oneOf/anyOf): key/mode/keywords_true are
		// all optional at the schema layer; required key+mode and the
		// keyword-mode contract are enforced by the server-side validation.
		extract := schema.Extract()
		properties := make(map[string]any, len(extract.Fields))
		for _, extractField := range extract.Fields {
			properties[extractField.Key] = graphStepFieldSchemaDoc(extractField)
		}
		extractDoc := schemaObject(properties, nil)
		extractDoc["description"] = "仅 llm_call；可选布尔抽取（condition 路由用）：{key, mode, keywords_true}，key/mode 必填由服务端校验"
		doc = extractDoc
	case schema.KindOperations:
		doc["type"] = "array"
		doc["items"] = graphOperationsSchemaDoc()
	}
	return doc
}

// graphOperationsSchemaDoc renders the transform operations array item as a
// flat union of the set/concat/copy operation fields (no oneOf/anyOf): op
// keeps its enum, every other field is optional at the schema layer, and the
// per-op required/unknown-field rules run in the server-side validation.
func graphOperationsSchemaDoc() map[string]any {
	properties := make(map[string]any, 6)
	union, fieldTypes := graphOperationUnion()
	for _, field := range union {
		doc := graphStepFieldSchemaDoc(field)
		switch field.Key {
		case "op":
			doc["enum"] = []string{"set", "concat", "copy"}
			doc["description"] = "操作类型：set 写字面量；concat 拼接状态键；copy 复制状态值；类型专属必填/互斥由服务端校验"
		case "value":
			doc["description"] = "任意 JSON 字面量；set 的 value 总是按字面量写入状态，不会解析引用"
		default:
			doc["description"] = "仅 " + strings.Join(fieldTypes[field.Key], "/") + "；" +
				graphConfigFieldDescriptions[field.Key]
		}
		properties[field.Key] = doc
	}
	doc := schemaObject(properties, nil)
	doc["description"] = "transform 操作对象：set / concat / copy 字段并集；op 外的类型专属必填/互斥/未知字段由服务端校验"
	return doc
}

// graphOperationUnion merges the set/concat/copy operation field sets into
// one flat union, recording which operation owns each field.
func graphOperationUnion() ([]schema.Field, map[string][]string) {
	var union []schema.Field
	fieldTypes := make(map[string][]string)
	seen := make(map[string]bool)
	for _, op := range []string{"set", "concat", "copy"} {
		opSchema, ok := schema.OperationFor(op)
		if !ok {
			continue
		}
		for _, field := range opSchema.Fields {
			fieldTypes[field.Key] = append(fieldTypes[field.Key], op)
			if seen[field.Key] {
				continue
			}
			seen[field.Key] = true
			union = append(union, schema.Field{Key: field.Key, Kind: field.Kind, Enum: field.Enum})
		}
	}
	return union, fieldTypes
}

var graphConfigFieldDescriptions = map[string]string{
	"model":           "LLM model id；必须解析到本 workspace 的 Provider revision",
	"system_prompt":   "chat ToolLoop 的系统提示",
	"max_iterations":  "ToolLoop 最大工具迭代次数",
	"max_loops":       "最大图重入循环次数",
	"prompt_template": "提示模板；{{key}} 占位符读取状态值",
	"input_keys":      "传入模板的状态键列表",
	"output_key":      "步骤输出写入的状态键",
	"stream":          "是否流式输出 LLM 响应",
	"extract":         "可选布尔抽取（condition 路由用）：{key, mode, keywords_true}",
	"extract_mode":    "json（默认）或 keyword；keyword 需要 keywords_true",
	"json_field":      "json 模式下读取的响应字段",
	"keywords_true":   "keyword 模式下判定为 true 的关键词",
	"yield_type":      "yield/pause 变体",
	"operations":      "transform 操作数组：set / concat / copy；set 的 value 是字面量",
	"builtin_type":    "guard | prompt_assemble | memory_retrieve",
	"op":              "set 写字面量；concat 拼接状态键；copy 复制状态值",
	"target":          "写入的状态键名",
	"value":           "任意 JSON 字面量（set 专用）",
	"keys":            "concat 拼接的状态键列表",
	"separator":       "concat 分隔符（默认换行）",
	"source":          "copy 的源状态键名",
}

// --- shared schema helpers ---

func schemaObject(properties map[string]any, required []string) map[string]any {
	return schemaObjectWithAdditional(properties, required, false)
}

func schemaObjectWithAdditional(properties map[string]any, required []string, additional bool) map[string]any {
	doc := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if additional {
		doc["additionalProperties"] = true
	} else {
		doc["additionalProperties"] = false
	}
	if len(required) > 0 {
		doc["required"] = required
	}
	return doc
}

func buildRunIDSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func mustSchemaJSON(toolName string, doc map[string]any) json.RawMessage {
	data, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("%s input schema marshal failed: %v", toolName, err))
	}
	return data
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
