package teamforge

// Employee internal-graph write tools (plan §10.2.2). The five tools build
// one draft per build_run_id + agent name in dispatcher memory: step
// operations accumulate on the draft, tf_graph_validate runs the full
// validator, and tf_graph_commit lands one immutable agent version through
// WriteGate.writeTx + AgentRegistry.PutTx. Every call is receipt-gated on
// AssetRef{Kind: "agent"} and audited through the shared write skeleton
// (writegate.go); wiring (next / condition true|false) is maintained by the
// tools, never written as raw fields by the model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
)

const (
	// ToolGraphBegin opens a draft seeded from the agent's current
	// GraphDefinition (an empty graph when the agent has none).
	ToolGraphBegin = "tf_graph_begin"
	// ToolGraphApply applies a batch of graph operations to the current
	// draft; any failing op rejects the whole batch.
	ToolGraphApply = "tf_graph_apply"
	// ToolGraphValidate runs the full validator over the current draft
	// without writing anything.
	ToolGraphValidate = "tf_graph_validate"
	// ToolGraphCommit validates the whole draft and, when valid, PutTx's a
	// new immutable agent version (graph_type=declarative, non-CLI engine).
	ToolGraphCommit = "tf_graph_commit"
	// ToolGraphDiscard drops the current draft.
	ToolGraphDiscard = "tf_graph_discard"
	// ToolGraphBuild (tf_graph_build) is declared in tools_graph_build.go:
	// the one-call complete employee internal graph build tool (ticket T18).
)

// Batch operation vocabulary of tf_graph_apply. Wiring ops keep the raw
// Next/ConditionDef fields tool-owned; the model only declares intent.
const (
	graphOpAddStep           = "add_step"
	graphOpUpdateStepConfig  = "update_step_config"
	graphOpRemoveStep        = "remove_step"
	graphOpConnectNext       = "connect_next"
	graphOpConnectCondition  = "connect_condition"
	graphOpSetEntry          = "set_entry"
	graphOpSetOutputContract = "set_output_contract"
)

// GraphWriteToolsDispatcher exposes the five employee internal-graph write
// tools. The workspace, calling agent, and BuildAuthorizationReceipt are
// fixed at construction time; the draft table is injected from the shared
// server-level DraftRegistry (keyed by build_run_id + agent name) so drafts
// survive across requests and dispatcher instances.
type GraphWriteToolsDispatcher struct {
	gate   *WriteGate
	drafts *graphDraftStore
	tools  []contract.ToolDef
}

// NewGraphWriteTools creates the employee internal-graph write dispatcher for
// one workspace, one calling agent, and one build authorization receipt.
// validator is the production *teambuild.Store (or a fake); audit is the
// production *audit.Store (or a fake); deps aggregates the narrow write
// interfaces defined in deps.go; drafts is the build-run graph draft store
// from the shared registry (nil falls back to a private per-dispatcher store
// for tests that never cross dispatcher instances).
func NewGraphWriteTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps WriteDeps,
	drafts *graphDraftStore,
) *GraphWriteToolsDispatcher {
	d := &GraphWriteToolsDispatcher{
		gate:   newWriteGate(workspaceID, agentName, receipt, validator, audit, deps),
		drafts: drafts,
	}
	if d.drafts == nil {
		d.drafts = newGraphDraftStore()
	}
	d.tools = []contract.ToolDef{
		{
			Name: ToolGraphBegin,
			Description: "打开员工内部图草稿：加载目标 Agent 当前 GraphDefinition（无则空图），返回草稿摘要。" +
				"目标 Agent 必须已存在且在本建设任务授权范围内。Open a draft seeded from the target agent's current " +
				"graph (empty when none), returning the draft summary. The agent must exist and sit inside the " +
				"build authorization scope.",
			InputSchema: graphBeginSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolGraphApply,
			Description: "批量应用图操作到当前草稿：add_step / update_step_config / remove_step / connect_next / " +
				"connect_condition / set_entry / set_output_contract。每步操作带 seq；任一失败整批拒绝并指明第几个 op。" +
				"Apply a batch of graph operations to the current draft; a failing op rejects the whole batch with " +
				"its position. Wiring (next/condition true|false) is tool-maintained: declare intent, never raw fields.",
			InputSchema: graphApplySchema,
			ReadOnly:    false,
		},
		{
			Name: ToolGraphValidate,
			Description: "对当前草稿跑完整校验（entry 可达性、无不可达步、出边、环、config、condition key、worker 禁用），" +
				"返回结构化问题列表 {path, step, code, message, hint}，不落库。Validate the current draft without " +
				"writing; returns structured problems plus non-blocking L1 capability-point warnings.",
			InputSchema: graphBeginSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolGraphCommit,
			Description: "完整校验通过后以 graph_type=declarative 落一个新版本（F2 model 校验 + F13 非 CLI engine 在同一事务内），" +
				"返回新版本号；校验失败拒绝并返回问题列表。Commit the validated draft as the next immutable agent version; " +
				"rejects with the problem list when validation fails.",
			InputSchema: graphBeginSchema,
			ReadOnly:    false,
		},
		{
			Name:        ToolGraphDiscard,
			Description: "丢弃当前草稿，不落库。Discard the current in-memory draft; nothing is written.",
			InputSchema: graphBeginSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolGraphBuild,
			Description: "一次调用提交完整员工内部图：steps 数组（每步 type + config + 接线意图 next/condition）+ entry + output_contract。" +
				"JSON Schema 按 step 类型枚举各自 config 字段，未知字段在参数层即拒并列出允许字段；" +
				"语义为 begin → apply（全量）→ validate → commit 原子执行，任一失败不落库、draft 不留存，错误按 step 定位。" +
				"提交的 steps 即完整图结构，整体替换既有图。结果带非阻断 warning：output_contract 是 object schema 时提示" +
				"员工自由文本输出通常不可达（建议 json 无约束或 text），transform set op 的 value 是 {path,source} 形状时提示会被当作字面量。" +
				"Submit the complete employee internal graph in one call: steps (type + config + wiring intent) + entry + output_contract; " +
				"atomic begin→apply→validate→commit; unknown config fields are rejected at the parameter layer per step type.",
			InputSchema: graphBuildInputSchema,
			ReadOnly:    false,
		},
	}
	return d
}

// NewGraphBuildTools exposes only the atomic full-graph macro. Interactive
// meta-team chats use this bounded surface so every LLM turn does not carry
// the five legacy draft-operation schemas in addition to the macro schema.
// The underlying dispatcher and write gate are unchanged.
func NewGraphBuildTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps WriteDeps,
	drafts *graphDraftStore,
) *GraphWriteToolsDispatcher {
	d := NewGraphWriteTools(workspaceID, agentName, receipt, validator, audit, deps, drafts)
	d.tools = []contract.ToolDef{d.tools[len(d.tools)-1]}
	return d
}

// ListTools returns the six employee internal-graph write tools.
func (d *GraphWriteToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

// Dispatch gates the call on the receipt and call envelope, executes the
// matching graph tool, and records the outcome in the audit trail. Every
// failure is an IsError result, never a Go error.
func (d *GraphWriteToolsDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (result *contract.ToolResult, err error) {
	defer func() {
		d.gate.recordAudit(ctx, call, result, err)
	}()

	if !d.toolRegistered(call.Name) {
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}

	switch call.Name {
	case ToolGraphBegin:
		return d.graphBegin(ctx, call)
	case ToolGraphApply:
		return d.graphApply(ctx, call)
	case ToolGraphValidate:
		return d.graphValidate(ctx, call)
	case ToolGraphCommit:
		return d.graphCommit(ctx, call)
	case ToolGraphDiscard:
		return d.graphDiscard(ctx, call)
	case ToolGraphBuild:
		return d.graphBuild(ctx, call)
	default:
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

func (d *GraphWriteToolsDispatcher) toolRegistered(name string) bool {
	for _, tool := range d.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

var _ contract.ToolDispatcher = (*GraphWriteToolsDispatcher)(nil)

// --- call shapes ---

type graphCallInput struct {
	BuildRunID string `json:"build_run_id"`
	Agent      string `json:"agent"`
}

type graphApplyOp struct {
	Seq int    `json:"seq"`
	Op  string `json:"op"`

	// add_step
	Name    string         `json:"name,omitempty"`
	Type    string         `json:"type,omitempty"`
	Display string         `json:"display,omitempty"`
	Config  map[string]any `json:"config,omitempty"`

	// connect_next
	Next string `json:"next,omitempty"`

	// connect_condition
	Key   string `json:"key,omitempty"`
	True  string `json:"true,omitempty"`
	False string `json:"false,omitempty"`

	// set_entry
	Entry string `json:"entry,omitempty"`

	// set_output_contract
	OutputContract json.RawMessage `json:"output_contract,omitempty"`
}

type graphApplyInput struct {
	BuildRunID string         `json:"build_run_id"`
	Agent      string         `json:"agent"`
	Ops        []graphApplyOp `json:"ops"`
}

func parseGraphCall(call contract.ToolCall) (*graphCallInput, error) {
	var input graphCallInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(input.Agent) == "" {
		return nil, errors.New("agent is required")
	}
	input.Agent = strings.TrimSpace(input.Agent)
	return &input, nil
}

// resolveGraphCall parses the graph call and fills the omitted build_run_id
// from the dispatcher's construction-bound run. A provided id that
// contradicts the binding is rejected here, before any draft state is
// touched.
func (d *GraphWriteToolsDispatcher) resolveGraphCall(call contract.ToolCall) (*graphCallInput, error) {
	input, err := parseGraphCall(call)
	if err != nil {
		return nil, err
	}
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return nil, err
	}
	input.BuildRunID = runID
	return input, nil
}

// gateCheck runs the shared per-call write gate for one graph tool: the
// target agent (by stable name or stable ID) is resolved to its record and
// must sit inside the frozen receipt scope under AssetRef{ID+Name}; the call
// envelope must match the receipt's build run. The resolved record is
// returned so every graph op drafts and loads under the stable name.
func (d *GraphWriteToolsDispatcher) gateCheck(
	ctx context.Context,
	call contract.ToolCall,
	agent string,
) (*registry.AgentRecord, error) {
	// Built-in platform assets are rejected before any resolution: their
	// reserved "__" name prefix must never reach the registry or the scope
	// check (authorizeRef re-checks the resolved name as defense in depth).
	if strings.HasPrefix(agent, "__") {
		return nil, fmt.Errorf("%w: %q", ErrWriteBuiltinAssetForbidden, agent)
	}
	record, err := d.gate.resolveAgentTarget(ctx, agent)
	if err != nil {
		return nil, err
	}
	if err := d.gate.authorizeRef(ctx, teambuild.AssetRef{
		Kind: "agent", ID: record.ID, Name: record.Name,
	}); err != nil {
		return nil, err
	}
	return record, d.gate.requireBuildRunContext(call.Args)
}

// graphTargetError maps a gate/resolution failure onto the tool result,
// keeping the friendly "use tf_create_agent" hint for missing agents.
func graphTargetError(call contract.ToolCall, agent string, err error) *contract.ToolResult {
	if isAgentNotFound(err) {
		return toolError(call.ID, fmt.Sprintf("agent %q not found; use tf_create_agent", agent))
	}
	return toolError(call.ID, err.Error())
}

// --- tf_graph_begin ---

func (d *GraphWriteToolsDispatcher) graphBegin(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveGraphCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}

	draft := &graphDraft{BuildRunID: input.BuildRunID, AgentName: record.Name}
	if record.GraphDefinition != nil {
		draft.Graph = cloneGraphDefinition(*record.GraphDefinition)
	}
	if record.OutputSchema != nil {
		draft.OutputContract = append(json.RawMessage(nil), *record.OutputSchema...)
	}
	d.drafts.put(draft)
	return toolJSON(call.ID, d.draftSummary(draft))
}

// --- tf_graph_apply ---

func (d *GraphWriteToolsDispatcher) graphApply(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input graphApplyInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if strings.TrimSpace(input.Agent) == "" {
		return toolError(call.ID, "agent is required"), nil
	}
	input.Agent = strings.TrimSpace(input.Agent)
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}
	if len(input.Ops) == 0 {
		return toolError(call.ID, "ops is required and must be non-empty"), nil
	}
	draft := d.drafts.get(input.BuildRunID, record.Name)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for agent %q; call tf_graph_begin first", record.Name)), nil
	}

	// Work on a copy: any failing op rejects the whole batch and leaves the
	// stored draft untouched.
	working := graphDraft{
		BuildRunID:     draft.BuildRunID,
		AgentName:      draft.AgentName,
		Graph:          cloneGraphDefinition(draft.Graph),
		OutputContract: append(json.RawMessage(nil), draft.OutputContract...),
	}
	for i := range input.Ops {
		op := &input.Ops[i]
		if err := applyGraphOp(&working, op); err != nil {
			return toolError(call.ID, fmt.Sprintf("op #%d (seq %d) failed: %v", i+1, op.Seq, err)), nil
		}
	}
	d.drafts.put(&working)
	return toolJSON(call.ID, d.draftSummary(&working))
}

func applyGraphOp(draft *graphDraft, op *graphApplyOp) error {
	switch op.Op {
	case graphOpAddStep:
		return applyAddStep(draft, op)
	case graphOpUpdateStepConfig:
		return applyUpdateStepConfig(draft, op)
	case graphOpRemoveStep:
		return applyRemoveStep(draft, op)
	case graphOpConnectNext:
		return applyConnectNext(draft, op)
	case graphOpConnectCondition:
		return applyConnectCondition(draft, op)
	case graphOpSetEntry:
		return applySetEntry(draft, op)
	case graphOpSetOutputContract:
		return applySetOutputContract(draft, op)
	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
}

func applyAddStep(draft *graphDraft, op *graphApplyOp) error {
	if strings.TrimSpace(op.Name) == "" {
		return errors.New("add_step requires a non-empty name")
	}
	if strings.TrimSpace(op.Type) == "" {
		return errors.New("add_step requires a type")
	}
	if op.Type == "worker" {
		return errors.New("worker steps are forbidden in employee internal graphs")
	}
	if !validEmployeeStepType(op.Type) {
		return fmt.Errorf("invalid step type %q (allowed: chat, llm_call, llm_check, transform, builtin, yield)", op.Type)
	}
	for _, step := range draft.Graph.Steps {
		if step.Name == op.Name {
			return fmt.Errorf("step %q already exists", op.Name)
		}
	}
	draft.Graph.Steps = append(draft.Graph.Steps, registry.StepDefinition{
		Name:    op.Name,
		Type:    op.Type,
		Display: op.Display,
		Config:  cloneConfigMap(op.Config),
	})
	return nil
}

func validEmployeeStepType(stepType string) bool {
	switch stepType {
	case "chat", "llm_call", "llm_check", "transform", "builtin", "yield":
		return true
	default:
		return false
	}
}

func applyUpdateStepConfig(draft *graphDraft, op *graphApplyOp) error {
	idx, err := findGraphStepIndex(&draft.Graph, op.Name)
	if err != nil {
		return err
	}
	draft.Graph.Steps[idx].Config = cloneConfigMap(op.Config)
	return nil
}

func applyRemoveStep(draft *graphDraft, op *graphApplyOp) error {
	idx, err := findGraphStepIndex(&draft.Graph, op.Name)
	if err != nil {
		return err
	}
	draft.Graph.Steps = append(draft.Graph.Steps[:idx], draft.Graph.Steps[idx+1:]...)
	if draft.Graph.Entry == op.Name {
		draft.Graph.Entry = ""
	}
	return nil
}

func applyConnectNext(draft *graphDraft, op *graphApplyOp) error {
	idx, err := findGraphStepIndex(&draft.Graph, op.Name)
	if err != nil {
		return err
	}
	if op.Next != "" {
		if _, err := findGraphStepIndex(&draft.Graph, op.Next); err != nil {
			return fmt.Errorf("next target %q not found", op.Next)
		}
	}
	next := op.Next // "" explicitly ends the graph
	draft.Graph.Steps[idx].Next = &next
	draft.Graph.Steps[idx].Condition = nil
	return nil
}

func applyConnectCondition(draft *graphDraft, op *graphApplyOp) error {
	idx, err := findGraphStepIndex(&draft.Graph, op.Name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(op.Key) == "" {
		return errors.New("connect_condition requires a non-empty key")
	}
	if op.True == "" || op.False == "" {
		return errors.New("connect_condition requires non-empty true and false targets: an empty branch " +
			"cannot be frozen, so create an explicit finalize step and wire the branch to it")
	}
	for _, target := range []string{op.True, op.False} {
		if _, err := findGraphStepIndex(&draft.Graph, target); err != nil {
			return fmt.Errorf("condition target %q not found", target)
		}
	}
	// The routing key must be provably produced upstream of this step (the
	// step itself counts: it runs before its own router). Failing fast here
	// keeps the batch semantics and gives the model immediate feedback.
	if !teameval.GraphKeyProvable(&draft.Graph, teameval.BuildGraphAdjacency(&draft.Graph), draft.Graph.Steps[idx].Name, op.Key) {
		return fmt.Errorf(
			"condition key %q is not provably produced by an upstream llm_check output_key or llm_call extract.key; "+
				"add and wire the producing step before this condition", op.Key)
	}
	trueStep := op.True
	falseStep := op.False
	condition := registry.ConditionDef{
		Key:       op.Key,
		TrueStep:  &trueStep,
		FalseStep: &falseStep,
	}
	draft.Graph.Steps[idx].Condition = &condition
	draft.Graph.Steps[idx].Next = nil
	return nil
}

func applySetEntry(draft *graphDraft, op *graphApplyOp) error {
	if strings.TrimSpace(op.Entry) == "" {
		return errors.New("set_entry requires an entry step name")
	}
	if _, err := findGraphStepIndex(&draft.Graph, op.Entry); err != nil {
		return err
	}
	draft.Graph.Entry = op.Entry
	return nil
}

func applySetOutputContract(draft *graphDraft, op *graphApplyOp) error {
	if len(op.OutputContract) == 0 {
		return errors.New("set_output_contract requires an output_contract JSON object")
	}
	var probe map[string]any
	if err := json.Unmarshal(op.OutputContract, &probe); err != nil || probe == nil {
		return errors.New("output_contract must be a JSON object")
	}
	draft.OutputContract = append(json.RawMessage(nil), op.OutputContract...)
	return nil
}

func findGraphStepIndex(graph *registry.GraphDefinition, name string) (int, error) {
	for i, step := range graph.Steps {
		if step.Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("step %q not found", name)
}

// --- tf_graph_validate ---

type graphValidateJSON struct {
	BuildRunID string            `json:"build_run_id"`
	Agent      string            `json:"agent"`
	Valid      bool              `json:"valid"`
	Errors     []GraphProblem    `json:"errors"`
	Warnings   []GraphProblem    `json:"warnings"`
	Summary    graphDraftSummary `json:"summary"`
}

func (d *GraphWriteToolsDispatcher) graphValidate(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveGraphCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}
	draft := d.drafts.get(input.BuildRunID, record.Name)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for agent %q; call tf_graph_begin first", record.Name)), nil
	}
	validation := validateEmployeeGraph(&draft.Graph)
	return toolJSON(call.ID, graphValidateJSON{
		BuildRunID: input.BuildRunID,
		Agent:      input.Agent,
		Valid:      validation.Valid,
		Errors:     validation.Errors,
		Warnings:   validation.Warnings,
		Summary:    d.draftSummary(draft),
	})
}

// --- tf_graph_commit ---

type graphCommitJSON struct {
	BuildRunID     string           `json:"build_run_id"`
	Agent          string           `json:"agent"`
	Version        int              `json:"version"`
	GraphType      string           `json:"graph_type"`
	OutputContract *json.RawMessage `json:"output_contract,omitempty"`
	Committed      bool             `json:"committed"`
}

func (d *GraphWriteToolsDispatcher) graphCommit(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveGraphCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}
	draft := d.drafts.get(input.BuildRunID, record.Name)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for agent %q; call tf_graph_begin first", record.Name)), nil
	}
	return d.commitGraphDraft(ctx, call, input, record, draft), nil
}

// commitGraphDraft validates and commits one employee internal graph draft
// as the next immutable agent version. Shared by tf_graph_commit and
// tf_graph_build so the macro tool reuses the exact F2/F13 write path
// without copying it.
func (d *GraphWriteToolsDispatcher) commitGraphDraft(
	ctx context.Context,
	call contract.ToolCall,
	input *graphCallInput,
	record *registry.AgentRecord,
	draft *graphDraft,
) *contract.ToolResult {
	validation := validateEmployeeGraph(&draft.Graph)
	if !validation.Valid {
		payload, marshalErr := json.Marshal(graphValidateJSON{
			BuildRunID: input.BuildRunID,
			Agent:      input.Agent,
			Valid:      false,
			Errors:     validation.Errors,
			Warnings:   validation.Warnings,
			Summary:    d.draftSummary(draft),
		})
		if marshalErr != nil {
			return toolError(call.ID, fmt.Sprintf("commit rejected; validation failed: %v", marshalErr))
		}
		return toolError(call.ID, string(payload))
	}
	merged := buildCommittedRecord(record, draft)

	// F13 pre-gate (re-checked inside the tx below): a CLI engine cannot host
	// an internal graph, so the commit is rejected before any transaction is
	// opened.
	if engine.IsCLIEngine(merged.Engine) {
		return toolError(call.ID, fmt.Sprintf(
			"%v: agent %q is on engine %q; employee internal graphs require the loom engine",
			ErrWriteCLIGraphConflict, merged.Name, merged.Engine))
	}
	if err := d.gate.validateWriteRecord(ctx, &merged, record, false); err != nil {
		return toolError(call.ID, err.Error())
	}

	var committedVersion int
	if err := d.gate.writeTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// F13 inside the tx: engine cannot race onto a CLI engine between the
		// pre-gate and the version write.
		if engine.IsCLIEngine(merged.Engine) {
			return fmt.Errorf(
				"%w: agent %q is on engine %q; employee internal graphs require the loom engine",
				ErrWriteCLIGraphConflict, merged.Name, merged.Engine)
		}
		if err := d.gate.resolveModel(ctx, tx, &merged); err != nil {
			return err
		}
		if d.gate.deps.Agents == nil {
			return errors.New("agent registry write is unavailable")
		}
		if err := d.gate.deps.Agents.PutTx(ctx, tx, d.gate.workspaceID, &merged); err != nil {
			return err
		}
		committedVersion = merged.Version
		return nil
	}); err != nil {
		return toolError(call.ID, err.Error())
	}
	d.drafts.delete(input.BuildRunID, record.Name)
	result, _ := toolJSON(call.ID, graphCommitJSON{
		BuildRunID:     input.BuildRunID,
		Agent:          record.Name,
		Version:        committedVersion,
		GraphType:      merged.GraphType,
		OutputContract: merged.OutputSchema,
		Committed:      true,
	})
	return result
}

// buildCommittedRecord overlays the validated draft onto the current agent
// record: the draft graph becomes the record's GraphDefinition, graph_type is
// pinned to declarative, and the draft's output contract becomes the agent's
// output_schema. Everything else in the record stays untouched.
func buildCommittedRecord(existing *registry.AgentRecord, draft *graphDraft) registry.AgentRecord {
	merged := *existing
	graphCopy := cloneGraphDefinition(draft.Graph)
	merged.GraphDefinition = &graphCopy
	merged.GraphType = "declarative"
	if draft.OutputContract != nil {
		contractCopy := append(json.RawMessage(nil), draft.OutputContract...)
		merged.OutputSchema = &contractCopy
	}
	return merged
}

// --- tf_graph_discard ---

func (d *GraphWriteToolsDispatcher) graphDiscard(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveGraphCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	record, err := d.gateCheck(ctx, call, input.Agent)
	if err != nil {
		return graphTargetError(call, input.Agent, err), nil
	}
	if d.drafts.get(input.BuildRunID, record.Name) == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for agent %q; call tf_graph_begin first", record.Name)), nil
	}
	d.drafts.delete(input.BuildRunID, record.Name)
	return toolJSON(call.ID, map[string]any{
		"build_run_id": input.BuildRunID,
		"agent":        record.Name,
		"discarded":    true,
	})
}

// --- tool schemas ---

var graphBeginSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"agent": {"type": "string", "description": "Target employee agent; must exist and sit inside the build authorization scope"}
	},
	"required": ["agent"],
	"additionalProperties": false
}`)

var graphApplySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"agent": {"type": "string"},
		"ops": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"seq": {"type": "integer", "description": "Operation sequence number for batch error reporting"},
					"op": {"type": "string", "enum": ["add_step", "update_step_config", "remove_step", "connect_next", "connect_condition", "set_entry", "set_output_contract"]},
					"name": {"type": "string", "description": "Step name (add/update/remove/connect)"},
					"type": {"type": "string", "description": "add_step only: chat | llm_call | llm_check | transform | builtin | yield (worker forbidden)"},
					"display": {"type": "string", "description": "add_step only: optional display label"},
					"config": {"type": "object", "description": "add_step / update_step_config: step config per factory.go"},
					"next": {"type": "string", "description": "connect_next only: next step name; empty string explicitly ends the graph"},
					"key": {"type": "string", "description": "connect_condition only: state bool key produced by llm_check output_key or llm_call extract.key"},
					"true": {"type": "string", "description": "connect_condition only: true-branch step name; must be non-empty — an empty branch cannot be frozen, so create an explicit finalize step and wire it"},
					"false": {"type": "string", "description": "connect_condition only: false-branch step name; must be non-empty — an empty branch cannot be frozen, so create an explicit finalize step and wire it"},
					"entry": {"type": "string", "description": "set_entry only: entry step name"},
					"output_contract": {"type": "object", "description": "set_output_contract only: agent output contract JSON object"}
				},
				"required": ["seq", "op"],
				"additionalProperties": false
			}
		}
	},
	"required": ["agent", "ops"],
	"additionalProperties": false
}`)
