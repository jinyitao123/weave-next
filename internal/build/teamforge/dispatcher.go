// Package teamforge implements the in-process typed tools the meta-team uses
// during discovery, execution, and assembly phases (plan §10.1-§10.2). Read
// tools are discovery-only before a TeamBuildRun exists, then carry a
// build_run_id and are gated on the persisted control record; write tools
// additionally carry a BuildAuthorizationReceipt
// minted by AuthorizeBuildRun and validated per call. Every tool call writes
// an audit row on every outcome. All tools return JSON strings inside
// ToolResult.Content; failures are reported as IsError results rather than
// Go errors so the LLM can self-heal.
package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	// ToolListAgents lists the workspace agents visible to the registry.
	ToolListAgents = "tf_list_agents"
	// ToolGetAgentVersion reads one exact immutable AgentRecord version.
	ToolGetAgentVersion = "tf_get_agent_version"
	// ToolGetTeam reads one team, its complete roster, and workflow summaries.
	ToolGetTeam = "tf_get_team"
	// ToolListTeams lists workspace teams with minimal fields for duplicate
	// checks and optimize-target selection. Built-in "__" platform teams are
	// excluded at the query layer.
	ToolListTeams = "tf_list_teams"
	// ToolGetDispatchRules reads one team's free-collaboration dispatch rules.
	ToolGetDispatchRules = "tf_get_dispatch_rules"
	// ToolGetWorkflow reads one workflow, its version list, and version contents.
	ToolGetWorkflow = "tf_get_workflow"
	// ToolListCapabilities lists skills, MCP servers, providers, and runtimes.
	ToolListCapabilities = "tf_list_capabilities"
	// ToolGetRunEvidence reads run/task/deliverable/usage evidence.
	ToolGetRunEvidence = "tf_get_run_evidence"
	// ToolGetBuildContext reads the exact persisted control record bound to
	// the current conversation so workers never guess authorized asset IDs.
	ToolGetBuildContext = "tf_get_build_context"
	// ToolGetHistoricalBuildContext reads one immutable prior BuildRun from
	// the same conversation without changing the current run binding.
	ToolGetHistoricalBuildContext = "tf_get_historical_build_context"

	// maxAuditDetailRunes mirrors the platform audit-detail truncation
	// convention (internal/mcphost/auditing.go).
	maxAuditDetailRunes = 200
)

// Evidence domain identifiers used by the tf_get_run_evidence call budget.
// The budget counts calls per domain (not per argument set), so rephrasing a
// filter never resets it.
const (
	evidenceDomainRuns         = "runs"
	evidenceDomainTasks        = "tasks"
	evidenceDomainDeliverables = "deliverables"
)

const (
	// evidenceDomainCallBudget caps how many tf_get_run_evidence calls may
	// touch one evidence domain within a single dispatcher lifetime (one chat
	// turn). Three is enough for: unfiltered domain scan, one refined filter,
	// one follow-up; past that the model is re-searching, not discovering.
	evidenceDomainCallBudget = 3
	// evidenceTotalCallBudget caps tf_get_run_evidence calls across all
	// domains in one turn, so cycling across domains cannot bypass the
	// per-domain budget for longer than a few extra calls.
	evidenceTotalCallBudget = 10
)

// Sentinel gate errors. Errors.Is can distinguish each rejection class.
var (
	// ErrBuildRunNotFound reports that no build run exists under the
	// dispatcher's workspace for the given build_run_id.
	ErrBuildRunNotFound = errors.New("build run not found")
	// ErrBuildRunMismatch reports a read call whose explicit build_run_id
	// contradicts the build run bound to the dispatcher at construction
	// time. Context passing is the platform's job: a call may omit the id
	// (the bound run fills it), but a provided id that disagrees with the
	// binding is rejected.
	ErrBuildRunMismatch = errors.New("build_run_id does not match the dispatcher-bound build run")
	// ErrBuildRunTerminal reports a build run in a terminal state
	// (passed, blocked, or cancelled); reads are rejected.
	ErrBuildRunTerminal = errors.New("build run is in a terminal state (passed, blocked, or cancelled)")
	// ErrWorkspaceMismatch reports a build run bound to another workspace.
	ErrWorkspaceMismatch = errors.New("build run belongs to another workspace")
	// ErrConversationMismatch reports a historical BuildRun outside the
	// conversation currently being replanned.
	ErrConversationMismatch = errors.New("build run belongs to another conversation")
	// ErrEvidenceBudgetExhausted reports a tf_get_run_evidence call that
	// exceeds the per-domain or total evidence call budget. The model must
	// converge (submit brief, ask the user, or report blocked) instead of
	// re-searching the same evidence domain.
	ErrEvidenceBudgetExhausted = errors.New("evidence_budget_exhausted")
)

// BuildRunReader is the narrow read surface of the team build run store.
// *teambuild.Store satisfies it.
type BuildRunReader interface {
	GetBuildRun(ctx context.Context, workspaceID, buildRunID string) (teambuild.TeamBuildRun, error)
}

type activeBuildRunByConversationReader interface {
	GetActiveBuildRunByConversation(ctx context.Context, workspaceID, conversationID string) (teambuild.TeamBuildRun, error)
}

type blueprintRevisionReader interface {
	GetLatestBlueprintRevision(ctx context.Context, workspaceID, buildRunID string) (teambuild.BlueprintRevision, error)
}

// AuditRecorder records one workspace-scoped tool invocation. *audit.Store
// satisfies it; tests use fakes.
type AuditRecorder interface {
	Record(ctx context.Context, workspaceID, agent, tool, status, detail string) error
}

// ReadToolsDispatcher exposes the teamforge read tools. The workspace and
// agent identity are fixed at construction time (taskstatus.go identity
// closure pattern); every dispatch re-checks the build run gate.
type ReadToolsDispatcher struct {
	workspaceID string
	agent       string
	// discovery is true only before this conversation has any TeamBuildRun.
	// It permits workspace-scoped reads with no build_run_id and rejects any
	// caller-supplied run id; writes are exposed by separate dispatchers only.
	discovery bool
	// boundRunID is the build run fixed at construction time (resolved from
	// the conversation's active run by the api wiring, or the round's run by
	// teamorch). Calls may omit build_run_id; the dispatcher fills it from
	// this binding, and an explicitly provided id must agree with it.
	boundRunID string
	// followConversationID is set only for the replanning bridge. The same
	// chat turn may create a replacement planning run after first reading the
	// blocked run, so subsequent context reads must follow that active run.
	followConversationID string
	runs                 BuildRunReader
	audit                AuditRecorder
	deps                 Deps
	tools                []contract.ToolDef
	// evidenceDomainCalls counts tf_get_run_evidence dispatches per evidence
	// domain, and evidenceCalls the total across domains. Both live exactly
	// as long as the dispatcher (one chat turn), so the budget is per-turn
	// and immune to rephrased arguments.
	evidenceDomainCalls map[string]int
	evidenceCalls       int
	// terminalContext permits only tf_get_build_context to read an immutable
	// blocked run while the user is explicitly replanning. All other terminal
	// reads remain fail-closed and the old run remains unmodifiable.
	terminalContext bool
}

// NewReadTools creates the read-tools dispatcher for one workspace and one
// calling agent. boundRunID is the build run authorized at construction time
// (the conversation's active run or the round's run); a call that omits
// build_run_id is bound to it, and an explicitly provided id must match it.
// runs is the production *teambuild.Store (or a fake); audit is the
// production *audit.Store (or a fake); deps aggregates the narrow read-only
// store interfaces defined in this package.
func NewReadTools(
	workspaceID, agentName, boundRunID string,
	runs BuildRunReader,
	audit AuditRecorder,
	deps Deps,
) *ReadToolsDispatcher {
	d := &ReadToolsDispatcher{
		workspaceID: workspaceID,
		agent:       agentName,
		boundRunID:  boundRunID,
		runs:        runs,
		audit:       audit,
		deps:        deps,
	}
	d.tools = []contract.ToolDef{
		{
			Name: ToolGetBuildContext,
			Description: "读取当前会话绑定的 TeamBuildRun 建设上下文，包含精确 allowed_assets、允许本次构建创建的 pending_create_assets、BuildBrief 与 EvaluationContract。" +
				"Read the exact TeamBuildRun context bound to this conversation, including allowed_assets, pending_create_assets that this build may create, BuildBrief, and EvaluationContract. " +
				"Call this first whenever it is available; use its stable IDs and never guess asset names or IDs.",
			InputSchema: buildRunOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolListAgents,
			Description: "列出本 workspace 的 Agent（含当前 version 与 archived 标注）。" +
				"List the workspace agents (with current version and archived annotation). " +
				"Returns JSON: {agents: [{name, id, display_name, role, version, team_id, engine, runtime_id, model, archived, updated_at}], total}.",
			InputSchema: buildRunOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolGetAgentVersion,
			Description: "按精确版本读取一个不可变的 AgentRecord（绝不 fallback 到 latest）。" +
				"Read one exact immutable AgentRecord version; never falls back to latest. " +
				"Returns the full agent record JSON.",
			InputSchema: getAgentVersionSchema,
			ReadOnly:    true,
		},
		{
			Name: ToolGetTeam,
			Description: "读取一个 Team、完整 Roster（含 duty/when_to_use/context_instruction/allowed_kinds/default_kind/result_requirement/enabled）及工作流摘要。" +
				"Read one team with its complete roster and workflow summaries. Returns JSON: {team, roster, roster_count, workflows: [{workflow_id, name, status, published_version}]}.",
			InputSchema: teamIDOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolListTeams,
			Description: "列出本 workspace 的团队（最小字段）。用途仅限：create 模式的重名/近似团队重复建设检查、optimize 模式选择真实优化目标。" +
				"平台内置团队（__ 前缀）已被排除。create 模式下发现相似团队时，禁止把任务自动改为交给现有团队或复用其 team_id 冒充新建目标；相似团队只是返回给你判断的事实提示。" +
				"List workspace teams with minimal fields for duplicate-construction checks (create) and real target selection (optimize). " +
				"Built-in __ platform teams are excluded. In create mode a similar existing team is only a fact to report, never a reason to reuse its team_id or redirect the task. " +
				"Returns JSON: {teams: [{id, name, objective, status, lead_avatar_id}], total}.",
			InputSchema: buildRunOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolGetDispatchRules,
			Description: "读取一个 Team 的自由协作派工规则。" +
				"Read one team's free-collaboration dispatch rules. " +
				"Returns JSON: {team_id, leg_timeout_sec, group_deadline_sec, quorum}.",
			InputSchema: teamIDOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolGetWorkflow,
			Description: "读取一个 TeamWorkflow、其全部版本列表以及 draft/published 版本内容；若误传团队 ID 或名称，则返回该团队的工作流列表和正确 workflow_id；若 optimize 团队尚无工作流且该 ID 是冻结范围允许的新建目标，则返回 status=pending_create（不是错误），应继续调用 tf_blueprint_plan。" +
				"Read one TeamWorkflow with its version list and draft/published version contents. " +
				"If given a team ID or name, returns that team's workflow summaries and guidance to retry with workflow_id. " +
				"For the one frozen optimize target that does not exist yet, returns status=pending_create (not an error); continue by calling tf_blueprint_plan. " +
				"Returns JSON: {status, workflow, versions: [{version, status, trigger_config, graph_definition, ...}], requested_version?, creation_allowed?, workflows?, guidance?}.",
			InputSchema: getWorkflowSchema,
			ReadOnly:    true,
		},
		{
			Name: ToolListCapabilities,
			Description: "列出本 workspace 的 Skill、MCP server、Provider、Runtime，以及 Agent 图与引擎模型能力矩阵。规划前必须按 agent_graph_policy 与 agent_model_policy 排除不可执行组合。" +
				"List skills, MCP servers, providers, runtimes, and the agent graph/model execution policies. Planning must obey agent_graph_policy and agent_model_policy. " +
				"Returns JSON: {skills, mcp_servers, providers, runtimes, agent_graph_policy, agent_model_policy}.",
			InputSchema: buildRunOnlySchema,
			ReadOnly:    true,
		},
		{
			Name: ToolGetRunEvidence,
			Description: "读取 run/task/deliverable/usage 的有界摘要证据。run_id 必须是 rt- 前缀的运行 ID 或 UUID，不能传团队名或 BuildRun ID；TeamBuildRun 阶段必须提供精确 run_id/task_id/workflow_id/agent_id/deliverable_id 之一，禁止全工作区扫描；大字段仅返回短预览。" +
				"每个证据段带 status：ok=有数据，not_found=精确过滤无匹配（该事实不存在，标为未知并收敛），exhaustive=该域全部内容已返回（再查也是这些，禁止重查）。" +
				"每个证据域每轮最多查询 " + fmt.Sprint(evidenceDomainCallBudget) + " 次，超限返回 evidence_budget_exhausted；届时必须停止检索并收敛：调用 tf_submit_brief、向用户提问缺失信息，或输出 status=blocked。" +
				"Read bounded evidence summaries. During a bound TeamBuildRun, provide an exact run_id, task_id, workflow_id, agent_id, or deliverable_id; workspace-wide scans are rejected. Large fields are short previews. " +
				"Each section carries a status: ok=has data, not_found=exact filter matched nothing (the fact does not exist; mark unknown and converge), exhaustive=the entire domain has been returned (re-querying yields the same data; do not re-search). " +
				"Each evidence domain accepts at most " + fmt.Sprint(evidenceDomainCallBudget) + " queries per turn; beyond that the call returns evidence_budget_exhausted and you must stop searching and converge: call tf_submit_brief, ask the user for the missing facts, or output status=blocked. " +
				"Returns JSON: {run_id, run, runs, runs_status, tasks, tasks_status, deliverables, deliverables_status, usage, limit, truncated}.",
			InputSchema: getRunEvidenceSchema,
			ReadOnly:    true,
		},
	}
	return d
}

// NewDiscoveryReadTools creates the no-run read-only surface used before the
// architect has produced drafts from which a planning TeamBuildRun can be
// created. A discovery dispatcher never accepts a caller-supplied run id and
// never exposes any write tools.
func NewDiscoveryReadTools(
	workspaceID, agentName string,
	audit AuditRecorder,
	deps Deps,
) *ReadToolsDispatcher {
	d := NewReadTools(workspaceID, agentName, "", nil, audit, deps)
	d.discovery = true
	// There is no persisted TeamBuildRun to read during initial discovery.
	// Keep the original seven workspace discovery tools and expose build
	// context only after the conversation is bound to a planning run.
	d.tools = d.tools[1:]
	return d
}

// NewPlanningReadTools creates the read-only surface for a persisted planning
// TeamBuildRun. Runtime evidence is intentionally excluded here: no team run
// exists yet, and exposing tf_get_run_evidence causes the architect to mistake
// BuildRun ids for runtime run ids instead of producing a blueprint revision.
func NewPlanningReadTools(
	workspaceID, agentName, boundRunID string,
	runs BuildRunReader,
	audit AuditRecorder,
	deps Deps,
) *ReadToolsDispatcher {
	d := NewReadTools(workspaceID, agentName, boundRunID, runs, audit, deps)
	d.tools = filterToolDefs(d.tools, ToolGetRunEvidence)
	return d
}

// NewReplanningReadTools exposes only the immutable context of one blocked
// run so the architect can submit a corrected brief as a new BuildRun.
func NewReplanningReadTools(
	workspaceID, conversationID, agentName, boundRunID string,
	runs BuildRunReader,
	audit AuditRecorder,
	deps Deps,
) *ReadToolsDispatcher {
	d := NewReadTools(workspaceID, agentName, boundRunID, runs, audit, deps)
	d.terminalContext = true
	d.followConversationID = conversationID
	d.tools = filterOnlyToolDefs(d.tools, ToolGetBuildContext)
	d.tools = append(d.tools, contract.ToolDef{
		Name: ToolGetHistoricalBuildContext,
		Description: "仅在返工规划时，按精确 build_run_id 只读获取同一会话中的历史 TeamBuildRun，供原样复用此前冻结的 BuildBrief、EvaluationContract 与 Blueprint；不会改变当前 BuildRun 绑定，也不能据此写入历史运行。" +
			"During replanning only, read one immutable historical TeamBuildRun from this same conversation by exact build_run_id, so a previously frozen BuildBrief, EvaluationContract, and Blueprint can be reused exactly. " +
			"This never changes the current BuildRun binding and grants no authority to mutate the historical run.",
		InputSchema: historicalBuildRunSchema,
		ReadOnly:    true,
	})
	return d
}

func filterToolDefs(tools []contract.ToolDef, excludedName string) []contract.ToolDef {
	filtered := tools[:0]
	for _, tool := range tools {
		if tool.Name != excludedName {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func filterOnlyToolDefs(tools []contract.ToolDef, includedName string) []contract.ToolDef {
	filtered := tools[:0]
	for _, tool := range tools {
		if tool.Name == includedName {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

// ListTools returns the read tools available in the dispatcher's phase.
func (d *ReadToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

// Dispatch gates the call on the build run control record, executes the
// matching read tool, and records the outcome in the audit trail. Every
// failure is an IsError result, never a Go error.
func (d *ReadToolsDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (result *contract.ToolResult, err error) {
	defer func() {
		d.recordAudit(ctx, call, result, err)
	}()

	if !d.toolRegistered(call.Name) {
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	if call.Name == ToolGetHistoricalBuildContext {
		return d.getHistoricalBuildContext(ctx, call)
	}
	var buildRunID string
	if d.discovery {
		var err error
		buildRunID, err = buildRunIDFromArgs(call.Args)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		if buildRunID != "" {
			return toolError(call.ID, "build_run_id is not accepted in discovery mode"), nil
		}
	} else {
		var err error
		buildRunID, err = d.resolveBuildRunIDForContext(ctx, call.Args)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		if err := d.authorizeBuildRun(ctx, buildRunID, call.Name); err != nil {
			return toolError(call.ID, err.Error()), nil
		}
	}

	switch call.Name {
	case ToolGetBuildContext:
		return d.getBuildContext(ctx, call, buildRunID)
	case ToolListAgents:
		return d.listAgents(ctx, call)
	case ToolGetAgentVersion:
		return d.getAgentVersion(ctx, call)
	case ToolGetTeam:
		return d.getTeam(ctx, call)
	case ToolListTeams:
		return d.listTeams(ctx, call)
	case ToolGetDispatchRules:
		return d.getDispatchRules(ctx, call)
	case ToolGetWorkflow:
		return d.getWorkflow(ctx, call)
	case ToolListCapabilities:
		return d.listCapabilities(ctx, call)
	case ToolGetRunEvidence:
		return d.getRunEvidence(ctx, call)
	default:
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

func (d *ReadToolsDispatcher) getHistoricalBuildContext(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	if strings.TrimSpace(d.followConversationID) == "" {
		return toolError(call.ID, "historical build context is available only while replanning"), nil
	}
	buildRunID, err := buildRunIDFromArgs(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if buildRunID == "" {
		return toolError(call.ID, "build_run_id is required"), nil
	}
	if d.runs == nil {
		return toolError(call.ID, "build run gate unavailable"), nil
	}
	run, err := d.runs.GetBuildRun(ctx, d.workspaceID, buildRunID)
	if err != nil {
		if errors.Is(err, teambuild.ErrBuildRunNotFound) {
			return toolError(call.ID, fmt.Sprintf("%s: build_run_id %q", ErrBuildRunNotFound, buildRunID)), nil
		}
		return toolError(call.ID, fmt.Sprintf("check historical build run %q: %v", buildRunID, err)), nil
	}
	if run.WorkspaceID != d.workspaceID {
		return toolError(call.ID, fmt.Sprintf("%s: build_run_id %q", ErrWorkspaceMismatch, buildRunID)), nil
	}
	if run.ConversationID != d.followConversationID {
		return toolError(call.ID, fmt.Sprintf("%s: build_run_id %q", ErrConversationMismatch, buildRunID)), nil
	}
	return d.getBuildContext(ctx, call, buildRunID)
}

type buildContextJSON struct {
	BuildRunID          string                       `json:"build_run_id"`
	Mode                string                       `json:"mode"`
	Status              string                       `json:"status"`
	AllowedAssets       teambuild.AssetScope         `json:"allowed_assets"`
	PendingCreateAssets []teambuild.AssetRef         `json:"pending_create_assets,omitempty"`
	PlanningGuidance    string                       `json:"planning_guidance,omitempty"`
	Brief               teambuild.BuildBrief         `json:"build_brief"`
	Contract            teambuild.EvaluationContract `json:"evaluation_contract"`
	BriefHash           string                       `json:"brief_hash,omitempty"`
	ContractHash        string                       `json:"contract_hash,omitempty"`
	LatestRevisionNo    int                          `json:"latest_blueprint_revision_no,omitempty"`
	LatestWorkflowMode  string                       `json:"latest_workflow_mode,omitempty"`
	LatestBlueprint     *teambuild.TeamBlueprintV1   `json:"latest_blueprint,omitempty"`
}

func (d *ReadToolsDispatcher) getBuildContext(
	ctx context.Context,
	call contract.ToolCall,
	buildRunID string,
) (*contract.ToolResult, error) {
	run, err := d.runs.GetBuildRun(ctx, d.workspaceID, buildRunID)
	if err != nil {
		return toolError(call.ID, ErrBuildRunNotFound.Error()), nil
	}
	pendingCreates := pendingCreateAssets(run)
	planningGuidance := ""
	if len(pendingCreates) != 0 {
		planningGuidance = "pending_create_assets are authorized future targets, not missing evidence; include them in the TeamBlueprint and call tf_blueprint_plan"
	}
	var latestBlueprint *teambuild.TeamBlueprintV1
	var latestRevisionNo int
	var latestWorkflowMode string
	if revisions, ok := d.runs.(blueprintRevisionReader); ok {
		if revision, revisionErr := revisions.GetLatestBlueprintRevision(ctx, d.workspaceID, buildRunID); revisionErr == nil {
			var blueprint teambuild.TeamBlueprintV1
			if decodeErr := json.Unmarshal(revision.BlueprintJSON, &blueprint); decodeErr == nil {
				latestBlueprint = &blueprint
				latestRevisionNo = revision.RevisionNo
				latestWorkflowMode = revision.WorkflowMode
			}
		}
	}
	if run.Status == teambuild.StatusBlocked {
		planningGuidance = "this BuildRun is immutable and blocked; reuse or correct its frozen brief, evaluation contract, and latest_blueprint roster, then call tf_submit_brief to create a new planning BuildRun; never submit planning tools against the blocked run"
	}
	return toolJSON(call.ID, buildContextJSON{
		BuildRunID: run.BuildRunID, Mode: run.Mode, Status: run.Status,
		AllowedAssets: run.AssetScope, Brief: run.Brief, Contract: run.Contract,
		PendingCreateAssets: pendingCreates, PlanningGuidance: planningGuidance,
		BriefHash: run.BriefHash, ContractHash: run.ContractHash,
		LatestRevisionNo: latestRevisionNo, LatestWorkflowMode: latestWorkflowMode,
		LatestBlueprint: latestBlueprint,
	})
}

func pendingCreateAssets(run teambuild.TeamBuildRun) []teambuild.AssetRef {
	if run.Mode != teambuild.ModeOptimize || strings.TrimSpace(run.Brief.TeamID) == "" {
		return nil
	}
	ref := teambuild.AssetRef{
		Kind: "workflow",
		ID:   teambuild.FirstOptimizeWorkflowID(run.Brief.TeamID),
	}
	if !run.AssetScope.Contains(ref) {
		return nil
	}
	return []teambuild.AssetRef{ref}
}

// chargeEvidenceBudget enforces the per-domain and total tf_get_run_evidence
// call budgets for this dispatcher's lifetime. A call is charged for the
// primary domain implied by its filters before any store access happens;
// global discovery charges every available domain. Exceeding either budget
// short-circuits with an evidence_budget_exhausted IsError result so no
// evidence query executes. Argument wording never resets the budget: only the
// requested domain matters.
func (d *ReadToolsDispatcher) chargeEvidenceBudget(call contract.ToolCall, domains []string) (*contract.ToolResult, error) {
	if d.evidenceDomainCalls == nil {
		d.evidenceDomainCalls = map[string]int{}
	}
	if d.evidenceCalls >= evidenceTotalCallBudget {
		return evidenceBudgetRejection(call, "total"), nil
	}
	for _, domain := range domains {
		if d.evidenceDomainCalls[domain] >= evidenceDomainCallBudget {
			return evidenceBudgetRejection(call, domain), nil
		}
	}
	d.evidenceCalls++
	for _, domain := range domains {
		d.evidenceDomainCalls[domain]++
	}
	return nil, nil
}

// evidenceBudgetRejection renders the deterministic rejection the model
// receives when an evidence budget is exhausted. It is an IsError result so
// a model that only scans for failure still reads it as a stop signal; the
// guidance names the three legal convergence exits so the turn can finish
// without more evidence.
func evidenceBudgetRejection(call contract.ToolCall, domain string) *contract.ToolResult {
	content, err := json.Marshal(map[string]any{
		"error":  ErrEvidenceBudgetExhausted.Error(),
		"domain": domain,
		"guidance": "该证据域在本轮的查询预算已用尽，继续检索不会得到新信息。停止重复查询并立即收敛为三者之一：" +
			"1) 事实足够时调用 tf_submit_brief 提交 brief 与 contract；" +
			"2) 缺少必要信息时在最终输出 status=needs_clarification 并向用户列出具体问题；" +
			"3) 无法继续时在最终输出 status=blocked 并给出 blocking_reasons。" +
			" The evidence budget for this domain is exhausted for this turn; re-querying yields no new information. " +
			"Stop searching and converge now: call tf_submit_brief when facts suffice, finish with status=needs_clarification listing concrete questions for the user, or finish with status=blocked and blocking_reasons.",
	})
	if err != nil {
		return toolError(call.ID, "failed to encode evidence budget rejection: "+err.Error())
	}
	return &contract.ToolResult{
		CallID:   call.ID,
		Content:  string(content),
		IsError:  true,
		ToolName: call.Name,
	}
}

func (d *ReadToolsDispatcher) toolRegistered(name string) bool {
	for _, tool := range d.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// authorizeBuildRun enforces the shared minimum gate for discovery and
// execution phase reads: the run exists, belongs to this workspace, and is
// not in a terminal state (passed, blocked, cancelled).
func (d *ReadToolsDispatcher) authorizeBuildRun(ctx context.Context, buildRunID, toolName string) error {
	if d.runs == nil {
		return errors.New("build run gate unavailable")
	}
	run, err := d.runs.GetBuildRun(ctx, d.workspaceID, buildRunID)
	if err != nil {
		if errors.Is(err, teambuild.ErrBuildRunNotFound) {
			return fmt.Errorf("%w: build_run_id %q", ErrBuildRunNotFound, buildRunID)
		}
		return fmt.Errorf("check build run %q: %w", buildRunID, err)
	}
	if run.WorkspaceID != d.workspaceID {
		return fmt.Errorf("%w: build_run_id %q", ErrWorkspaceMismatch, buildRunID)
	}
	if isTerminalBuildRunStatus(run.Status) {
		if d.terminalContext && run.Status == teambuild.StatusBlocked && toolName == ToolGetBuildContext {
			return nil
		}
		return fmt.Errorf("%w: build_run_id %q status %q", ErrBuildRunTerminal, buildRunID, run.Status)
	}
	return nil
}

func isTerminalBuildRunStatus(status string) bool {
	switch status {
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled:
		return true
	default:
		return false
	}
}

// recordAudit writes one best-effort audit row for every dispatch outcome
// (success and rejection alike). Detail follows the platform convention:
// status ok|error, redacted raw args, 200-rune truncation.
func (d *ReadToolsDispatcher) recordAudit(
	ctx context.Context,
	call contract.ToolCall,
	result *contract.ToolResult,
	err error,
) {
	recordAudit(ctx, d.audit, d.workspaceID, d.agent, call, result, err, d.boundRunID)
}

func joinAuditDetail(annotation, detail string) string {
	if annotation == "" {
		return detail
	}
	if detail == "" || detail == "ok" {
		return annotation
	}
	return annotation + "; " + detail
}

func sanitizeAuditDetail(detail, rawArgs string) string {
	if rawArgs != "" {
		detail = strings.ReplaceAll(detail, rawArgs, "[redacted]")
	}
	runes := []rune(detail)
	if len(runes) > maxAuditDetailRunes {
		return string(runes[:maxAuditDetailRunes])
	}
	return detail
}

// buildRunIDFromArgs extracts build_run_id from a tool call. An omitted id
// returns "" (not an error) so the caller can fill it from the dispatcher's
// construction-time binding; malformed input still errors.
func buildRunIDFromArgs(raw string) (string, error) {
	var input struct {
		BuildRunID string `json:"build_run_id"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	return strings.TrimSpace(input.BuildRunID), nil
}

// resolveBuildRunID returns the call's build_run_id when the caller provided
// one, or the dispatcher's construction-bound run id when the call omits it.
// A provided id that contradicts the bound run is rejected; a dispatcher
// without a binding (zero-value construction) keeps build_run_id required so
// the fail-closed gate never guesses a run.
func (d *ReadToolsDispatcher) resolveBuildRunID(rawArgs string) (string, error) {
	runID, err := buildRunIDFromArgs(rawArgs)
	if err != nil {
		return "", err
	}
	if runID == "" {
		if d.boundRunID == "" {
			return "", errors.New("build_run_id is required")
		}
		return d.boundRunID, nil
	}
	if d.boundRunID != "" && runID != d.boundRunID {
		return "", fmt.Errorf(
			"%w: call carries %q, dispatcher is bound to %q",
			ErrBuildRunMismatch, runID, d.boundRunID,
		)
	}
	return runID, nil
}

func (d *ReadToolsDispatcher) resolveBuildRunIDForContext(ctx context.Context, rawArgs string) (string, error) {
	if strings.TrimSpace(d.followConversationID) == "" {
		return d.resolveBuildRunID(rawArgs)
	}
	reader, ok := d.runs.(activeBuildRunByConversationReader)
	if !ok {
		return "", errors.New("active build run lookup unavailable for replanning context")
	}
	effectiveBound := d.boundRunID
	active, err := reader.GetActiveBuildRunByConversation(ctx, d.workspaceID, d.followConversationID)
	if err == nil {
		effectiveBound = active.BuildRunID
	} else if !errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return "", fmt.Errorf("check active replanning build run: %w", err)
	}
	runID, err := buildRunIDFromArgs(rawArgs)
	if err != nil {
		return "", err
	}
	if runID == "" {
		return effectiveBound, nil
	}
	if runID != effectiveBound {
		return "", fmt.Errorf(
			"%w: call carries %q, current conversation run is %q",
			ErrBuildRunMismatch, runID, effectiveBound,
		)
	}
	return runID, nil
}

func toolJSON(callID string, payload any) (*contract.ToolResult, error) {
	content, err := json.Marshal(payload)
	if err != nil {
		return toolError(callID, "failed to encode tool result: "+err.Error()), nil
	}
	return &contract.ToolResult{CallID: callID, Content: string(content)}, nil
}

var _ contract.ToolDispatcher = (*ReadToolsDispatcher)(nil)

var buildRunOnlySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run（br-...）。Optional: omit to auto-bind the authorized build run."}
	}
}`)

var historicalBuildRunSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "同一会话内要只读复用的历史 BuildRun 精确 ID（br-...）。Exact historical BuildRun ID from this conversation."}
	},
	"required": ["build_run_id"]
}`)

var getAgentVersionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"agent_id": {"type": "string", "description": "Stable agent ID returned by tf_list_agents"},
		"version": {"type": "integer", "description": "Exact immutable version; never falls back to latest"}
	},
	"required": ["agent_id", "version"]
}`)

var teamIDOnlySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"team_id": {"type": "string", "description": "Workspace team ID"}
	},
	"required": ["team_id"]
}`)

var getWorkflowSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"workflow_id": {"type": "string", "description": "Workspace workflow ID"},
		"version": {"type": "integer", "description": "Optional exact version to include in the response"}
	},
	"required": ["workflow_id"]
}`)

var getRunEvidenceSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"run_id": {"type": "string", "description": "精确运行 ID，必须为 rt-... 或 UUID；团队信息请用 tf_get_team/tf_get_agent_version。Exact runtime run ID; must be rt-... or a UUID."},
		"task_id": {"type": "string", "description": "Optional exact task ID"},
		"workflow_id": {"type": "string", "description": "Optional exact workflow ID; returns only matching task evidence"},
		"agent_id": {"type": "string", "description": "Optional exact stable agent ID; returns only matching task evidence"},
		"deliverable_id": {"type": "string", "description": "Optional exact deliverable ID"},
		"limit": {"type": "integer", "description": "Max evidence items per section; default 10, clamped to 50"}
	}
}`)

// WriteToolsDispatcher exposes the agent-assembly write tools (plan §10.2.1).
// The workspace, calling agent, and BuildAuthorizationReceipt are fixed at
// construction time; every dispatch re-validates the target asset against
// the receipt, rejects out-of-whitelist parameters, validates the assembled
// record, and writes one immutable version via AgentRegistry.PutTx.
type WriteToolsDispatcher struct {
	gate  *WriteGate
	tools []contract.ToolDef
}

// Receipt exposes the dispatcher's bound BuildAuthorizationReceipt for
// tests and audit tooling. The receipt itself remains private to the store
// that minted it; this accessor only surfaces the bound credential.
func (d *WriteToolsDispatcher) Receipt() teambuild.BuildAuthorizationReceipt {
	return d.gate.receipt
}

// NewWriteTools creates the write-tools dispatcher for one workspace, one
// calling agent, and one build authorization receipt. validator is the
// production *teambuild.Store (or a fake); audit is the production
// *audit.Store (or a fake); deps aggregates the narrow write interfaces
// defined in deps.go.
func NewWriteTools(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps WriteDeps,
) *WriteToolsDispatcher {
	d := &WriteToolsDispatcher{
		gate: newWriteGate(workspaceID, agentName, receipt, validator, audit, deps),
	}
	d.tools = []contract.ToolDef{
		{
			Name: ToolCreateAgent,
			Description: "创建/装配一个新 Agent（创建即不可变 v1）。role 必填 worker/avatar；" +
				"装配字段全量可选且只允许白名单字段；model 必须解析到本 workspace 的 Provider revision；" +
				"name 必须落在本次建设任务授权范围内。Create and assemble a new agent as immutable v1. " +
				"role is required (worker|avatar); all other fields are optional and whitelisted; " +
				"the model must resolve to a workspace provider revision; the name must sit inside the " +
				"build authorization scope. Returns the persisted AgentRecord JSON.",
			InputSchema: createAgentSchema,
			ReadOnly:    false,
		},
		{
			Name: ToolUpdateAgent,
			Description: "按白名单字段补丁更新一个现有 Agent（生成下一个不可变版本）。" +
				"只动本次给出的字段，未给字段保持不变；name 是目标标识，本工具绝不改名；" +
				"graph_definition/id/workspace_id/version/team_id/owner_user_id 等平台字段一律拒绝。" +
				"Patch an existing agent over the assembly whitelist, producing the next immutable version. " +
				"Only the given fields change; unspecified fields stay untouched; name is the target identity " +
				"and is never changed; platform-managed fields are rejected. Returns the persisted AgentRecord JSON.",
			InputSchema: updateAgentSchema,
			ReadOnly:    false,
		},
	}
	return d
}

// ListTools returns the two assembly write tools.
func (d *WriteToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return d.tools, nil
}

// Dispatch gates the call on the receipt, the whitelist, and the write-time
// validation stack, executes the matching write tool, and records the
// outcome in the audit trail. Every failure is an IsError result, never a Go
// error.
func (d *WriteToolsDispatcher) Dispatch(
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
	case ToolCreateAgent:
		return d.createAgent(ctx, call)
	case ToolUpdateAgent:
		return d.updateAgent(ctx, call)
	default:
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
}

func (d *WriteToolsDispatcher) toolRegistered(name string) bool {
	for _, tool := range d.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

var _ contract.ToolDispatcher = (*WriteToolsDispatcher)(nil)

var createAgentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run。Optional: omit to auto-bind the authorized build run."},
		"name": {"type": "string", "description": "New agent name: ^[a-z0-9][a-z0-9_-]*$ (max 64), inside the receipt's asset scope"},
		"role": {"type": "string", "enum": ["worker", "avatar"], "description": "Required: worker or avatar"},
		"display_name": {"type": "string"},
		"identity": {"type": "object", "properties": {
			"core": {"type": "string"}, "extended": {"type": "string"}, "raw": {"type": "string"}
		}},
		"system_prompt": {"type": "string", "description": "v1.2 compatibility prompt"},
		"profiles": {"type": "object", "description": "profile name -> {system_addition, greeting}"},
		"skills": {"type": "array", "items": {"type": "object"}},
		"mcp_servers": {"type": "array", "items": {"type": "object"}},
		"model": {"type": "string", "description": "Must resolve to a workspace provider revision (F2)"},
		"memory": {"type": "object"},
		"memory_slots": {"type": "array", "items": {"type": "object"}},
		"guard": {"type": "object"},
		"compaction": {"type": "object"},
		"permissions": {"type": "object"},
		"output_schema": {"type": "object"},
		"max_cost_usd": {"type": "number"},
		"max_tokens": {"type": "integer"},
		"max_output_tokens": {"type": "integer"},
		"step_budget": {"type": "integer"},
		"max_tool_repeats": {"type": "integer"},
  "tool_loop_control": {"type":"object","additionalProperties":false,"required":["slice_rounds","initial_total_rounds"],"properties":{"slice_rounds":{"type":"integer","minimum":1,"maximum":1000},"initial_total_rounds":{"type":"integer","minimum":1,"maximum":9007199254740991}}},
		"fallback_models": {"type": "array", "items": {"type": "string"}},
		"fallback_retries": {"type": "integer"},
		"engine": {"type": "string", "description": "\"\", loom, opencode, codex, or claude"},
		"runtime_id": {"type": "string", "description": "Required exactly for CLI engines (claude/codex/opencode); must exist in this workspace"},
		"tags": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["name", "role"],
	"additionalProperties": false
}`)

var updateAgentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"build_run_id": {"type": "string", "description": "可选；省略时自动绑定当前授权 run"},
		"name": {"type": "string", "description": "Target agent name; this tool never renames"},
		"role": {"type": "string", "enum": ["worker", "avatar"]},
		"display_name": {"type": "string"},
		"identity": {"type": "object", "properties": {
			"core": {"type": "string"}, "extended": {"type": "string"}, "raw": {"type": "string"}
		}},
		"system_prompt": {"type": "string", "description": "v1.2 compatibility prompt"},
		"profiles": {"type": "object", "description": "profile name -> {system_addition, greeting}"},
		"skills": {"type": "array", "items": {"type": "object"}},
		"mcp_servers": {"type": "array", "items": {"type": "object"}},
		"model": {"type": "string", "description": "Must resolve to a workspace provider revision (F2)"},
		"memory": {"type": "object"},
		"memory_slots": {"type": "array", "items": {"type": "object"}},
		"guard": {"type": "object"},
		"compaction": {"type": "object"},
		"permissions": {"type": "object"},
		"output_schema": {"type": "object"},
		"max_cost_usd": {"type": "number"},
		"max_tokens": {"type": "integer"},
		"max_output_tokens": {"type": "integer"},
		"step_budget": {"type": "integer"},
		"max_tool_repeats": {"type": "integer"},
  "tool_loop_control": {"type":"object","additionalProperties":false,"required":["slice_rounds","initial_total_rounds"],"properties":{"slice_rounds":{"type":"integer","minimum":1,"maximum":1000},"initial_total_rounds":{"type":"integer","minimum":1,"maximum":9007199254740991}}},
		"fallback_models": {"type": "array", "items": {"type": "string"}},
		"fallback_retries": {"type": "integer"},
		"engine": {"type": "string", "description": "\"\", loom, opencode, codex, or claude"},
		"runtime_id": {"type": "string", "description": "Required exactly for CLI engines (claude/codex/opencode); must exist in this workspace"},
		"tags": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["name"],
	"additionalProperties": false
}`)
