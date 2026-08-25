package teamforge

// Team-workflow write tools (plan §10.2.3). The five tools build one draft
// per build_run_id + workflow_id in dispatcher memory: node/edge/binding/
// contract/trigger operations accumulate on the typed machine model,
// tf_wf_validate runs the full machine validator with proofs read from the
// real stores, and tf_wf_commit encodes the draft into trigger_config +
// graph_definition and lands one CAS UpdateDraft. Every call is
// receipt-gated on AssetRef{Kind: "workflow"} and audited through the shared
// write skeleton (writegate.go). The model never composes raw graph JSON:
// tool output is the only encoding that reaches the workflow store, and
// commit re-runs the strict machine decoders over it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const (
	// ToolWorkflowBegin opens a draft: creates a workflow with its v1 draft,
	// clones a published workflow into the next draft, or loads an existing
	// draft.
	ToolWorkflowBegin = "tf_wf_begin"
	// ToolWorkflowApply applies a batch of typed workflow operations; any
	// failing op rejects the whole batch.
	ToolWorkflowApply = "tf_wf_apply"
	// ToolWorkflowValidate runs the full machine validator over the draft.
	ToolWorkflowValidate = "tf_wf_validate"
	// ToolWorkflowCommit validates, strictly re-encodes, and CAS-updates the
	// mutable draft.
	ToolWorkflowCommit = "tf_wf_commit"
	// ToolWorkflowDiscard drops the in-memory draft.
	ToolWorkflowDiscard = "tf_wf_discard"
	// ToolWorkflowBuild (tf_wf_build) is declared in
	// tools_workflow_build.go: the one-call complete team-workflow build
	// tool (ticket T18).
)

// Batch operation vocabulary of tf_wf_apply.
const (
	workflowOpAddNode           = "add_node"
	workflowOpUpdateNodeConfig  = "update_node_config"
	workflowOpRemoveNode        = "remove_node"
	workflowOpSetNodeInput      = "set_node_input"
	workflowOpSetNodeOutput     = "set_node_output"
	workflowOpConnect           = "connect"
	workflowOpSetEntry          = "set_entry"
	workflowOpSetInputContract  = "set_input_contract"
	workflowOpSetOutputContract = "set_output_contract"
	workflowOpSetTrigger        = "set_trigger"
)

// WorkflowWriteToolsDispatcher exposes the five team-workflow write tools.
// The workspace, calling agent, and BuildAuthorizationReceipt are fixed at
// construction time; the draft table is injected from the shared server-level
// DraftRegistry (keyed by build_run_id + workflow_id) so drafts survive
// across requests and dispatcher instances.
type WorkflowWriteToolsDispatcher struct {
	gate      *WriteGate
	deps      Deps
	writeDeps WriteDeps
	drafts    *workflowDraftStore
	tools     []contract.ToolDef
}

// NewWorkflowWriteTools creates the team-workflow write dispatcher for one
// workspace, one calling agent, and one build authorization receipt.
// validator is the production *teambuild.Store (or a fake); audit is the
// production *audit.Store (or a fake); deps aggregates the narrow read
// surfaces used to assemble the validation context; writeDeps carries the
// narrow write surfaces (workflow store, team creator, roster command);
// drafts is the build-run workflow draft store from the shared registry (nil
// falls back to a private per-dispatcher store for tests that never cross
// dispatcher instances).
func NewWorkflowWriteTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps Deps,
	writeDeps WriteDeps,
	drafts *workflowDraftStore,
) *WorkflowWriteToolsDispatcher {
	d := &WorkflowWriteToolsDispatcher{
		gate:      newWriteGate(workspaceID, agentName, receipt, validator, audit, writeDeps),
		deps:      deps,
		writeDeps: writeDeps,
		drafts:    drafts,
	}
	if d.drafts == nil {
		d.drafts = newWorkflowDraftStore()
	}
	d.tools = []contract.ToolDef{
		{
			Name: ToolWorkflowBegin,
			Description: "打开团队流程草稿：create 时原子新建 workflow+v1 draft，否则克隆 published 版本为新 draft（已有 draft 则加载）。" +
				"Open a workflow draft: create atomically builds workflow+v1 draft; otherwise clone the published version (or load an existing draft), " +
				"returning the draft summary (version, node/edge counts, trigger type).",
			InputSchema: workflowBeginSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolWorkflowApply,
			Description: "批量应用类型化流程操作到当前草稿：add_node / update_node_config / remove_node / set_node_input / set_node_output / " +
				"connect（route+priority/predicate）/ set_entry / set_input_contract / set_output_contract / set_trigger。任一 op 失败整批拒绝并指明第几个 op。" +
				"Apply a batch of typed workflow operations; a failing op rejects the whole batch with its position. Wiring is tool-maintained: " +
				"declare intent, never raw graph JSON.",
			InputSchema: workflowApplySchema,
			ReadOnly:    false,
		},
		{
			Name: ToolWorkflowValidate,
			Description: "对当前草稿跑完整 machine validator（拓扑/route/loop/parallel/condition/值绑定可证明性/trigger/授权/Agent 版本/依赖/factory），" +
				"返回结构化问题 {path, node_id, code, message, hint}，另附非阻断 L1 能力点警告。Validate the draft without writing; " +
				"returns structured problems plus non-blocking L1 capability-point warnings.",
			InputSchema: workflowIDOnlySchema,
			ReadOnly:    false,
		},
		{
			Name: ToolWorkflowCommit,
			Description: "完整校验通过后把草稿严格编码为 trigger_config + graph_definition 并 UpdateDraft（CAS），返回新 draft 状态；" +
				"校验失败或版本冲突拒绝。Commit the validated draft through the workflow store's CAS UpdateDraft; rejects with the problem list " +
				"when validation fails or the draft version changed.",
			InputSchema: workflowIDOnlySchema,
			ReadOnly:    false,
		},
		{
			Name:        ToolWorkflowDiscard,
			Description: "丢弃当前内存草稿，不落库。Discard the current in-memory draft; nothing is written.",
			InputSchema: workflowIDOnlySchema,
			ReadOnly:    false,
		},
		{
			Name: ToolWorkflowBlueprintBuild,
			Description: "默认的团队流程构建入口：提交小型 Blueprint，平台确定性编译节点、边、路由、ValueRef 与有界返修结构，再运行完整 proof-backed validator 并原子提交。" +
				"返修模板：delivery_rework_loop / creative_critique_loop（primary→reviewer→PASS/REVISE→反馈返修→deliver）；" +
				"汇总模板：parallel_review / research_synthesis（parallel workers→join→finalizer→deliver）。" +
				"Use this by default; it returns all blueprint field errors in one response.",
			InputSchema: workflowBlueprintBuildInputSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolWorkflowBuild,
			Description: "显式定制逃生口，仅当四个 Blueprint 模板确实无法表达需求时使用；一次调用提交完整团队流程：workflow（新/现有）+ nodes 数组（11 类节点 config 字段按类型枚举，未知字段参数层即拒）+ " +
				"edges（route 枚举，基数由 validator 判）+ entry + trigger（+ 可选 input/output contract）。" +
				"语义为 begin → apply（全量）→ validate → commit 原子执行：构建内容在校验通过前不落库，draft 不留存，错误按节点定位。" +
				"提交的 nodes/edges/trigger 整体替换 workflow draft 内容。结果带非阻断 D9 warning：lead/worker output 合同是 object schema 时提示" +
				"员工自由文本输出通常不可达（建议 json 无约束或 text）。" +
				"Submit the complete team workflow in one call: workflow (new/existing) + nodes (per-type config fields) + edges + entry + trigger; " +
				"atomic begin→apply→validate→commit; unknown node config fields are rejected at the parameter layer.",
			InputSchema: workflowBuildInputSchema,
			ReadOnly:    false,
		},
	}
	return d
}

// NewWorkflowBuildTools exposes only the small Blueprint compiler. The raw
// graph schema is deliberately absent from the default LLM tool surface, so
// conversation history cannot silently downgrade a normal build into custom
// graph authoring.
func NewWorkflowBuildTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps Deps,
	writeDeps WriteDeps,
	drafts *workflowDraftStore,
) *WorkflowWriteToolsDispatcher {
	d := NewWorkflowWriteTools(
		workspaceID, agentName, receipt, validator, audit, deps, writeDeps, drafts,
	)
	d.tools = []contract.ToolDef{d.tools[len(d.tools)-2]}
	return d
}

// NewCustomWorkflowBuildTools exposes only the raw full-graph builder. The API
// wiring selects this dispatcher exclusively when the frozen BuildBrief was
// admin-confirmed with workflow_build_mode=custom and a non-empty template_gap.
func NewCustomWorkflowBuildTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps Deps,
	writeDeps WriteDeps,
	drafts *workflowDraftStore,
) *WorkflowWriteToolsDispatcher {
	d := NewWorkflowWriteTools(
		workspaceID, agentName, receipt, validator, audit, deps, writeDeps, drafts,
	)
	d.tools = []contract.ToolDef{d.tools[len(d.tools)-1]}
	return d
}

// ListTools returns the team-workflow write tools configured for this dispatcher.
func (d *WorkflowWriteToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

// Dispatch gates the call on the receipt and call envelope, executes the
// matching workflow tool, and records the outcome in the audit trail. Every
// failure is an IsError result, never a Go error.
func (d *WorkflowWriteToolsDispatcher) Dispatch(
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
	case ToolWorkflowBegin:
		return d.workflowBegin(ctx, call)
	case ToolWorkflowApply:
		return d.workflowApply(ctx, call)
	case ToolWorkflowValidate:
		return d.workflowValidate(ctx, call)
	case ToolWorkflowCommit:
		return d.workflowCommit(ctx, call)
	case ToolWorkflowDiscard:
		return d.workflowDiscard(ctx, call)
	case ToolWorkflowBlueprintBuild:
		return d.workflowBlueprintBuild(ctx, call)
	case ToolWorkflowBuild:
		return d.workflowBuild(ctx, call)
	default:
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

func (d *WorkflowWriteToolsDispatcher) toolRegistered(name string) bool {
	for _, tool := range d.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

var _ contract.ToolDispatcher = (*WorkflowWriteToolsDispatcher)(nil)

// --- call shapes ---

type workflowCallInput struct {
	BuildRunID string `json:"build_run_id"`
	WorkflowID string `json:"workflow_id"`
}

func parseWorkflowCall(call contract.ToolCall) (*workflowCallInput, error) {
	var input workflowCallInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(input.WorkflowID) == "" {
		return nil, errors.New("workflow_id is required")
	}
	input.WorkflowID = strings.TrimSpace(input.WorkflowID)
	return &input, nil
}

// resolveWorkflowCall parses the workflow call and fills the omitted
// build_run_id from the dispatcher's construction-bound run. A provided id
// that contradicts the binding is rejected before any draft state is
// touched.
func (d *WorkflowWriteToolsDispatcher) resolveWorkflowCall(call contract.ToolCall) (*workflowCallInput, error) {
	input, err := parseWorkflowCall(call)
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

// gateCheck runs the shared per-call write gate for one workflow tool: the
// target workflow must sit inside the frozen receipt scope and the call
// envelope must match the receipt's build run.
func (d *WorkflowWriteToolsDispatcher) gateCheck(ctx context.Context, call contract.ToolCall, workflowID string) error {
	// The workflow ID is the platform identity in both modes: create-mode
	// scopes match it as the naming-namespace identity (Name) and
	// optimize-mode scopes match it as the pinned stable ID. Passing both
	// fields fixes the ID-only optimize ref that the old name-only gate
	// could never satisfy.
	if err := d.gate.authorizeRef(ctx, teambuild.AssetRef{
		Kind: "workflow", ID: workflowID, Name: workflowID,
	}); err != nil {
		return err
	}
	return d.gate.requireBuildRunContext(call.Args)
}

// --- tf_wf_begin ---

type workflowBeginCreate struct {
	Name        string `json:"name"`
	TeamID      string `json:"team_id"`
	Description string `json:"description"`
}

type workflowBeginInput struct {
	BuildRunID string               `json:"build_run_id"`
	WorkflowID string               `json:"workflow_id"`
	Create     *workflowBeginCreate `json:"create,omitempty"`
}

func (d *WorkflowWriteToolsDispatcher) workflowBegin(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input workflowBeginInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if strings.TrimSpace(input.WorkflowID) == "" {
		return toolError(call.ID, "workflow_id is required"), nil
	}
	input.WorkflowID = strings.TrimSpace(input.WorkflowID)
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	if existing := d.drafts.get(input.BuildRunID, input.WorkflowID); existing != nil {
		return toolJSON(call.ID, d.draftSummary(existing, true))
	}
	draft, err := d.openWorkflowDraft(ctx, input.BuildRunID, input.WorkflowID, input.Create)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	d.drafts.put(draft)
	return toolJSON(call.ID, d.draftSummary(draft, false))
}

// openWorkflowDraft resolves the workflow CAS carrier without touching the
// in-memory draft table: create mode atomically builds the workflow row with
// its v1 draft, otherwise the existing draft is loaded, or a published
// version is cloned into a new draft. Shared by tf_wf_begin and
// tf_wf_build so the macro tool reuses the begin semantics without copying
// them — and without leaving a draft behind on failure.
func (d *WorkflowWriteToolsDispatcher) openWorkflowDraft(
	ctx context.Context,
	buildRunID, workflowID string,
	create *workflowBeginCreate,
) (*workflowDraft, error) {
	if create != nil {
		// Creating a workflow requires either the create-mode namespace or
		// one exact workflow ID frozen into an optimize receipt.
		if err := d.gate.authorizeExactCreate(ctx, teambuild.AssetRef{
			Kind: "workflow", ID: workflowID, Name: workflowID,
		}); err != nil {
			return nil, fmt.Errorf("workflow create rejected: %v", err)
		}
		if d.writeDeps.Workflows == nil {
			return nil, errors.New("workflow write is unavailable")
		}
		if strings.TrimSpace(create.Name) == "" || strings.TrimSpace(create.TeamID) == "" {
			return nil, errors.New("create.name and create.team_id are required")
		}
		version, err := d.writeDeps.Workflows.Create(ctx, &workflow.TeamWorkflow{
			WorkspaceID: d.gate.workspaceID,
			ID:          workflowID,
			TeamID:      strings.TrimSpace(create.TeamID),
			Name:        strings.TrimSpace(create.Name),
			Description: create.Description,
			Status:      workflow.WorkflowStatusActive,
		}, workflow.DraftInput{
			TriggerConfig:   emptyTriggerJSON,
			GraphDefinition: emptyGraphJSON,
			CreatedBy:       d.gate.agent,
		})
		if err != nil {
			if isWorkflowUniqueViolation(err) {
				draft, reuseErr := d.openExistingWorkflowDraftForCreate(ctx, buildRunID, workflowID, create)
				if reuseErr == nil {
					return draft, nil
				}
				return nil, fmt.Errorf("create workflow %q collided with an existing workflow and could not reuse it: %v", workflowID, reuseErr)
			}
			return nil, fmt.Errorf("create workflow %q: %v", workflowID, err)
		}
		trigger, triggerReport := machine.DecodeTriggerConfigV1(version.TriggerConfig)
		graph, graphReport := machine.DecodeGraphDefinitionV1(version.GraphDefinition)
		if (triggerReport != nil && len(triggerReport.Issues) != 0) ||
			(graphReport != nil && len(graphReport.Issues) != 0) {
			return nil, errors.New("created draft failed the strict decoder; begin again")
		}
		return &workflowDraft{
			BuildRunID: buildRunID,
			WorkflowID: workflowID,
			Version:    version.Version,
			Trigger:    trigger,
			Graph:      graph,
			UpdatedAt:  version.UpdatedAt,
			CreatedBy:  d.gate.agent,
		}, nil
	}

	if d.deps.Workflows == nil {
		return nil, errors.New("workflow read is unavailable")
	}
	versions, err := d.deps.Workflows.ListVersionsByWorkflows(ctx, d.gate.workspaceID, []string{workflowID})
	if err != nil {
		return nil, fmt.Errorf("list workflow versions: %v", err)
	}
	for i := range versions {
		if versions[i].Status != workflow.VersionStatusDraft {
			continue
		}
		draft, err := loadWorkflowDraft(buildRunID, workflowID, &versions[i], d.gate.agent)
		if err != nil {
			return nil, err
		}
		return draft, nil
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
	version, err := d.writeDeps.Workflows.CreateDraft(ctx, d.gate.workspaceID, workflowID, d.gate.agent)
	if err != nil {
		return nil, fmt.Errorf("create draft for workflow %q: %v", workflowID, err)
	}
	draft, err := loadWorkflowDraft(buildRunID, workflowID, version, d.gate.agent)
	if err != nil {
		return nil, err
	}
	return draft, nil
}

func (d *WorkflowWriteToolsDispatcher) openExistingWorkflowDraftForCreate(
	ctx context.Context,
	buildRunID, workflowID string,
	create *workflowBeginCreate,
) (*workflowDraft, error) {
	if create == nil {
		return nil, errors.New("create contract is required")
	}
	if d.deps.Workflows == nil {
		return nil, errors.New("workflow read is unavailable")
	}
	workflowRow, err := d.deps.Workflows.Get(ctx, d.gate.workspaceID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("read existing workflow %q after create collision: %v", workflowID, err)
	}
	expectedTeamID := strings.TrimSpace(create.TeamID)
	if workflowRow.TeamID != expectedTeamID {
		return nil, fmt.Errorf("existing workflow belongs to team %q, not requested team %q", workflowRow.TeamID, expectedTeamID)
	}
	return d.openWorkflowDraft(ctx, buildRunID, workflowID, nil)
}

func isWorkflowUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func loadWorkflowDraft(buildRunID, workflowID string, version *workflow.TeamWorkflowVersion, createdBy string) (*workflowDraft, error) {
	trigger, triggerReport := machine.DecodeTriggerConfigV1(version.TriggerConfig)
	graph, graphReport := machine.DecodeGraphDefinitionV1(version.GraphDefinition)
	if (triggerReport != nil && len(triggerReport.Issues) != 0) ||
		(graphReport != nil && len(graphReport.Issues) != 0) {
		return nil, fmt.Errorf("stored draft %q v%d failed the strict decoder; refuse to load", workflowID, version.Version)
	}
	return &workflowDraft{
		BuildRunID: buildRunID,
		WorkflowID: workflowID,
		Version:    version.Version,
		Trigger:    trigger,
		Graph:      graph,
		UpdatedAt:  version.UpdatedAt,
		CreatedBy:  createdBy,
	}, nil
}

// --- tf_wf_apply ---

type workflowApplyOp struct {
	Seq int    `json:"seq"`
	Op  string `json:"op"`

	// add_node / update_node_config
	NodeID string          `json:"node_id,omitempty"`
	Type   string          `json:"type,omitempty"`
	Label  string          `json:"label,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`

	// connect
	EdgeID    string          `json:"edge_id,omitempty"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Route     string          `json:"route,omitempty"`
	Priority  *int64          `json:"priority,omitempty"`
	Predicate json.RawMessage `json:"predicate,omitempty"`

	// set_node_input
	Name         string          `json:"name,omitempty"`
	ExpectedType string          `json:"expected_type,omitempty"`
	Value        json.RawMessage `json:"value,omitempty"`

	// set_node_output / set_input_contract / set_output_contract
	Output   json.RawMessage `json:"output,omitempty"`
	Contract json.RawMessage `json:"contract,omitempty"`

	// set_entry
	EntryNodeID string `json:"entry_node_id,omitempty"`

	// set_trigger
	Trigger json.RawMessage `json:"trigger,omitempty"`
}

type workflowApplyInput struct {
	BuildRunID string            `json:"build_run_id"`
	WorkflowID string            `json:"workflow_id"`
	Ops        []workflowApplyOp `json:"ops"`
}

func (d *WorkflowWriteToolsDispatcher) workflowApply(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input workflowApplyInput
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	if strings.TrimSpace(input.WorkflowID) == "" {
		return toolError(call.ID, "workflow_id is required"), nil
	}
	input.WorkflowID = strings.TrimSpace(input.WorkflowID)
	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if len(input.Ops) == 0 {
		return toolError(call.ID, "ops is required and must be non-empty"), nil
	}
	draft := d.drafts.get(input.BuildRunID, input.WorkflowID)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for workflow %q; call tf_wf_begin first", input.WorkflowID)), nil
	}

	// Work on a copy: any failing op rejects the whole batch and leaves the
	// stored draft untouched.
	working := cloneWorkflowDraft(draft)
	for i := range input.Ops {
		op := &input.Ops[i]
		if err := applyWorkflowOp(working, op); err != nil {
			return toolError(call.ID, fmt.Sprintf("op #%d (seq %d) failed: %v", i+1, op.Seq, err)), nil
		}
	}
	d.drafts.put(working)
	return toolJSON(call.ID, d.draftSummary(working, false))
}

func applyWorkflowOp(draft *workflowDraft, op *workflowApplyOp) error {
	switch op.Op {
	case workflowOpAddNode:
		return applyWorkflowAddNode(draft, op)
	case workflowOpUpdateNodeConfig:
		return applyWorkflowUpdateNodeConfig(draft, op)
	case workflowOpRemoveNode:
		return applyWorkflowRemoveNode(draft, op)
	case workflowOpSetNodeInput:
		return applyWorkflowSetNodeInput(draft, op)
	case workflowOpSetNodeOutput:
		return applyWorkflowSetNodeOutput(draft, op)
	case workflowOpConnect:
		return applyWorkflowConnect(draft, op)
	case workflowOpSetEntry:
		return applyWorkflowSetEntry(draft, op)
	case workflowOpSetInputContract:
		return applyWorkflowSetContract(draft, op, false)
	case workflowOpSetOutputContract:
		return applyWorkflowSetContract(draft, op, true)
	case workflowOpSetTrigger:
		return applyWorkflowSetTrigger(draft, op)
	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
}

func applyWorkflowAddNode(draft *workflowDraft, op *workflowApplyOp) error {
	nodeID := strings.TrimSpace(op.NodeID)
	if nodeID == "" {
		return errors.New("add_node requires a non-empty node_id")
	}
	nodeType := strings.TrimSpace(op.Type)
	if nodeType == "" {
		return errors.New("add_node requires a type")
	}
	if !workflowNodeTypes[nodeType] {
		return fmt.Errorf(
			"invalid node type %q (allowed: lead, worker, transform, condition, parallel, join, wait, loop, deliver, handoff)",
			nodeType)
	}
	if len(op.Config) == 0 {
		return fmt.Errorf("add_node %q requires a config object", nodeID)
	}
	for _, node := range draft.Graph.Nodes {
		if node.ID == nodeID {
			return fmt.Errorf("node %q already exists", nodeID)
		}
	}
	config, err := decodeNodeConfigStrict(machine.NodeType(nodeType), op.Config)
	if err != nil {
		return err
	}
	draft.Graph.Nodes = append(draft.Graph.Nodes, machine.Node{
		ID:     nodeID,
		Type:   machine.NodeType(nodeType),
		Label:  op.Label,
		Config: config,
	})
	return nil
}

func applyWorkflowUpdateNodeConfig(draft *workflowDraft, op *workflowApplyOp) error {
	index, node, err := findWorkflowNode(&draft.Graph, op.NodeID)
	if err != nil {
		return err
	}
	if len(op.Config) == 0 {
		return fmt.Errorf("update_node_config %q requires a config object", node.ID)
	}
	config, err := decodeNodeConfigStrict(node.Type, op.Config)
	if err != nil {
		return err
	}
	draft.Graph.Nodes[index].Config = config
	return nil
}

func applyWorkflowRemoveNode(draft *workflowDraft, op *workflowApplyOp) error {
	index, _, err := findWorkflowNode(&draft.Graph, op.NodeID)
	if err != nil {
		return err
	}
	nodeID := draft.Graph.Nodes[index].ID
	draft.Graph.Nodes = append(draft.Graph.Nodes[:index], draft.Graph.Nodes[index+1:]...)
	edges := draft.Graph.Edges[:0]
	for _, edge := range draft.Graph.Edges {
		if edge.FromNodeID != nodeID && edge.ToNodeID != nodeID {
			edges = append(edges, edge)
		}
	}
	draft.Graph.Edges = edges
	if draft.Graph.EntryNodeID == nodeID {
		draft.Graph.EntryNodeID = ""
	}
	return nil
}

func applyWorkflowSetNodeInput(draft *workflowDraft, op *workflowApplyOp) error {
	index, node, err := findWorkflowNode(&draft.Graph, op.NodeID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(op.Name) == "" {
		return errors.New("set_node_input requires a non-empty name")
	}
	if !workflowValueTypes[op.ExpectedType] {
		return fmt.Errorf("set_node_input expected_type must be text, json, boolean, or number (got %q)", op.ExpectedType)
	}
	if len(op.Value) == 0 {
		return fmt.Errorf("set_node_input %q requires a value ValueRef", op.Name)
	}
	value, err := decodeValueRefStrict(op.Value)
	if err != nil {
		return err
	}
	if node.Inputs == nil {
		node.Inputs = make(map[string]machine.InputBinding)
	}
	node.Inputs[op.Name] = machine.InputBinding{
		ExpectedType: machine.ValueType(op.ExpectedType),
		Value:        value,
	}
	draft.Graph.Nodes[index].Inputs = node.Inputs
	return nil
}

func applyWorkflowSetNodeOutput(draft *workflowDraft, op *workflowApplyOp) error {
	index, node, err := findWorkflowNode(&draft.Graph, op.NodeID)
	if err != nil {
		return err
	}
	if len(op.Output) == 0 {
		return fmt.Errorf("set_node_output %q requires an output contract", node.ID)
	}
	contract, err := decodeOutputContractStrict(op.Output)
	if err != nil {
		return err
	}
	switch node.Type {
	case machine.NodeCondition, machine.NodeParallel, machine.NodeJoin,
		machine.NodeWait, machine.NodeLoop,
		machine.NodeDeliver, machine.NodeHandoff:
		return fmt.Errorf("output contract is forbidden on %s nodes", node.Type)
	}
	draft.Graph.Nodes[index].Output = &contract
	return nil
}

func applyWorkflowConnect(draft *workflowDraft, op *workflowApplyOp) error {
	fromIndex, fromNode, err := findWorkflowNode(&draft.Graph, op.From)
	if err != nil {
		return err
	}
	if _, _, err := findWorkflowNode(&draft.Graph, op.To); err != nil {
		return err
	}
	route := strings.TrimSpace(op.Route)
	if route == "" {
		return errors.New("connect requires a route")
	}
	if !edgeRoutes[route] {
		return fmt.Errorf("connect route %q is not in the machine route vocabulary", route)
	}
	edgeID := strings.TrimSpace(op.EdgeID)
	if edgeID == "" {
		edgeID = fmt.Sprintf("edge-%d", len(draft.Graph.Edges)+1)
	}
	for _, edge := range draft.Graph.Edges {
		if edge.ID == edgeID {
			return fmt.Errorf("edge id %q already exists", edgeID)
		}
	}

	if route == "back" {
		if err := validateWorkflowBackEdge(draft, fromIndex, op.To); err != nil {
			return err
		}
	} else if !workflowRouteAllowedForNode(fromNode, route) {
		return fmt.Errorf("route %q is not allowed for source node type %q", route, fromNode.Type)
	}
	if op.Predicate != nil || op.Priority != nil {
		if fromNode.Type != machine.NodeCondition || route != "case" {
			return errors.New("priority/predicate are only allowed on condition case edges")
		}
	}
	var predicate *machine.Predicate
	if len(op.Predicate) != 0 {
		decoded, err := decodePredicateStrict(op.Predicate)
		if err != nil {
			return err
		}
		predicate = &decoded
	}
	draft.Graph.Edges = append(draft.Graph.Edges, machine.Edge{
		ID:         edgeID,
		FromNodeID: fromNode.ID,
		ToNodeID:   op.To,
		Route:      machine.EdgeRoute(route),
		Priority:   cloneInt64Ptr(op.Priority),
		Predicate:  predicate,
	})
	return nil
}

// validateWorkflowBackEdge enforces the F14 discipline at apply time: a back
// edge must connect the loop's configured latch to the loop header.
func validateWorkflowBackEdge(draft *workflowDraft, fromIndex int, toNodeID string) error {
	_, toNode, err := findWorkflowNode(&draft.Graph, toNodeID)
	if err != nil {
		return err
	}
	if toNode.Type != machine.NodeLoop {
		return fmt.Errorf("back route must target a loop node (F14); got %q", toNode.Type)
	}
	loopConfig, ok := toNode.Config.(machine.LoopConfig)
	if !ok {
		return errors.New("loop node config is not a LoopConfig")
	}
	if loopConfig.LatchNodeID != draft.Graph.Nodes[fromIndex].ID {
		return fmt.Errorf(
			"back route must come from the loop latch %q (F14); condition/other nodes cannot route back directly",
			loopConfig.LatchNodeID)
	}
	return nil
}

// workflowRouteAllowedForNode mirrors the machine route→node pairing so the
// apply op gives immediate feedback; full cardinality stays with the
// validator.
func workflowRouteAllowedForNode(node machine.Node, route string) bool {
	switch route {
	case "success":
		switch node.Type {
		case machine.NodeLead, machine.NodeTransform, machine.NodeJoin, machine.NodeWait:
			return true
		case machine.NodeWorker:
			config, ok := node.Config.(machine.WorkerConfig)
			return ok && config.Kind == machine.WorkerConsult
		}
	case "failure":
		switch node.Type {
		case machine.NodeLead, machine.NodeTransform, machine.NodeJoin, machine.NodeWait:
			return true
		case machine.NodeWorker:
			config, ok := node.Config.(machine.WorkerConfig)
			return ok && config.Kind == machine.WorkerConsult
		case machine.NodeCondition:
			return true
		case machine.NodeHandoff:
			return true
		}
	case "case", "default":
		return node.Type == machine.NodeCondition
	case "branch":
		return node.Type == machine.NodeParallel
	case "join":
		if node.Type == machine.NodeWorker {
			config, ok := node.Config.(machine.WorkerConfig)
			return ok && config.Kind == machine.WorkerDispatch
		}
	case "timeout":
		return node.Type == machine.NodeWait
	case "body", "exit":
		return node.Type == machine.NodeLoop
	}
	return false
}

func applyWorkflowSetEntry(draft *workflowDraft, op *workflowApplyOp) error {
	entry := strings.TrimSpace(op.EntryNodeID)
	if entry == "" {
		return errors.New("set_entry requires an entry_node_id")
	}
	if _, _, err := findWorkflowNode(&draft.Graph, entry); err != nil {
		return err
	}
	draft.Graph.EntryNodeID = entry
	return nil
}

func applyWorkflowSetContract(draft *workflowDraft, op *workflowApplyOp, output bool) error {
	if len(op.Contract) == 0 {
		if output {
			return errors.New("set_output_contract requires a contract object")
		}
		return errors.New("set_input_contract requires a contract object")
	}
	contract, err := decodeOutputContractStrict(op.Contract)
	if err != nil {
		return err
	}
	if output {
		draft.Graph.OutputContract = contract
	} else {
		draft.Graph.InputContract = contract
	}
	return nil
}

func applyWorkflowSetTrigger(draft *workflowDraft, op *workflowApplyOp) error {
	if len(op.Trigger) == 0 {
		return errors.New("set_trigger requires a trigger object")
	}
	trigger, err := decodeTriggerStrict(op.Trigger)
	if err != nil {
		return err
	}
	draft.Trigger = trigger
	return nil
}

func findWorkflowNode(graph *machine.GraphDefinition, nodeID string) (int, machine.Node, error) {
	for i, node := range graph.Nodes {
		if node.ID == nodeID {
			return i, node, nil
		}
	}
	return 0, machine.Node{}, fmt.Errorf("node %q not found", nodeID)
}

// --- tf_wf_validate ---

type workflowValidateJSON struct {
	BuildRunID string               `json:"build_run_id"`
	WorkflowID string               `json:"workflow_id"`
	Version    int                  `json:"version"`
	Valid      bool                 `json:"valid"`
	Errors     []WorkflowProblem    `json:"errors"`
	Warnings   []WorkflowProblem    `json:"warnings"`
	Summary    workflowDraftSummary `json:"summary"`
}

func (d *WorkflowWriteToolsDispatcher) workflowValidate(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveWorkflowCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	draft := d.drafts.get(input.BuildRunID, input.WorkflowID)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for workflow %q; call tf_wf_begin first", input.WorkflowID)), nil
	}
	validation, err := validateWorkflowDraft(ctx, d.deps, d.gate.workspaceID, draft)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	return toolJSON(call.ID, workflowValidateJSON{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
		Version:    draft.Version,
		Valid:      validation.Valid,
		Errors:     validation.Errors,
		Warnings:   validation.Warnings,
		Summary:    d.draftSummary(draft, false),
	})
}

// --- tf_wf_commit ---

type workflowCommitJSON struct {
	BuildRunID string    `json:"build_run_id"`
	WorkflowID string    `json:"workflow_id"`
	Version    int       `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
	Committed  bool      `json:"committed"`
}

func (d *WorkflowWriteToolsDispatcher) workflowCommit(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveWorkflowCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	draft := d.drafts.get(input.BuildRunID, input.WorkflowID)
	if draft == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for workflow %q; call tf_wf_begin first", input.WorkflowID)), nil
	}
	return d.commitWorkflowDraft(ctx, call, input, draft), nil
}

// commitWorkflowDraft validates, strictly re-encodes, and CAS-updates the
// mutable draft. Shared by tf_wf_commit and tf_wf_build so the macro tool
// reuses the exact encode/decode/UpdateDraft path without copying it.
func (d *WorkflowWriteToolsDispatcher) commitWorkflowDraft(
	ctx context.Context,
	call contract.ToolCall,
	input *workflowCallInput,
	draft *workflowDraft,
) *contract.ToolResult {
	validation, err := validateWorkflowDraft(ctx, d.deps, d.gate.workspaceID, draft)
	if err != nil {
		return toolError(call.ID, err.Error())
	}
	if !validation.Valid {
		payload, marshalErr := json.Marshal(workflowValidateJSON{
			BuildRunID: input.BuildRunID,
			WorkflowID: input.WorkflowID,
			Version:    draft.Version,
			Valid:      false,
			Errors:     validation.Errors,
			Warnings:   validation.Warnings,
			Summary:    d.draftSummary(draft, false),
		})
		if marshalErr != nil {
			return toolError(call.ID, fmt.Sprintf("commit rejected; validation failed: %v", marshalErr))
		}
		return toolError(call.ID, string(payload))
	}
	if d.writeDeps.Workflows == nil {
		return toolError(call.ID, "workflow write is unavailable")
	}

	triggerJSON, graphJSON, err := encodeWorkflowDraftJSON(draft.Trigger, draft.Graph)
	if err != nil {
		return toolError(call.ID, fmt.Sprintf("encode draft: %v", err))
	}
	// Strict round trip: the tool's output is the only encoding that may
	// reach the workflow store; the machine decoders must accept it.
	if _, report := machine.DecodeTriggerConfigV1(triggerJSON); report != nil && len(report.Issues) != 0 {
		return toolError(call.ID, "tool-encoded trigger failed the strict decoder")
	}
	if _, report := machine.DecodeGraphDefinitionV1(graphJSON); report != nil && len(report.Issues) != 0 {
		return toolError(call.ID, "tool-encoded graph failed the strict decoder")
	}

	updated, err := d.writeDeps.Workflows.UpdateDraft(
		ctx,
		d.gate.workspaceID,
		input.WorkflowID,
		draft.Version,
		draft.UpdatedAt,
		workflow.DraftInput{
			TriggerConfig:   triggerJSON,
			GraphDefinition: graphJSON,
			CreatedBy:       draft.CreatedBy,
		},
	)
	if err != nil {
		if errors.Is(err, workflow.ErrVersionConflict) {
			return toolError(call.ID, fmt.Sprintf(
				"%v: workflow %q draft changed since begin; call tf_wf_begin again to reload before committing",
				workflow.ErrVersionConflict, input.WorkflowID))
		}
		return toolError(call.ID, fmt.Sprintf("update workflow draft: %v", err))
	}
	d.drafts.delete(input.BuildRunID, input.WorkflowID)
	result, _ := toolJSON(call.ID, workflowCommitJSON{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
		Version:    updated.Version,
		UpdatedAt:  updated.UpdatedAt,
		Committed:  true,
	})
	return result
}

// --- tf_wf_discard ---

func (d *WorkflowWriteToolsDispatcher) workflowDiscard(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	input, err := d.resolveWorkflowCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if d.drafts.get(input.BuildRunID, input.WorkflowID) == nil {
		return toolError(call.ID, fmt.Sprintf("no draft for workflow %q; call tf_wf_begin first", input.WorkflowID)), nil
	}
	d.drafts.delete(input.BuildRunID, input.WorkflowID)
	return toolJSON(call.ID, map[string]any{
		"build_run_id": input.BuildRunID,
		"workflow_id":  input.WorkflowID,
		"discarded":    true,
	})
}

// --- draft summary ---

type workflowDraftSummary struct {
	BuildRunID  string          `json:"build_run_id"`
	WorkflowID  string          `json:"workflow_id"`
	Version     int             `json:"version"`
	TriggerType string          `json:"trigger_type"`
	Trigger     json.RawMessage `json:"trigger,omitempty"`
	Graph       json.RawMessage `json:"graph,omitempty"`
	NodeCount   int             `json:"node_count"`
	EdgeCount   int             `json:"edge_count"`
	HasDraft    bool            `json:"has_draft"`
	Restored    bool            `json:"restored,omitempty"`
}

func (d *WorkflowWriteToolsDispatcher) draftSummary(draft *workflowDraft, restored bool) workflowDraftSummary {
	if draft == nil {
		return workflowDraftSummary{HasDraft: false}
	}
	triggerJSON, graphJSON, _ := encodeWorkflowDraftJSON(draft.Trigger, draft.Graph)
	return workflowDraftSummary{
		BuildRunID:  draft.BuildRunID,
		WorkflowID:  draft.WorkflowID,
		Version:     draft.Version,
		TriggerType: string(draft.Trigger.Type),
		Trigger:     triggerJSON,
		Graph:       graphJSON,
		NodeCount:   len(draft.Graph.Nodes),
		EdgeCount:   len(draft.Graph.Edges),
		HasDraft:    true,
		Restored:    restored,
	}
}

// --- tool schemas ---

var workflowIDOnlySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"workflow_id": {"type": "string", "description": "Target workflow ID; must sit inside the build authorization scope"}
	},
	"required": ["workflow_id"],
	"additionalProperties": false
}`)

var workflowBeginSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"workflow_id": {"type": "string", "description": "Target workflow ID; for create mode this is the new workflow ID"},
		"create": {
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Workflow display name (required for create)"},
				"team_id": {"type": "string", "description": "Owning team ID (required for create)"},
				"description": {"type": "string"}
			},
			"required": ["name", "team_id"],
			"additionalProperties": false
		}
	},
	"required": ["workflow_id"],
	"additionalProperties": false
}`)

var workflowApplySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"workflow_id": {"type": "string"},
		"ops": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"seq": {"type": "integer", "description": "Operation sequence number for batch error reporting"},
					"op": {"type": "string", "enum": ["add_node", "update_node_config", "remove_node", "set_node_input", "set_node_output", "connect", "set_entry", "set_input_contract", "set_output_contract", "set_trigger"]},
					"node_id": {"type": "string", "description": "add/update/remove/set node target"},
					"type": {"type": "string", "description": "add_node only: lead | worker | transform | condition | parallel | join | wait | loop | deliver | handoff"},
					"label": {"type": "string", "description": "add_node only: optional display label"},
					"config": {"type": "object", "description": "add_node / update_node_config: typed node config per machine/types.go"},
					"edge_id": {"type": "string", "description": "connect only: optional edge ID (auto-generated when absent)"},
					"from": {"type": "string", "description": "connect only: source node ID"},
					"to": {"type": "string", "description": "connect only: target node ID"},
					"route": {"type": "string", "description": "connect only: success | failure | case | default | branch | join | timeout | body | exit | back"},
					"priority": {"type": "integer", "description": "connect only: condition case priority"},
					"predicate": {"type": "object", "description": "connect only: condition case predicate {left, operator, right?}"},
					"name": {"type": "string", "description": "set_node_input only: input binding name"},
					"expected_type": {"type": "string", "description": "set_node_input only: text | json | boolean | number"},
					"value": {"type": "object", "description": "set_node_input only: ValueRef {source, path, node_id, value, iteration, default}"},
					"output": {"type": "object", "description": "set_node_output only: {type, schema?}"},
					"contract": {"type": "object", "description": "set_input_contract / set_output_contract: {type, schema?}"},
					"entry_node_id": {"type": "string", "description": "set_entry only: entry node ID"},
					"trigger": {"type": "object", "description": "set_trigger only: {schema_version, type, config, delivery?}"}
				},
				"required": ["seq", "op"],
				"additionalProperties": false
			}
		}
	},
	"required": ["workflow_id", "ops"],
	"additionalProperties": false
}`)

// emptyTriggerJSON and emptyGraphJSON seed a brand-new workflow's v1 draft.
// The empty graph is structurally decodable (entry empty, no nodes); the
// validator reports the missing entry until ops build the topology.
var (
	emptyTriggerJSON = json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	emptyGraphJSON   = json.RawMessage(`{
		"schema_version":1,
		"entry_node_id":"",
		"input_contract":{"type":"text"},
		"output_contract":{"type":"text"},
		"nodes":[],
		"edges":[]
	}`)
)
