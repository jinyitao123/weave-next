package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/org"
)

const (
	// ToolSubmitBrief is the discovery-to-planning bridge. Identity, status,
	// expiration, and conversation binding are fixed by the dispatcher.
	ToolSubmitBrief                    = "tf_submit_brief"
	BlueprintTemplateParameterGuidance = "模板参数规则：rework（delivery_rework_loop、creative_critique_loop）要求 primary_ref、reviewer_ref、max_iterations，禁止 parallel_worker_refs、finalizer_ref；synthesis（parallel_review、research_synthesis）要求至少两个 parallel_worker_refs 和 finalizer_ref，禁止 primary_ref、reviewer_ref、max_iterations。"

	defaultBriefTTL = 24 * time.Hour
	minimumBriefTTL = time.Hour
	maximumBriefTTL = 7 * 24 * time.Hour

	submitBriefErrorBudget = 3
)

var (
	// ErrActiveBuildRunConflict reports a conversation that already owns a
	// different active planning payload.
	ErrActiveBuildRunConflict = errors.New("conversation already has a different active build run")

	submitBriefInputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["brief","contract"],
  "properties":{
    "brief":{"$ref":"#/$defs/brief"},
    "contract":{"$ref":"#/$defs/contract"}
  },
  "$defs":{
    "budget":{"type":"object","additionalProperties":false,"properties":{"max_input_tokens":{"type":"integer","minimum":0},"max_output_tokens":{"type":"integer","minimum":0},"max_tool_calls":{"type":"integer","minimum":0},"max_cost_usd":{"type":"number","minimum":0}}},
    "asset_ref":{"type":"object","additionalProperties":false,"required":["kind","id"],"description":"Optimize refs require the exact stable ID read from the platform. Include the target team, every roster Agent, and every in-scope workflow before creating the immutable BuildRun.","properties":{"kind":{"type":"string","enum":["agent","team","workflow"]},"id":{"type":"string","minLength":1},"name":{"type":"string"}}},
    "asset_scope":{"type":"object","additionalProperties":false,"required":["allowed_kinds"],"properties":{"allowed_kinds":{"type":"array","minItems":1,"items":{"type":"string","enum":["agent","team","workflow"]}},"refs":{"type":"array","items":{"$ref":"#/$defs/asset_ref"}},"name_prefix":{"type":"string"}}},
    "waiver":{"type":"object","additionalProperties":false,"required":["gate_id","reason"],"properties":{"gate_id":{"type":"string"},"reason":{"type":"string"}}},
    "brief":{"type":"object","additionalProperties":false,"required":["schema_version","mode","business_direction","task","success_criteria","allowed_assets","round_budget","total_budget"],"properties":{"schema_version":{"const":1},"mode":{"type":"string","enum":["create","optimize"]},"business_direction":{"type":"string"},"task":{"type":"string"},"workflow_build_mode":{"type":"string","enum":["blueprint","custom"]},"template_gap":{"type":"string"},"team_id":{"type":"string"},"new_team_name":{"type":"string"},"expected_users":{"type":"string"},"inputs":{"type":"array","items":{"type":"string"}},"outputs":{"type":"array","items":{"type":"string"}},"success_criteria":{"type":"array","minItems":1,"items":{"type":"string"}},"constraints":{"type":"array","items":{"type":"string"}},"prohibitions":{"type":"array","items":{"type":"string"}},"waived_gates":{"type":"array","items":{"$ref":"#/$defs/waiver"}},"allowed_assets":{"$ref":"#/$defs/asset_scope"},"round_budget":{"$ref":"#/$defs/budget"},"total_budget":{"$ref":"#/$defs/budget"}}},
    "hard_gate":{"type":"object","additionalProperties":false,"required":["id","description","check"],"properties":{"id":{"type":"string"},"description":{"type":"string"},"check":{"type":"string"},"requirements":{"type":"array","items":{"type":"string"}},"floor":{"type":"boolean"}}},
    "rubric":{"type":"object","description":"One business-output quality dimension directly observable in scenario artifacts, such as correctness, completeness, consistency, or readability. Never use topology, config, runtime, model, management, or governance fidelity here; put those requirements in hard_gates.","additionalProperties":false,"required":["id","name","description","max_score","pass_threshold"],"properties":{"id":{"type":"string"},"name":{"type":"string"},"description":{"type":"string"},"max_score":{"type":"integer"},"pass_threshold":{"type":"integer"}}},
    "scenario":{"type":"object","additionalProperties":false,"required":["id","input","expected"],"properties":{"id":{"type":"string","minLength":1},"input":{"type":"string","minLength":1},"expected":{"type":"string","minLength":1}}},
    "perturbation_scenario":{"type":"object","description":"A concrete evaluator-only mutation. Materialize exactly one input+expected pair for every public_scenario × perturbation_rule pair; never append an abstract rule to business input or ask the candidate to invent the mutation.","additionalProperties":false,"required":["id","base_scenario_id","rule","input","expected"],"properties":{"id":{"type":"string","minLength":1},"base_scenario_id":{"type":"string","minLength":1},"rule":{"type":"string","minLength":1},"input":{"type":"string","minLength":1},"expected":{"type":"string","minLength":1}}},
    "contract":{"type":"object","additionalProperties":false,"required":["schema_version","hard_gates","rubric","public_scenarios","perturbation_rules","perturbation_scenarios","hidden_scenario_count","severe_defect_definition","run_count","max_iterations","pass_rules","block_rules","infra_failure_rules"],"properties":{"schema_version":{"const":2},"hard_gates":{"type":"array","items":{"$ref":"#/$defs/hard_gate"}},"waived_gates":{"type":"array","items":{"$ref":"#/$defs/waiver"}},"rubric":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/rubric"}},"public_scenarios":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/scenario"}},"perturbation_rules":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"perturbation_scenarios":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/perturbation_scenario"}},"hidden_scenario_count":{"type":"integer","minimum":0},"hidden_scenario_constraints":{"type":"array","items":{"type":"string"}},"severe_defect_definition":{"type":"string"},"run_count":{"type":"integer","minimum":1},"model_requirements":{"type":"array","items":{"type":"string"}},"runtime_requirements":{"type":"array","items":{"type":"string"}},"cost_cap_usd":{"type":"integer","minimum":0},"max_iterations":{"const":3},"pass_rules":{"type":"array","minItems":1,"items":{"type":"string"}},"block_rules":{"type":"array","minItems":1,"items":{"type":"string"}},"infra_failure_rules":{"type":"array","minItems":1,"items":{"type":"string"}}}}
  }
}`)
)

// BriefSubmissionStore is the exact storage surface needed by
// tf_submit_brief. *teambuild.Store satisfies it.
type BriefSubmissionStore interface {
	GetActiveBuildRunByConversation(ctx context.Context, workspaceID, conversationID string) (teambuild.TeamBuildRun, error)
	CreateBuildRun(ctx context.Context, workspaceID, buildRunID string, params teambuild.CreateRunParams) (teambuild.TeamBuildRun, error)
}

type latestBriefSubmissionStore interface {
	GetLatestBuildRunByConversation(ctx context.Context, workspaceID, conversationID string) (teambuild.TeamBuildRun, error)
}

// SubmitBriefToolsDispatcher exposes one server-bound discovery write.
type SubmitBriefToolsDispatcher struct {
	workspaceID    string
	conversationID string
	agent          string
	createdBy      string
	runs           BriefSubmissionStore
	teams          TeamReader
	roster         RosterReader
	workflows      WorkflowReader
	audit          AuditRecorder
	now            func() time.Time
	ttl            time.Duration
	mu             sync.Mutex
	errorCount     int
}

// NewSubmitBriefTools constructs the discovery bridge. ttl is a server-side
// setting and is clamped to [1h, 7d]; zero selects the 24h default.
func NewSubmitBriefTools(
	workspaceID, conversationID, agentName, createdBy string,
	runs BriefSubmissionStore,
	teams TeamReader,
	roster RosterReader,
	workflows WorkflowReader,
	audit AuditRecorder,
	now func() time.Time,
	ttl time.Duration,
) *SubmitBriefToolsDispatcher {
	if now == nil {
		now = time.Now
	}
	if ttl == 0 {
		ttl = defaultBriefTTL
	}
	if ttl < minimumBriefTTL {
		ttl = minimumBriefTTL
	}
	if ttl > maximumBriefTTL {
		ttl = maximumBriefTTL
	}
	return &SubmitBriefToolsDispatcher{
		workspaceID: workspaceID, conversationID: conversationID,
		agent: agentName, createdBy: createdBy,
		runs: runs, teams: teams, roster: roster, workflows: workflows,
		audit: audit, now: now, ttl: ttl,
	}
}

// ListTools publishes only the two model-authored business documents.
func (d *SubmitBriefToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	if d == nil || d.runs == nil || strings.TrimSpace(d.workspaceID) == "" ||
		strings.TrimSpace(d.conversationID) == "" || strings.TrimSpace(d.agent) == "" ||
		strings.TrimSpace(d.createdBy) == "" {
		return nil, nil
	}
	return []contract.ToolDef{{
		Name:        ToolSubmitBrief,
		Description: "提交已完成发现的 BuildBrief 与 EvaluationContract，建立绑定当前会话的 planning 规划。Rubric 只允许评价场景业务产物中可直接观察的正确性、完整性、一致性、可读性等质量；topology/config/runtime/model/management/governance 等配置保真要求必须写入 hard_gates。perturbation_rules 只是生成约束；必须在 perturbation_scenarios 中为每个 public_scenario × rule 冻结一个真实改变过的 input 及与之匹配的 expected，禁止把抽象规则追加到业务输入或让被测团队自行猜测变体。optimize 提交前必须读取目标团队、完整 roster 与相关 workflow；allowed_assets.refs 必须逐项包含目标 team、每个 roster Agent、每个纳入范围 workflow 的真实 kind+id，不能只填名称或只填 team，因为 BuildRun 建立后 brief 不可修改。身份、会话、状态、过期时间和 build_run_id 均由平台绑定；不会批准或启动构建。成功后下一步必须在同一轮或紧随其后的规划轮调用 tf_blueprint_plan 提交 TeamBlueprint；没有 blueprint revision 时不得请求用户批准。当前交互入口只接受 workflow_build_mode=blueprint。" + BlueprintTemplateParameterGuidance,
		InputSchema: submitBriefInputSchema,
	}}, nil
}

type submitBriefArgs struct {
	Brief    teambuild.BuildBrief         `json:"brief"`
	Contract teambuild.EvaluationContract `json:"contract"`
}

type submitBriefResult struct {
	BuildRunID        string `json:"build_run_id"`
	Status            string `json:"status"`
	ConversationID    string `json:"conversation_id"`
	NextAction        string `json:"next_action"`
	BlueprintGuidance string `json:"blueprint_guidance"`
}

// Dispatch validates, deduplicates, and creates the planning run. The unique
// index remains the final guard for concurrent calls; a losing identical call
// re-reads and returns the winner.
func (d *SubmitBriefToolsDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (result *contract.ToolResult, err error) {
	if d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	defer func() {
		if d != nil {
			recordAudit(ctx, d.audit, d.workspaceID, d.agent, call, result, err, "")
			if result != nil && result.IsError {
				d.errorCount++
			}
		}
	}()
	if call.Name != ToolSubmitBrief {
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	if d == nil || d.runs == nil || strings.TrimSpace(d.workspaceID) == "" ||
		strings.TrimSpace(d.conversationID) == "" || strings.TrimSpace(d.agent) == "" ||
		strings.TrimSpace(d.createdBy) == "" {
		return toolError(call.ID, "tf_submit_brief is unavailable"), nil
	}
	if d.errorCount >= submitBriefErrorBudget {
		return toolError(call.ID, `{"code":"submit_brief_budget_exhausted","error":"tf_submit_brief failed too many times in this turn","guidance":"Stop retrying this write tool. Produce a concise needs_clarification or blocked response, or restart discovery with a valid brief. allowed_assets.allowed_kinds may only be agent, team, workflow; optimize mode must use allowed_assets.refs with exact team/workflow/agent IDs; create mode must use name_prefix."}`), nil
	}
	var args submitBriefArgs
	if err := strictRaw(json.RawMessage(call.Args), &args); err != nil {
		return toolError(call.ID, "invalid tf_submit_brief arguments: "+err.Error()), nil
	}
	args = normalizeSubmitBriefArgs(args)
	briefHash, normalizedContract, contractHash, validateErr := teambuild.ValidateBuildRunDrafts(args.Brief, args.Contract)
	if validateErr != nil {
		var floorViolation *teambuild.ContractFloorViolation
		if errors.As(validateErr, &floorViolation) {
			payload, marshalErr := json.Marshal(map[string]string{
				"code":     "server_floor_roundtrip_invalid",
				"error":    validateErr.Error(),
				"guidance": "Remove every hard_gate with floor=true and retry tf_submit_brief in this turn. The server reinjects the canonical floor gates; preserve all business hard gates.",
			})
			if marshalErr != nil {
				return toolError(call.ID, validateErr.Error()), nil
			}
			return toolError(call.ID, string(payload)), nil
		}
		return toolError(call.ID, validateErr.Error()), nil
	}
	args.Contract = normalizedContract
	if args.Brief.EffectiveWorkflowBuildMode() == teambuild.WorkflowBuildModeCustom {
		return toolError(call.ID, "custom workflow build mode is not available from discovery: use workflow_build_mode=blueprint with one of the supported templates. For Lead/Coder/Verifier delivery and verification, use delivery_rework_loop and put the Lead's coordination/final-report duties in lead_instruction instead of a custom graph or finalizer."), nil
	}
	if result := d.rejectCreateDuplicateTeamName(ctx, call.ID, args.Brief); result != nil {
		return result, nil
	}
	if result := d.rejectInvalidOptimizeAssetRefs(ctx, call.ID, args.Brief); result != nil {
		return result, nil
	}

	if active, activeErr := d.runs.GetActiveBuildRunByConversation(ctx, d.workspaceID, d.conversationID); activeErr == nil {
		return d.resolveExisting(call.ID, active, briefHash, contractHash), nil
	} else if !errors.Is(activeErr, teambuild.ErrBuildRunNotFound) {
		return toolError(call.ID, "check active build run: "+activeErr.Error()), nil
	}

	run, createErr := d.runs.CreateBuildRun(ctx, d.workspaceID, "br-"+uuid.NewString(), teambuild.CreateRunParams{
		Brief: args.Brief, Contract: args.Contract,
		ExpiresAt: d.now().Add(d.ttl), CreatedBy: d.createdBy,
		ConversationID: d.conversationID,
	})
	if createErr != nil {
		// A concurrent call may have won the partial unique index. Re-read to
		// distinguish an identical retry from a conflicting payload.
		active, activeErr := d.runs.GetActiveBuildRunByConversation(ctx, d.workspaceID, d.conversationID)
		if activeErr == nil {
			return d.resolveExisting(call.ID, active, briefHash, contractHash), nil
		}
		return toolError(call.ID, createErr.Error()), nil
	}
	return submitBriefToolResult(call.ID, run), nil
}

func normalizeSubmitBriefArgs(args submitBriefArgs) submitBriefArgs {
	args.Brief = normalizeSubmitBrief(args.Brief)
	args.Contract = normalizeSubmitContract(args.Contract)
	return args
}

func normalizeSubmitBrief(brief teambuild.BuildBrief) teambuild.BuildBrief {
	if brief.SchemaVersion == 0 {
		brief.SchemaVersion = 1
	}
	if strings.TrimSpace(brief.WorkflowBuildMode) == "" {
		brief.WorkflowBuildMode = teambuild.WorkflowBuildModeBlueprint
	}
	if strings.TrimSpace(brief.TeamID) == "" && brief.Mode == teambuild.ModeOptimize {
		if teamID, ok := singleAssetRefID(brief.AllowedAssets.Refs, "team"); ok {
			brief.TeamID = teamID
		}
	}
	if brief.Mode == teambuild.ModeCreate && strings.TrimSpace(brief.AllowedAssets.NamePrefix) != "" {
		brief.AllowedAssets.AllowedKinds = appendSubmitAllowedKind(brief.AllowedAssets.AllowedKinds, "agent")
		brief.AllowedAssets.AllowedKinds = appendSubmitAllowedKind(brief.AllowedAssets.AllowedKinds, "team")
		brief.AllowedAssets.AllowedKinds = appendSubmitAllowedKind(brief.AllowedAssets.AllowedKinds, "workflow")
	}
	if budgetIsEmpty(brief.RoundBudget) {
		brief.RoundBudget = teambuild.Budget{MaxInputTokens: 100000, MaxOutputTokens: 20000, MaxToolCalls: 200}
	}
	if budgetIsEmpty(brief.TotalBudget) {
		brief.TotalBudget = teambuild.Budget{MaxInputTokens: 600000, MaxOutputTokens: 120000, MaxToolCalls: 2000}
	}
	return brief
}

func normalizeSubmitContract(contract teambuild.EvaluationContract) teambuild.EvaluationContract {
	// The discovery write surface only creates v2 contracts. Schema v1 remains
	// readable for historical BuildRuns but cannot create another ambiguous
	// abstract-rule suite.
	contract.SchemaVersion = 2
	if contract.RunCount == 0 {
		contract.RunCount = 1
	}
	if contract.MaxIterations == 0 {
		contract.MaxIterations = 3
	}
	if len(contract.PassRules) == 0 {
		contract.PassRules = []string{"all hard gates pass and rubric pass thresholds are met"}
	}
	if len(contract.BlockRules) == 0 {
		defect := strings.TrimSpace(contract.SevereDefectDefinition)
		if defect == "" {
			defect = "a severe defect is found"
		}
		contract.BlockRules = []string{defect}
	}
	if len(contract.InfraFailureRules) == 0 {
		contract.InfraFailureRules = []string{"required model, runtime, database, or platform tool is unavailable"}
	}
	return contract
}

func appendSubmitAllowedKind(kinds []string, kind string) []string {
	for _, existing := range kinds {
		if strings.TrimSpace(existing) == kind {
			return kinds
		}
	}
	return append(kinds, kind)
}

func singleAssetRefID(refs []teambuild.AssetRef, kind string) (string, bool) {
	var found string
	for _, ref := range refs {
		if strings.TrimSpace(ref.Kind) != kind {
			continue
		}
		id := strings.TrimSpace(ref.ID)
		if id == "" {
			continue
		}
		if found != "" && found != id {
			return "", false
		}
		found = id
	}
	return found, found != ""
}

func budgetIsEmpty(budget teambuild.Budget) bool {
	return budget.MaxInputTokens == 0 &&
		budget.MaxOutputTokens == 0 &&
		budget.MaxToolCalls == 0 &&
		budget.MaxCostUSD == 0
}

func (d *SubmitBriefToolsDispatcher) rejectCreateDuplicateTeamName(
	ctx context.Context,
	callID string,
	brief teambuild.BuildBrief,
) *contract.ToolResult {
	if brief.Mode != teambuild.ModeCreate || d.teams == nil {
		return nil
	}
	newTeamName := strings.TrimSpace(brief.NewTeamName)
	if newTeamName == "" {
		return nil
	}
	teams, err := d.teams.ListTeams(ctx, d.workspaceID)
	if err != nil {
		return toolError(callID, "validate create team name: "+err.Error())
	}
	for _, team := range teams {
		if strings.EqualFold(strings.TrimSpace(team.Name), newTeamName) {
			if recovery := d.blockedCreateRecovery(ctx, team, brief); recovery != nil {
				content, marshalErr := json.Marshal(recovery)
				if marshalErr != nil {
					return toolError(callID, "encode blocked create recovery: "+marshalErr.Error())
				}
				return toolError(callID, string(content))
			}
			return toolError(callID, fmt.Sprintf(
				"invalid build brief: create mode targets existing team %q (team_id %s); use optimize mode with team_id",
				team.Name, team.ID,
			))
		}
	}
	return nil
}

type blockedCreateRecoveryJSON struct {
	Code               string               `json:"code"`
	Error              string               `json:"error"`
	Guidance           string               `json:"guidance"`
	RecoveryBriefPatch map[string]any       `json:"recovery_brief_patch"`
	AllowedAssets      teambuild.AssetScope `json:"allowed_assets"`
}

func (d *SubmitBriefToolsDispatcher) blockedCreateRecovery(
	ctx context.Context,
	team org.Team,
	brief teambuild.BuildBrief,
) *blockedCreateRecoveryJSON {
	if d.runs == nil || d.roster == nil || d.workflows == nil {
		return nil
	}
	latestRuns, ok := d.runs.(latestBriefSubmissionStore)
	if !ok {
		return nil
	}
	run, err := latestRuns.GetLatestBuildRunByConversation(ctx, d.workspaceID, d.conversationID)
	if err != nil || run.Status != teambuild.StatusBlocked || run.Mode != teambuild.ModeCreate ||
		run.DecidedAt == nil || run.CreatedAt.IsZero() ||
		team.CreatedAt.Before(run.CreatedAt) || team.CreatedAt.After(*run.DecidedAt) ||
		!strings.EqualFold(strings.TrimSpace(run.Brief.NewTeamName), strings.TrimSpace(brief.NewTeamName)) {
		return nil
	}
	workers, err := d.roster.ListByTeam(ctx, d.workspaceID, team.ID)
	if err != nil {
		return nil
	}
	workflows, err := d.workflows.ListByTeam(ctx, d.workspaceID, team.ID)
	if err != nil {
		return nil
	}
	refs := []teambuild.AssetRef{{Kind: "team", ID: team.ID, Name: team.Name}}
	agentIDs := make(map[string]struct{}, len(workers)+1)
	addAgent := func(agentID string) {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			return
		}
		if _, exists := agentIDs[agentID]; exists {
			return
		}
		agentIDs[agentID] = struct{}{}
		refs = append(refs, teambuild.AssetRef{Kind: "agent", ID: agentID})
	}
	addAgent(team.LeadAvatarID)
	for _, worker := range workers {
		addAgent(worker.WorkerAgentID)
	}
	hasPublishedWorkflow := false
	for _, item := range workflows {
		if item.PublishedVersion == nil {
			continue
		}
		hasPublishedWorkflow = true
		refs = append(refs, teambuild.AssetRef{Kind: "workflow", ID: item.ID, Name: item.Name})
	}
	if !hasPublishedWorkflow {
		refs = append(refs, teambuild.AssetRef{
			Kind: "workflow",
			ID:   teambuild.FirstOptimizeWorkflowID(team.ID),
		})
	}
	scope := teambuild.AssetScope{
		AllowedKinds: []string{"agent", "team", "workflow"},
		Refs:         refs,
	}
	return &blockedCreateRecoveryJSON{
		Code:     "blocked_create_assets_materialized",
		Error:    fmt.Sprintf("create mode targets the staged team %q from the blocked build", team.Name),
		Guidance: "Retry tf_submit_brief in this turn with mode=optimize, team_id and allowed_assets from recovery_brief_patch. This is recovery of the same blocked build, not reuse of an unrelated team. Preserve the latest blueprint roster and correct any obsolete provider/model gates against current capabilities.",
		RecoveryBriefPatch: map[string]any{
			"mode":           teambuild.ModeOptimize,
			"team_id":        team.ID,
			"new_team_name":  "",
			"allowed_assets": scope,
		},
		AllowedAssets: scope,
	}
}

func (d *SubmitBriefToolsDispatcher) rejectInvalidOptimizeAssetRefs(
	ctx context.Context,
	callID string,
	brief teambuild.BuildBrief,
) *contract.ToolResult {
	if brief.Mode != teambuild.ModeOptimize || d.teams == nil || d.roster == nil || d.workflows == nil {
		return nil
	}
	teamID := strings.TrimSpace(brief.TeamID)
	teams, err := d.teams.ListTeams(ctx, d.workspaceID)
	if err != nil {
		return toolError(callID, "validate optimize asset refs: list teams: "+err.Error())
	}
	valid := map[[2]string]struct{}{}
	teamFound := false
	for _, team := range teams {
		if team.ID != teamID {
			continue
		}
		teamFound = true
		valid[[2]string{"team", team.ID}] = struct{}{}
		if leadID := strings.TrimSpace(team.LeadAvatarID); leadID != "" {
			valid[[2]string{"agent", leadID}] = struct{}{}
		}
		break
	}
	if !teamFound {
		return toolError(callID, fmt.Sprintf("invalid optimize build brief: target team_id %q does not exist", teamID))
	}
	workers, err := d.roster.ListByTeam(ctx, d.workspaceID, teamID)
	if err != nil {
		return toolError(callID, "validate optimize asset refs: read roster: "+err.Error())
	}
	for _, worker := range workers {
		if id := strings.TrimSpace(worker.WorkerAgentID); id != "" {
			valid[[2]string{"agent", id}] = struct{}{}
		}
	}
	teamWorkflows, err := d.workflows.ListByTeam(ctx, d.workspaceID, teamID)
	if err != nil {
		return toolError(callID, "validate optimize asset refs: read workflows: "+err.Error())
	}
	hasPublishedWorkflow := false
	for _, workflow := range teamWorkflows {
		valid[[2]string{"workflow", workflow.ID}] = struct{}{}
		if workflow.PublishedVersion != nil {
			hasPublishedWorkflow = true
		}
	}
	if !hasPublishedWorkflow {
		valid[[2]string{"workflow", teambuild.FirstOptimizeWorkflowID(teamID)}] = struct{}{}
	}
	for _, ref := range brief.AllowedAssets.Refs {
		key := [2]string{strings.TrimSpace(ref.Kind), strings.TrimSpace(ref.ID)}
		if _, ok := valid[key]; !ok {
			return toolError(callID, fmt.Sprintf(
				"invalid optimize allowed_assets ref: kind %q id %q is not an exact target-team baseline asset; use the stable IDs returned by tf_get_team",
				ref.Kind, ref.ID,
			))
		}
	}
	return nil
}

func (d *SubmitBriefToolsDispatcher) resolveExisting(
	callID string,
	run teambuild.TeamBuildRun,
	briefHash, contractHash string,
) *contract.ToolResult {
	if run.BriefHash == briefHash && run.ContractHash == contractHash {
		return submitBriefToolResult(callID, run)
	}
	return toolError(callID, fmt.Sprintf(
		"%s: build_run_id %q; cancel or finish the existing plan before submitting different brief/contract content",
		ErrActiveBuildRunConflict, run.BuildRunID,
	))
}

func submitBriefToolResult(callID string, run teambuild.TeamBuildRun) *contract.ToolResult {
	result, err := toolJSON(callID, submitBriefResult{
		BuildRunID: run.BuildRunID, Status: teambuild.StatusPlanning,
		ConversationID: run.ConversationID, NextAction: "submit_blueprint_plan",
		BlueprintGuidance: BlueprintTemplateParameterGuidance,
	})
	if err != nil {
		return toolError(callID, err.Error())
	}
	return result
}
