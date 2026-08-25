package teamforge

// tf_wf_build — the one-call team-workflow build tool (ticket T18). The
// model submits the complete workflow structure once: nodes (11 node types
// with per-type config field enumeration in the JSON Schema), edges (route
// enum; cardinality is judged by the validator), the entry node, the
// trigger, and optional input/output contracts. The tool then runs
// begin → apply → validate → commit atomically: the built trigger/graph
// content never lands before validation passes, no in-memory draft is ever
// stored, and errors are located per node with the correct field names.
// The submitted nodes/edges/trigger replace the workflow draft's content
// wholesale, so the call is idempotent. Results carry non-blocking D9
// reachability warnings for lead/worker output contracts whose object
// schema a ToolLoop employee's free-text output usually cannot satisfy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// ToolWorkflowBuild submits a complete team workflow in one call.
const ToolWorkflowBuild = "tf_wf_build"

// Failure phases of tf_wf_build inside the structured error JSON.
const (
	workflowBuildPhaseSchema   = "schema"
	workflowBuildPhaseApply    = "apply"
	workflowBuildPhaseValidate = "validate"
)

// workflowBuildNodeInput is one declared node: type + per-type config plus
// input bindings and an optional output contract.
type workflowBuildNodeInput struct {
	ID     string                          `json:"id"`
	Type   string                          `json:"type"`
	Label  string                          `json:"label,omitempty"`
	Config json.RawMessage                 `json:"config"`
	Inputs map[string]workflowBuildBinding `json:"inputs,omitempty"`
	Output *workflowBuildContractInput     `json:"output,omitempty"`
}

type workflowBuildBinding struct {
	ExpectedType string          `json:"expected_type"`
	Value        json.RawMessage `json:"value"`
}

type workflowBuildContractInput struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type workflowBuildEdgeInput struct {
	ID        string          `json:"id,omitempty"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Route     string          `json:"route"`
	Priority  *int64          `json:"priority,omitempty"`
	Predicate json.RawMessage `json:"predicate,omitempty"`
}

type workflowBuildInput struct {
	BuildRunID     string                      `json:"build_run_id"`
	WorkflowID     string                      `json:"workflow_id"`
	Create         *workflowBeginCreate        `json:"create,omitempty"`
	Nodes          []workflowBuildNodeInput    `json:"nodes"`
	Edges          []workflowBuildEdgeInput    `json:"edges"`
	Entry          string                      `json:"entry"`
	Trigger        json.RawMessage             `json:"trigger"`
	InputContract  *workflowBuildContractInput `json:"input_contract,omitempty"`
	OutputContract *workflowBuildContractInput `json:"output_contract,omitempty"`
}

type workflowBuildResultJSON struct {
	BuildRunID  string            `json:"build_run_id"`
	WorkflowID  string            `json:"workflow_id"`
	Tool        string            `json:"tool"`
	BuildMode   string            `json:"build_mode"`
	Committed   bool              `json:"committed"`
	Version     int               `json:"version"`
	UpdatedAt   time.Time         `json:"updated_at"`
	NodeCount   int               `json:"node_count"`
	EdgeCount   int               `json:"edge_count"`
	TriggerType string            `json:"trigger_type"`
	Warnings    []WorkflowProblem `json:"warnings"`
}

type workflowBuildFailureJSON struct {
	BuildRunID string            `json:"build_run_id"`
	WorkflowID string            `json:"workflow_id"`
	Tool       string            `json:"tool"`
	Committed  bool              `json:"committed"`
	Phase      string            `json:"phase"`
	Errors     []WorkflowProblem `json:"errors"`
	Warnings   []WorkflowProblem `json:"warnings"`
}

// --- tf_wf_build handler ---

func (d *WorkflowWriteToolsDispatcher) workflowBuild(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, problems, warnings := parseWorkflowBuildInput(call.Args)
	if len(problems) > 0 {
		return toolError(call.ID, workflowBuildFailureContent(input, workflowBuildPhaseSchema, problems, warnings)), nil
	}
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	ops, err := workflowBuildOps(&input)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	// Apply the full structure to a scratch draft first so every parameter
	// error (deep node config, ValueRef, trigger) surfaces before the
	// workflow carrier row is created or cloned.
	scratch := &workflowDraft{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
		Trigger:    machine.TriggerConfig{},
		Graph: machine.GraphDefinition{
			SchemaVersion:  machine.SchemaVersionV1,
			InputContract:  machine.OutputContract{Type: machine.ValueText},
			OutputContract: machine.OutputContract{Type: machine.ValueText},
		},
	}
	for i := range ops {
		if err := applyWorkflowOp(scratch, &ops[i]); err != nil {
			nodeID := ops[i].NodeID
			if nodeID == "" {
				nodeID = ops[i].From
			}
			problem := WorkflowProblem{
				NodeID:  nodeID,
				Path:    workflowBuildOpPath(&ops[i]),
				Message: err.Error(),
				Hint:    "按该节点的 schema 修复后重新 tf_wf_build",
			}
			return toolError(call.ID, workflowBuildFailureContent(
				input, workflowBuildPhaseApply, []WorkflowProblem{problem}, nil)), nil
		}
	}

	// begin: resolve the workflow CAS carrier (create row, existing draft,
	// or cloned published draft). Only this phase can write the carrier;
	// the built trigger/graph content is committed only after validation.
	carrier, err := d.openWorkflowBuildCarrier(ctx, input.BuildRunID, input.WorkflowID, input.Create)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	working := cloneWorkflowDraft(carrier)
	working.Trigger = scratch.Trigger
	working.Graph = scratch.Graph

	validation, err := validateWorkflowDraft(ctx, d.deps, d.gate.workspaceID, working)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if !validation.Valid {
		failureWarnings := append([]WorkflowProblem(nil), warnings...)
		failureWarnings = append(failureWarnings, validation.Warnings...)
		failureWarnings = append(failureWarnings, workflowBuildReachabilityWarnings(working.Graph)...)
		return toolError(call.ID, workflowBuildFailureContent(
			input, workflowBuildPhaseValidate, validation.Errors, failureWarnings)), nil
	}
	warnings = append(warnings, validation.Warnings...)
	warnings = append(warnings, workflowBuildReachabilityWarnings(working.Graph)...)

	result := d.commitWorkflowDraft(ctx, call, &workflowCallInput{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
	}, working)
	if result.IsError {
		return result, nil
	}
	var committed workflowCommitJSON
	if err := json.Unmarshal([]byte(result.Content), &committed); err != nil {
		return toolError(call.ID, "tf_wf_build commit result decode failed: "+err.Error()), nil
	}
	buildResult, _ := toolJSON(call.ID, workflowBuildResultJSON{
		BuildRunID:  input.BuildRunID,
		WorkflowID:  input.WorkflowID,
		Tool:        ToolWorkflowBuild,
		BuildMode:   "custom",
		Committed:   true,
		Version:     committed.Version,
		UpdatedAt:   committed.UpdatedAt,
		NodeCount:   len(working.Graph.Nodes),
		EdgeCount:   len(working.Graph.Edges),
		TriggerType: string(working.Trigger.Type),
		Warnings:    warnings,
	})
	return buildResult, nil
}

// openWorkflowBuildCarrier resolves only the mutable version used by the
// final CAS. Unlike incremental tf_wf_begin, a full build replaces trigger
// and graph immediately after this call, so decoding legacy content is both
// unnecessary and harmful: an invalid old graph would otherwise make itself
// impossible to replace with a valid new graph.
func (d *WorkflowWriteToolsDispatcher) openWorkflowBuildCarrier(
	ctx context.Context,
	buildRunID, workflowID string,
	create *workflowBeginCreate,
) (*workflowDraft, error) {
	if create != nil {
		return d.openWorkflowDraft(ctx, buildRunID, workflowID, create)
	}
	if d.deps.Workflows == nil {
		return nil, errors.New("workflow read is unavailable")
	}
	versions, err := d.deps.Workflows.ListVersionsByWorkflows(
		ctx, d.gate.workspaceID, []string{workflowID},
	)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions: %v", err)
	}
	for i := range versions {
		if versions[i].Status == workflow.VersionStatusDraft {
			return workflowBuildCarrier(buildRunID, workflowID, &versions[i], d.gate.agent), nil
		}
	}

	workflowRow, err := d.deps.Workflows.Get(ctx, d.gate.workspaceID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow %q not found; pass create to begin a new workflow: %v", workflowID, err)
	}
	if workflowRow.PublishedVersion == nil {
		return nil, fmt.Errorf(
			"workflow %q has no published source; pass create to begin a new workflow", workflowID)
	}
	if d.writeDeps.Workflows == nil {
		return nil, errors.New("workflow write is unavailable")
	}
	version, err := d.writeDeps.Workflows.CreateDraft(
		ctx, d.gate.workspaceID, workflowID, d.gate.agent,
	)
	if err != nil {
		return nil, fmt.Errorf("create draft for workflow %q: %v", workflowID, err)
	}
	return workflowBuildCarrier(buildRunID, workflowID, version, d.gate.agent), nil
}

func workflowBuildCarrier(
	buildRunID, workflowID string,
	version *workflow.TeamWorkflowVersion,
	createdBy string,
) *workflowDraft {
	return &workflowDraft{
		BuildRunID: buildRunID,
		WorkflowID: workflowID,
		Version:    version.Version,
		UpdatedAt:  version.UpdatedAt,
		CreatedBy:  createdBy,
	}
}

// workflowBuildOps converts one complete workflow build input into the
// tf_wf_apply op vocabulary. Trigger and contracts come first, then all
// nodes (with input bindings and output contracts), then edges, then entry.
func workflowBuildOps(input *workflowBuildInput) ([]workflowApplyOp, error) {
	var ops []workflowApplyOp
	seq := 0
	nextSeq := func() int {
		seq++
		return seq
	}
	ops = append(ops, workflowApplyOp{
		Seq: nextSeq(), Op: workflowOpSetTrigger, Trigger: input.Trigger,
	})
	if input.InputContract != nil {
		contractRaw, err := marshalWorkflowBuildContract(*input.InputContract)
		if err != nil {
			return nil, fmt.Errorf("input_contract: %w", err)
		}
		ops = append(ops, workflowApplyOp{
			Seq: nextSeq(), Op: workflowOpSetInputContract, Contract: contractRaw,
		})
	}
	if input.OutputContract != nil {
		contractRaw, err := marshalWorkflowBuildContract(*input.OutputContract)
		if err != nil {
			return nil, fmt.Errorf("output_contract: %w", err)
		}
		ops = append(ops, workflowApplyOp{
			Seq: nextSeq(), Op: workflowOpSetOutputContract, Contract: contractRaw,
		})
	}
	for i := range input.Nodes {
		node := &input.Nodes[i]
		ops = append(ops, workflowApplyOp{
			Seq: nextSeq(), Op: workflowOpAddNode,
			NodeID: node.ID, Type: node.Type, Label: node.Label, Config: node.Config,
		})
		for name, binding := range node.Inputs {
			ops = append(ops, workflowApplyOp{
				Seq: nextSeq(), Op: workflowOpSetNodeInput,
				NodeID: node.ID, Name: name,
				ExpectedType: binding.ExpectedType, Value: binding.Value,
			})
		}
		if node.Output != nil {
			outputRaw, err := marshalWorkflowBuildContract(*node.Output)
			if err != nil {
				return nil, fmt.Errorf("node %q output: %w", node.ID, err)
			}
			ops = append(ops, workflowApplyOp{
				Seq: nextSeq(), Op: workflowOpSetNodeOutput,
				NodeID: node.ID, Output: outputRaw,
			})
		}
	}
	for i := range input.Edges {
		edge := &input.Edges[i]
		op := workflowApplyOp{
			Seq: nextSeq(), Op: workflowOpConnect,
			EdgeID: edge.ID, From: edge.From, To: edge.To,
			Route: edge.Route, Priority: edge.Priority,
		}
		if len(edge.Predicate) > 0 {
			op.Predicate = edge.Predicate
		}
		ops = append(ops, op)
	}
	ops = append(ops, workflowApplyOp{
		Seq: nextSeq(), Op: workflowOpSetEntry, EntryNodeID: input.Entry,
	})
	return ops, nil
}

func marshalWorkflowBuildContract(contract workflowBuildContractInput) (json.RawMessage, error) {
	payload := map[string]any{"type": contract.Type}
	if len(contract.Schema) > 0 {
		payload["schema"] = contract.Schema
	}
	return json.Marshal(payload)
}

func workflowBuildOpPath(op *workflowApplyOp) string {
	switch op.Op {
	case workflowOpAddNode, workflowOpUpdateNodeConfig, workflowOpRemoveNode,
		workflowOpSetNodeInput, workflowOpSetNodeOutput:
		if op.NodeID != "" {
			return "nodes[" + op.NodeID + "]"
		}
	case workflowOpConnect:
		if op.From != "" {
			return "edges[" + op.From + "->" + op.To + "]"
		}
	}
	return fmt.Sprintf("ops[%d]", op.Seq)
}

// --- parameter validation (schema layer) ---

// parseWorkflowBuildInput strictly parses the tool arguments and validates
// node configs against the per-type field enumeration (the D2 54-try
// failure mode — guessing worker config field names — is rejected here with
// the allowed field list), plus light checks on trigger, contracts,
// bindings, and edges. Deep ValueRef/predicate/trigger semantics ride on
// the typed op decoders, which run on the scratch draft before any write.
func parseWorkflowBuildInput(raw string) (workflowBuildInput, []WorkflowProblem, []WorkflowProblem) {
	var input workflowBuildInput
	if err := strictRaw(json.RawMessage(raw), &input); err != nil {
		return input, []WorkflowProblem{{
			Message: "invalid input: " + err.Error(),
			Hint:    "按 tf_wf_build 的 JSON Schema 提交参数",
		}}, nil
	}
	var problems, warnings []WorkflowProblem
	input.WorkflowID = strings.TrimSpace(input.WorkflowID)
	input.Entry = strings.TrimSpace(input.Entry)
	if input.WorkflowID == "" {
		problems = append(problems, WorkflowProblem{Path: "workflow_id", Message: "workflow_id is required"})
	}
	if input.Create != nil {
		if strings.TrimSpace(input.Create.Name) == "" {
			problems = append(problems, WorkflowProblem{Path: "create.name", Message: "create.name is required"})
		}
		if strings.TrimSpace(input.Create.TeamID) == "" {
			problems = append(problems, WorkflowProblem{Path: "create.team_id", Message: "create.team_id is required"})
		}
	}
	if len(input.Nodes) == 0 {
		problems = append(problems, WorkflowProblem{Path: "nodes", Message: "nodes is required and must be non-empty"})
	}
	if input.Entry == "" {
		problems = append(problems, WorkflowProblem{Path: "entry", Message: "entry is required"})
	}
	if len(input.Trigger) == 0 {
		problems = append(problems, WorkflowProblem{Path: "trigger", Message: "trigger is required"})
	} else {
		problems = append(problems, validateWorkflowBuildTrigger(input.Trigger)...)
	}
	if input.InputContract != nil {
		problems = append(problems, validateWorkflowBuildContract("input_contract", input.InputContract)...)
	}
	if input.OutputContract != nil {
		problems = append(problems, validateWorkflowBuildContract("output_contract", input.OutputContract)...)
	}
	for i := range input.Nodes {
		nodeProblems, nodeWarnings := validateWorkflowBuildNode(i, &input.Nodes[i])
		problems = append(problems, nodeProblems...)
		warnings = append(warnings, nodeWarnings...)
	}
	for i := range input.Edges {
		problems = append(problems, validateWorkflowBuildEdge(i, &input.Edges[i])...)
	}
	return input, problems, warnings
}

func validateWorkflowBuildNode(index int, node *workflowBuildNodeInput) ([]WorkflowProblem, []WorkflowProblem) {
	basePath := fmt.Sprintf("nodes[%d]", index)
	nodeID := strings.TrimSpace(node.ID)
	nodeType := strings.TrimSpace(node.Type)
	var problems []WorkflowProblem
	if nodeID == "" {
		problems = append(problems, WorkflowProblem{Path: basePath + ".id", Message: "node id is required"})
	}
	_, known := workflowNodeConfigSpecs[nodeType]
	if !known {
		problems = append(problems, WorkflowProblem{
			Path: basePath + ".type", NodeID: nodeID,
			Message: fmt.Sprintf("invalid node type %q", nodeType),
			Hint:    "allowed: lead, worker, transform, condition, parallel, join, wait, loop, deliver, handoff",
		})
		return problems, nil
	}
	fields, err := decodeRawFields(node.Config)
	if err != nil {
		problems = append(problems, WorkflowProblem{
			Path: basePath + ".config", NodeID: nodeID, Message: "config must be a JSON object",
		})
		return problems, nil
	}
	problems = append(problems, validateWorkflowNodeConfigFields(basePath+".config", nodeID, nodeType, fields)...)
	for name, binding := range node.Inputs {
		bindingPath := fmt.Sprintf("%s.inputs.%s", basePath, name)
		if !workflowValueTypes[binding.ExpectedType] {
			problems = append(problems, WorkflowProblem{
				Path: bindingPath + ".expected_type", NodeID: nodeID,
				Message: fmt.Sprintf("expected_type must be text, json, boolean, or number (got %q)", binding.ExpectedType),
				Hint:    "ValueType：text=裸 JSON 字符串；json=任意 JSON 值；boolean=布尔；number=数字",
			})
		}
		if len(binding.Value) == 0 {
			problems = append(problems, WorkflowProblem{
				Path: bindingPath + ".value", NodeID: nodeID, Message: "value is required",
			})
		} else if _, err := decodeRawFields(binding.Value); err != nil {
			problems = append(problems, WorkflowProblem{
				Path: bindingPath + ".value", NodeID: nodeID,
				Message: "value must be a ValueRef object",
				Hint:    "ValueRef: {source, path?, node_id?, value?, iteration?, default?}",
			})
		}
	}
	if node.Output != nil {
		problems = append(problems, validateWorkflowBuildContract(basePath+".output", node.Output)...)
		if workflowOutputForbidden[nodeType] {
			problems = append(problems, WorkflowProblem{
				Path: basePath + ".output", NodeID: nodeID,
				Message: fmt.Sprintf("output contract is forbidden on %s nodes", nodeType),
			})
		}
	}
	return problems, nil
}

// validateWorkflowNodeConfigFields checks one node config object against the
// per-type field enumeration: unknown fields are rejected with the allowed
// list, required fields and per-kind types are checked. Deep ValueRef and
// predicate semantics are enforced by decodeNodeConfigStrict at apply time.
func validateWorkflowNodeConfigFields(basePath, nodeID, nodeType string, fields map[string]json.RawMessage) []WorkflowProblem {
	spec := workflowNodeConfigSpecs[nodeType]
	allowed := make([]string, 0, len(spec.Fields))
	for _, field := range spec.Fields {
		allowed = append(allowed, field.Key)
	}
	var problems []WorkflowProblem
	for key := range fields {
		if workflowNodeSpecHasField(spec, key) {
			continue
		}
		problems = append(problems, WorkflowProblem{
			Path: basePath + "." + key, NodeID: nodeID,
			Message: fmt.Sprintf("unknown field %q", key),
			Hint:    fmt.Sprintf("%s config allows: %s", nodeType, strings.Join(allowed, ", ")),
		})
	}
	for _, field := range spec.Fields {
		value, ok := fields[field.Key]
		if !ok {
			if field.Required {
				problems = append(problems, WorkflowProblem{
					Path: basePath + "." + field.Key, NodeID: nodeID,
					Message: fmt.Sprintf("config.%s is required", field.Key),
				})
			}
			continue
		}
		if message := checkWorkflowNodeFieldValue(field, value); message != "" {
			problems = append(problems, WorkflowProblem{
				Path: basePath + "." + field.Key, NodeID: nodeID,
				Message: fmt.Sprintf("config.%s %s", field.Key, message),
			})
		}
	}
	return problems
}

func workflowNodeSpecHasField(spec workflowNodeConfigSpec, key string) bool {
	for _, field := range spec.Fields {
		if field.Key == key {
			return true
		}
	}
	return false
}

func checkWorkflowNodeFieldValue(field workflowNodeFieldSpec, raw json.RawMessage) string {
	switch field.JSONType {
	case "string", "enum":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a string"
		}
		if field.JSONType == "enum" && !stringInSlice(s, field.Enum) {
			return fmt.Sprintf("must be one of %s", strings.Join(field.Enum, ", "))
		}
	case "integer":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != math.Trunc(n) {
			return "must be an integer"
		}
	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "must be a boolean"
		}
	case "object", "value_ref", "predicate", "value_ref_map":
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			return "must be an object"
		}
	case "array", "value_ref_array":
		var probe []json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			return "must be an array"
		}
	}
	return ""
}

func validateWorkflowBuildEdge(index int, edge *workflowBuildEdgeInput) []WorkflowProblem {
	basePath := fmt.Sprintf("edges[%d]", index)
	var problems []WorkflowProblem
	if strings.TrimSpace(edge.From) == "" {
		problems = append(problems, WorkflowProblem{Path: basePath + ".from", Message: "from is required"})
	}
	if strings.TrimSpace(edge.To) == "" {
		problems = append(problems, WorkflowProblem{Path: basePath + ".to", Message: "to is required"})
	}
	if !edgeRoutes[edge.Route] {
		problems = append(problems, WorkflowProblem{
			Path:    basePath + ".route",
			Message: fmt.Sprintf("route %q is not in the machine route vocabulary", edge.Route),
			Hint:    "allowed: success, failure, case, default, branch, join, timeout, body, exit, back",
		})
	}
	if len(edge.Predicate) > 0 {
		if _, err := decodeRawFields(edge.Predicate); err != nil {
			problems = append(problems, WorkflowProblem{
				Path: basePath + ".predicate", Message: "predicate must be a JSON object",
			})
		}
	}
	return problems
}

func validateWorkflowBuildTrigger(raw json.RawMessage) []WorkflowProblem {
	fields, err := decodeRawFields(raw)
	if err != nil {
		return []WorkflowProblem{{Path: "trigger", Message: "trigger must be a JSON object"}}
	}
	var problems []WorkflowProblem
	var schemaVersion int
	if err := json.Unmarshal(fields["schema_version"], &schemaVersion); err != nil {
		problems = append(problems, WorkflowProblem{
			Path: "trigger.schema_version", Message: "schema_version must be an integer",
		})
	} else if schemaVersion != machine.SchemaVersionV1 {
		problems = append(problems, WorkflowProblem{
			Path: "trigger.schema_version", Message: fmt.Sprintf("trigger schema_version must be %d", machine.SchemaVersionV1),
		})
	}
	var triggerType string
	if err := json.Unmarshal(fields["type"], &triggerType); err != nil {
		problems = append(problems, WorkflowProblem{Path: "trigger.type", Message: "trigger type must be a string"})
	} else {
		switch machine.TriggerType(triggerType) {
		case machine.TriggerConversationExplicit, machine.TriggerConversationAuto,
			machine.TriggerSchedule, machine.TriggerAPI, machine.TriggerEvent:
		default:
			problems = append(problems, WorkflowProblem{
				Path:    "trigger.type",
				Message: fmt.Sprintf("trigger type must be conversation_explicit, conversation_auto, schedule, api, or event (got %q)", triggerType),
			})
		}
	}
	if rawConfig, ok := fields["config"]; ok {
		if _, err := decodeRawFields(rawConfig); err != nil {
			problems = append(problems, WorkflowProblem{Path: "trigger.config", Message: "trigger config must be an object"})
		}
	} else {
		problems = append(problems, WorkflowProblem{Path: "trigger.config", Message: "trigger config is required"})
	}
	if rawDelivery, ok := fields["delivery"]; ok {
		deliveryFields, err := decodeRawFields(rawDelivery)
		if err != nil {
			problems = append(problems, WorkflowProblem{Path: "trigger.delivery", Message: "delivery must be a JSON object"})
		} else {
			var kind string
			if err := json.Unmarshal(deliveryFields["kind"], &kind); err != nil {
				problems = append(problems, WorkflowProblem{Path: "trigger.delivery.kind", Message: "delivery kind must be a string"})
			} else if !stringInSlice(kind, []string{"job_record", "callback_ref", "target_ref"}) {
				problems = append(problems, WorkflowProblem{
					Path:    "trigger.delivery.kind",
					Message: fmt.Sprintf("delivery kind must be job_record, callback_ref, or target_ref (got %q)", kind),
				})
			}
		}
	}
	return problems
}

func validateWorkflowBuildContract(path string, contract *workflowBuildContractInput) []WorkflowProblem {
	var problems []WorkflowProblem
	if !workflowValueTypes[contract.Type] {
		problems = append(problems, WorkflowProblem{
			Path:    path + ".type",
			Message: fmt.Sprintf("contract type must be text, json, boolean, or number (got %q)", contract.Type),
			Hint:    "ValueType：text=裸 JSON 字符串；json=任意 JSON 值；boolean=布尔；number=数字",
		})
	}
	if len(contract.Schema) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(contract.Schema, &probe); err != nil || probe == nil {
			problems = append(problems, WorkflowProblem{
				Path: path + ".schema", Message: "contract schema must be a JSON object",
			})
		}
	}
	return problems
}

// --- D9 result warnings ---

// workflowBuildReachabilityWarnings implements the D9 contract-reachability
// guidance: a lead/worker node whose output contract is an object schema
// (json + schema) usually cannot be satisfied by the referenced employee's
// free-text ToolLoop output, so the build reports a non-blocking warning.
func workflowBuildReachabilityWarnings(graph machine.GraphDefinition) []WorkflowProblem {
	var warnings []WorkflowProblem
	for _, node := range graph.Nodes {
		if node.Type != machine.NodeLead && node.Type != machine.NodeWorker {
			continue
		}
		if node.Output == nil || node.Output.Type != machine.ValueJSON || len(node.Output.Schema) == 0 {
			continue
		}
		warnings = append(warnings, WorkflowProblem{
			Path: "/nodes/" + node.ID + "/output", NodeID: node.ID,
			Code: "workflow_warning_employee_object_unreachable",
			Message: fmt.Sprintf(
				"%s 节点 %q 的 output 合同是 object schema（json + schema）：员工自由文本输出通常不可达",
				node.Type, node.ID),
			Hint: "建议 json 无约束或 text（output 合同改为 {\"type\":\"json\"} 任意 JSON 值或 {\"type\":\"text\"}）；" +
				"若必须约束结构，给员工配置声明式内部图并让图直接产出该结构",
		})
	}
	return warnings
}

func workflowBuildFailureContent(
	input workflowBuildInput,
	phase string,
	problems, warnings []WorkflowProblem,
) string {
	payload := workflowBuildFailureJSON{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
		Tool:       ToolWorkflowBuild,
		Committed:  false,
		Phase:      phase,
		Errors:     problems,
		Warnings:   warnings,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("%s failed (%s): %v", ToolWorkflowBuild, phase, err)
	}
	return string(data)
}

// --- per-type node config facts (mirrors machine/types.go + the strict
// decoders) ---

type workflowNodeFieldSpec struct {
	Key      string
	JSONType string // string | integer | boolean | object | array | enum | value_ref | predicate | value_ref_map | value_ref_array
	Enum     []string
	Required bool
}

type workflowNodeConfigSpec struct {
	Description string
	Fields      []workflowNodeFieldSpec
}

var workflowNodeConfigSpecs = map[string]workflowNodeConfigSpec{
	"lead": {
		Description: "团队负责人节点：读取运行输入并派发",
		Fields: []workflowNodeFieldSpec{
			{Key: "instruction", JSONType: "string", Required: true},
		},
	},
	"worker": {
		Description: "员工节点：引用精确 Agent 版本",
		Fields: []workflowNodeFieldSpec{
			{Key: "agent_id", JSONType: "string", Required: true},
			{Key: "agent_version", JSONType: "integer", Required: true},
			{Key: "kind", JSONType: "enum", Enum: []string{"consult", "dispatch"}, Required: true},
			{Key: "result_requirement", JSONType: "string", Required: true},
		},
	},
	"transform": {
		Description: "值变换节点：identity/object/array",
		Fields: []workflowNodeFieldSpec{
			{Key: "operation", JSONType: "enum", Enum: []string{"identity", "object", "array"}, Required: true},
			{Key: "value", JSONType: "value_ref"},
			{Key: "fields", JSONType: "value_ref_map"},
			{Key: "items", JSONType: "value_ref_array"},
		},
	},
	"condition": {
		Description: "条件节点：无 config 字段",
	},
	"parallel": {
		Description: "并行分发节点",
		Fields: []workflowNodeFieldSpec{
			{Key: "join_node_id", JSONType: "string", Required: true},
		},
	},
	"join": {
		Description: "并行汇合节点",
		Fields: []workflowNodeFieldSpec{
			{Key: "policy", JSONType: "enum", Enum: []string{"all_success", "quorum", "deadline", "fail_fast"}, Required: true},
			{Key: "success_count", JSONType: "integer"},
			{Key: "deadline_seconds", JSONType: "integer"},
		},
	},
	"wait": {
		Description: "等待节点（会话触发）",
		Fields: []workflowNodeFieldSpec{
			{Key: "resume_schema", JSONType: "object", Required: true},
			{Key: "timeout_seconds", JSONType: "integer"},
		},
	},
	"loop": {
		Description: "机器原生返修循环：body 边进并行体，latch 的 back 边回 header，exit 边离开（F14）",
		Fields: []workflowNodeFieldSpec{
			{Key: "max_iterations", JSONType: "integer", Required: true},
			{Key: "latch_node_id", JSONType: "string", Required: true},
			{Key: "continue_predicate", JSONType: "predicate", Required: true},
		},
	},
	"deliver": {
		Description: "交付节点：最终结果写入 workflow 输出合同",
		Fields: []workflowNodeFieldSpec{
			{Key: "result", JSONType: "value_ref", Required: true},
		},
	},
	"handoff": {
		Description: "转交节点：交给精确 Agent 版本",
		Fields: []workflowNodeFieldSpec{
			{Key: "agent_id", JSONType: "string", Required: true},
			{Key: "agent_version", JSONType: "integer", Required: true},
			{Key: "instruction", JSONType: "string", Required: true},
			{Key: "timeout_seconds", JSONType: "integer"},
		},
	},
}

var workflowNodeTypeOrder = []string{
	"lead", "worker", "transform", "condition", "parallel", "join",
	"wait", "loop", "deliver", "handoff",
}

// workflowOutputForbidden mirrors applyWorkflowSetNodeOutput's per-type
// rule: only lead/worker/transform declare output contracts.
var workflowOutputForbidden = map[string]bool{
	"condition": true, "parallel": true, "join": true, "wait": true,
	"loop": true, "deliver": true, "handoff": true,
}

// --- tool input schema (generated from the machine node/ValueRef/Trigger
// facts) ---

// T22 (DeepSeek schema compat): the wire InputSchema must stay inside
// DeepSeek's function-calling JSON Schema subset — no oneOf/anyOf/$ref and
// no other out-of-subset constraint keywords (const, minItems) or
// schema-valued additionalProperties. The node config is a flat object
// whose property set is the union of every node type's fields, all optional
// at the schema layer; the type discriminator keeps its enum, every field
// description names the owning types, and the per-type required/mutex/enum
// rules run in the server-side validation (parse layer + machine strict
// decoders) with field-level errors.

var workflowBuildInputSchema = mustSchemaJSON("tf_wf_build", workflowBuildInputSchemaDoc())

func workflowBuildInputSchemaDoc() map[string]any {
	nodeConfigProperties := make(map[string]any, 21)
	configTypeNames := make([]string, 0, len(workflowNodeTypeOrder))
	for _, nodeType := range workflowNodeTypeOrder {
		configTypeNames = append(configTypeNames, nodeType+" config")
	}
	union, fieldTypes := workflowNodeConfigUnion()
	for _, field := range union {
		doc := workflowNodeFieldSchemaDoc(field)
		doc["description"] = "仅 " + strings.Join(fieldTypes[field.Key], "/") + "；" +
			workflowNodeFieldDescriptions[field.Key]
		nodeConfigProperties[field.Key] = doc
	}
	configDoc := schemaObject(nodeConfigProperties, nil)
	configDoc["description"] = "Node config：字段集为 " + strings.Join(configTypeNames, " / ") +
		" 的并集，schema 层均非必填；类型专属必填/互斥/枚举由服务端校验，错误保持字段级定位"
	nodeItem := schemaObject(map[string]any{
		"id": map[string]any{"type": "string", "description": "Node ID；图内唯一"},
		"type": map[string]any{
			"type": "string", "enum": workflowNodeTypeOrder,
			"description": "Node type；config 字段为各类型并集（未知字段在 schema 层即拒）；类型专属字段适用性由服务端校验",
		},
		"label":  map[string]any{"type": "string", "description": "可选显示标签"},
		"config": configDoc,
		"inputs": map[string]any{
			"type":        "object",
			"description": "Input bindings: 绑定名 -> {expected_type, value}；绑定名任意，expected_type/value 必填与 ValueRef 形状由服务端校验",
		},
		"output": workflowContractSchemaDoc(
			"Node output 合同；lead/worker/transform 必填，condition/parallel/join/wait/loop/deliver/handoff 禁止",
		),
	}, []string{"id", "type", "config"})
	edgeItem := schemaObject(map[string]any{
		"id":   map[string]any{"type": "string", "description": "可选边 id；省略自动生成"},
		"from": map[string]any{"type": "string"},
		"to":   map[string]any{"type": "string"},
		"route": map[string]any{
			"type":        "string",
			"enum":        []string{"success", "failure", "case", "default", "branch", "join", "timeout", "body", "exit", "back"},
			"description": "route 与源节点类型的配对和出边基数由 validator 判定",
		},
		"priority":  map[string]any{"type": "integer", "description": "仅 condition case 边"},
		"predicate": workflowPredicateSchemaDoc(),
	}, []string{"from", "to", "route"})
	return schemaObject(map[string]any{
		"build_run_id": buildRunIDSchema("可选；省略时自动绑定当前授权 run"),
		"workflow_id": map[string]any{
			"type": "string", "description": "目标 Workflow ID；create 模式为新建 ID，否则必须已存在且在授权范围内",
		},
		"create": schemaObject(map[string]any{
			"name":        map[string]any{"type": "string", "description": "Workflow 显示名（create 必填）"},
			"team_id":     map[string]any{"type": "string", "description": "归属 Team ID（create 必填）"},
			"description": map[string]any{"type": "string"},
		}, []string{"name", "team_id"}),
		"nodes": map[string]any{
			"type": "array", "items": nodeItem,
			"description": "完整节点结构：提交后整体替换 workflow draft 的图内容；非空由服务端校验",
		},
		"edges": map[string]any{
			"type": "array", "items": edgeItem,
			"description": "完整边结构（route 枚举；基数由 validator 判）",
		},
		"entry":           map[string]any{"type": "string", "description": "入口节点 id；必须存在于 nodes"},
		"trigger":         workflowTriggerSchemaDoc(),
		"input_contract":  workflowContractSchemaDoc("Workflow 输入合同；省略默认 {\"type\":\"text\"}"),
		"output_contract": workflowContractSchemaDoc("Workflow 输出合同；省略默认 {\"type\":\"text\"}"),
	}, []string{"workflow_id", "nodes", "edges", "entry", "trigger"})
}

// workflowNodeConfigUnion merges the per-type node config field sets into
// one flat union. Every union member is non-required at the schema layer;
// the owning node types are recorded so the model-facing description can
// point at the right type. Per-type required/unknown-field/cross-field rules
// stay in the server-side validation (parse layer + machine strict
// decoders).
func workflowNodeConfigUnion() ([]workflowNodeFieldSpec, map[string][]string) {
	var union []workflowNodeFieldSpec
	fieldTypes := make(map[string][]string)
	seen := make(map[string]bool)
	for _, nodeType := range workflowNodeTypeOrder {
		spec := workflowNodeConfigSpecs[nodeType]
		for _, field := range spec.Fields {
			fieldTypes[field.Key] = append(fieldTypes[field.Key], nodeType)
			if seen[field.Key] {
				continue
			}
			seen[field.Key] = true
			union = append(union, workflowNodeFieldSpec{Key: field.Key, JSONType: field.JSONType, Enum: field.Enum})
		}
	}
	return union, fieldTypes
}

func workflowNodeFieldSchemaDoc(field workflowNodeFieldSpec) map[string]any {
	doc := map[string]any{"description": workflowNodeFieldDescriptions[field.Key]}
	switch field.JSONType {
	case "string":
		doc["type"] = "string"
	case "integer":
		doc["type"] = "integer"
	case "boolean":
		doc["type"] = "boolean"
	case "object":
		doc["type"] = "object"
	case "array":
		doc["type"] = "array"
	case "enum":
		doc["type"] = "string"
		doc["enum"] = field.Enum
	case "value_ref":
		doc = workflowValueRefSchemaDoc()
	case "predicate":
		doc = workflowPredicateSchemaDoc()
	case "value_ref_map":
		// Flat object: binding names are arbitrary, so no closed property
		// set and no schema-valued additionalProperties; each value's
		// ValueRef shape is validated by the server-side strict decoder.
		doc["type"] = "object"
		doc["description"] = "字段名 -> ValueRef（值结构由服务端校验）"
	case "value_ref_array":
		doc["type"] = "array"
		doc["items"] = workflowValueRefSchemaDoc()
	}
	return doc
}

var workflowNodeFieldDescriptions = map[string]string{
	"instruction":        "节点指令/任务说明",
	"agent_id":           "引用的精确员工 Agent 稳定 ID",
	"agent_version":      "引用的精确 Agent 版本（必须存在）",
	"kind":               "consult=咨询；dispatch=派工（parallel 的 branch 必须指向 dispatch worker）",
	"result_requirement": "对员工产出的结果要求",
	"operation":          "identity=透传一个 ValueRef；object=按字段映射组装对象；array=按 items 组装数组",
	"value":              "identity 变换的源 ValueRef",
	"fields":             "object 变换：目标字段名 -> 源 ValueRef",
	"items":              "array 变换：ValueRef 数组",
	"join_node_id":       "parallel 归属的 join 节点 id",
	"policy":             "all_success/quorum/deadline/fail_fast 之一",
	"success_count":      "quorum 所需成功数（正数）",
	"deadline_seconds":   "deadline 超时秒数（正数）",
	"resume_schema":      "wait 恢复载荷的 JSON Schema（object）",
	"timeout_seconds":    "超时秒数（可选）",
	"max_iterations":     "循环最大迭代次数（正数）",
	"latch_node_id":      "循环 latch 节点 id（back 边的唯一来源）",
	"continue_predicate": "继续循环的谓词（通常判断 latch 输出是否仍为 failed）",
	"result":             "交付结果 ValueRef（必须与 workflow output_contract 兼容）",
}

func workflowValueRefSchemaDoc() map[string]any {
	return schemaObject(map[string]any{
		"source": map[string]any{
			"type": "string", "enum": []string{"run_input", "node_output", "literal"},
			"description": "run_input=运行输入；node_output=上游节点输出；literal=字面量",
		},
		"path": map[string]any{
			"type":        "string",
			"description": "JSON Pointer（空串或 / 开头）；run_input 必填，node_output 可为空串",
		},
		"node_id": map[string]any{"type": "string", "description": "node_output 必填：产生输出的上游节点 id"},
		"value":   map[string]any{"description": "literal 的 JSON 值（任意类型）"},
		"iteration": map[string]any{
			"type": "string", "enum": []string{"current_iteration", "previous_iteration"},
			"description": "仅 loop body 内引用 body 节点输出时必须",
		},
		"default": map[string]any{
			"type":        "object",
			"description": "仅 previous_iteration 引用：字面量兜底 {source: literal, value: ...}",
		},
	}, []string{"source"})
}

func workflowPredicateSchemaDoc() map[string]any {
	return schemaObject(map[string]any{
		"left":     workflowValueRefSchemaDoc(),
		"operator": map[string]any{"type": "string", "enum": []string{"exists", "eq", "neq", "gt", "gte", "lt", "lte", "contains", "in"}},
		"right":    workflowValueRefSchemaDoc(),
	}, []string{"left", "operator"})
}

func workflowContractSchemaDoc(description string) map[string]any {
	return schemaObject(map[string]any{
		"type": map[string]any{
			"type": "string", "enum": []string{"text", "json", "boolean", "number"},
			"description": "ValueType：text=裸 JSON 字符串；json=任意 JSON 值；boolean=布尔；number=数字",
		},
		"schema": map[string]any{
			"type": "object",
			"description": "仅 json 可带。object schema 对员工自由文本输出通常不可达（触发可达性 warning，不阻断）；" +
				"建议 json 无约束或 text",
		},
	}, []string{"type"})
}

func workflowTriggerSchemaDoc() map[string]any {
	// Flat config union (no oneOf/anyOf): every trigger-type field is
	// optional at the schema layer; the per-type config requirement and the
	// session-vs-delivery rules are enforced by the server-side validation
	// (parse layer + machine strict trigger decoder).
	configDoc := schemaObject(map[string]any{
		"catalog_key":  map[string]any{"type": "string", "description": "仅 conversation_auto；catalog 键"},
		"schedule_id":  map[string]any{"type": "string", "description": "仅 schedule；调度 ID"},
		"endpoint_key": map[string]any{"type": "string", "description": "仅 api；端点键"},
		"event_type":   map[string]any{"type": "string", "description": "仅 event；事件类型"},
	}, nil)
	configDoc["description"] = "触发配置：各 trigger type 字段并集，schema 层均非必填；类型专属必填/互斥由服务端校验"
	return schemaObject(map[string]any{
		"schema_version": map[string]any{
			"type":        "integer",
			"description": "固定为 1；取值由服务端校验",
		},
		"type": map[string]any{
			"type":        "string",
			"enum":        []string{"conversation_explicit", "conversation_auto", "schedule", "api", "event"},
			"description": "conversation_explicit/auto 会话触发禁止 delivery；schedule/api/event 必须配置 delivery{kind,ref}",
		},
		"config": configDoc,
		"delivery": schemaObject(map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"job_record", "callback_ref", "target_ref"}},
			"ref":  map[string]any{"type": "string", "description": "callback_ref/target_ref 必填；job_record 禁止"},
		}, []string{"kind"}),
	}, []string{"schema_version", "type", "config"})
}
