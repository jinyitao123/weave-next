// Package teambuild defines the frozen contract documents and the persisted
// execution-control record for one team build task. The package owns the
// storage layer only: no HTTP wiring, no build tooling, and no rollback
// execution live here.
package teambuild

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teameval/gatecodes"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

const (
	schemaVersion = 1

	ModeCreate   = "create"
	ModeOptimize = "optimize"

	ExecutionStrategyLegacy              = "legacy"
	ExecutionStrategyCompilerV1          = "compiler_v1"
	ExecutionStrategyTemplateInstantiate = "template_instantiate"

	WorkflowBuildModeBlueprint = "blueprint"
	WorkflowBuildModeCustom    = "custom"

	StatusPlanning     = "planning"
	StatusAuthorized   = "authorized"
	StatusRoundRunning = "round_running"
	StatusPublishing   = "publishing"
	StatusPassed       = "passed"
	StatusBlocked      = "blocked"
	StatusCancelled    = "cancelled"

	BudgetExhaustedReason = "budget_exhausted"

	RollbackNone       = "none"
	RollbackRolledBack = "rolled_back"
	RollbackFailed     = "rollback_failed"

	ConclusionPass    = "pass"
	ConclusionRevise  = "revise"
	ConclusionBlocked = "blocked"

	maxContractIterations = 3

	// UsageSourceKindBuildAgent is the usage source kind recorded by the
	// Build phase (T14B-1): each built-in Loom meta-team employee run.
	UsageSourceKindBuildAgent = "build_agent"
	// UsageSourceKindCandidateRuntime is the usage source kind recorded by
	// the Evaluate phase (T14B-2A): the candidate test run that executes a
	// frozen candidate as a fixed workflow root.
	UsageSourceKindCandidateRuntime = "candidate_runtime"

	// SourceRole constants identify built-in Loom meta-agent usage. Architect
	// remains a stable historical/pre-authorization role; post-authorization
	// construction attributes config engineer and graph designer runs, while
	// business-quality revision planning uses its dedicated planner role.
	SourceRoleArchitect             = "architect"
	SourceRoleConfigEngineer        = "config_engineer"
	SourceRoleGraphDesigner         = "graph_designer"
	SourceRoleEvalDebugger          = "eval_debugger"
	SourceRoleSemanticJudge         = "semantic_judge"
	SourceRoleBlueprintPatchPlanner = "blueprint_patch_planner"
	// SourceRoleFixedWorkflowRoot is the role of the candidate runtime usage
	// source: one candidate test run of a fixed workflow root.
	SourceRoleFixedWorkflowRoot = "fixed_workflow_root"
)

var buildBriefAssetKinds = map[string]struct{}{
	"agent":    {},
	"team":     {},
	"workflow": {},
}

// FirstOptimizeWorkflowID is the only workflow identity an optimize run may
// introduce when the target team has no workflow in its frozen baseline.
// Keeping the derivation here makes authorization scope, baseline closure,
// and compiler output agree on the same non-speculative asset.
func FirstOptimizeWorkflowID(teamID string) string {
	return strings.TrimSpace(teamID) + "-workflow"
}

var terminalStatuses = map[string]bool{
	StatusPassed:    true,
	StatusBlocked:   true,
	StatusCancelled: true,
}

var contractFloorGateIDs = []string{
	gatecodes.L1WorkerCapabilities,
	gatecodes.L1FlowCapabilities,
	gatecodes.EngineGraphMatch,
	gatecodes.DepsPinned,
	gatecodes.GraphSchema,
}

var contractFloorGates = map[string]HardGate{
	gatecodes.L1WorkerCapabilities: {
		ID:          gatecodes.L1WorkerCapabilities,
		Description: "workers have an executable capability path through standard ToolLoop, CLI runtime, or a valid internal graph",
		Check:       "machine evaluator derives each worker execution path from persisted agent and runtime configuration",
		Requirements: []string{
			"worker_execution_path", "exact_runtime_or_model_binding",
		},
		Floor: true,
	},
	gatecodes.L1FlowCapabilities: {
		ID:          gatecodes.L1FlowCapabilities,
		Description: "team workflow composes real worker execution into a deliverable and uses only legal control flow",
		Check:       "machine evaluator derives coordination from the persisted workflow; fanout and rework are required only when present in the business shape",
		Requirements: []string{
			"worker_coordination", "deliver", "machine_valid_control_flow",
		},
		Floor: true,
	},
	gatecodes.EngineGraphMatch: {
		ID:           gatecodes.EngineGraphMatch,
		Description:  "agent engine and graph configuration agree",
		Check:        "machine evaluator compares engine selection with graph configuration",
		Requirements: []string{"engine_graph_match"},
		Floor:        true,
	},
	gatecodes.DepsPinned: {
		ID:           gatecodes.DepsPinned,
		Description:  "runtime, model, skill, MCP, and agent dependencies are pinned",
		Check:        "machine evaluator resolves exact immutable dependency versions",
		Requirements: []string{"exact_dependency_versions"},
		Floor:        true,
	},
	gatecodes.GraphSchema: {
		ID:           gatecodes.GraphSchema,
		Description:  "employee graphs and team workflows satisfy machine schemas",
		Check:        "shared graph and workflow validators return no errors",
		Requirements: []string{"graph_and_workflow_schema_valid"},
		Floor:        true,
	},
}

var knownEvaluationGateIDs = map[string]bool{
	gatecodes.AgentRolesExist:       true,
	gatecodes.TeamShape:             true,
	gatecodes.WorkerKinds:           true,
	gatecodes.WorkflowRefs:          true,
	gatecodes.NoCrossEmployee:       true,
	gatecodes.GraphSchema:           true,
	gatecodes.DepsFreezable:         true,
	gatecodes.DepsPinned:            true,
	gatecodes.EngineGraphMatch:      true,
	gatecodes.ScopeCompliance:       true,
	gatecodes.L1WorkerCapabilities:  true,
	gatecodes.L1FlowCapabilities:    true,
	gatecodes.RunTerminalConsistent: true,
	gatecodes.NoGovernanceViolation: true,
}

// ContractFloorViolation carries field-level reasons for a rejected
// EvaluationContract. Store methods wrap it with ErrBuildRunDraftInvalid;
// errors.As still exposes these details to the HTTP layer.
type ContractFloorViolation struct {
	MissingGateIDs      []string
	WeakenedGateIDs     []string
	InvalidFloorGateIDs []string
	InvalidWaivers      []string
}

func (e *ContractFloorViolation) Error() string {
	if e == nil {
		return "evaluation contract floor violation"
	}
	parts := make([]string, 0, 4)
	if len(e.MissingGateIDs) > 0 {
		parts = append(parts, "missing floor gates: "+strings.Join(e.MissingGateIDs, ", "))
	}
	if len(e.WeakenedGateIDs) > 0 {
		parts = append(parts, "weakened floor gates: "+strings.Join(e.WeakenedGateIDs, ", "))
	}
	if len(e.InvalidFloorGateIDs) > 0 {
		parts = append(parts, "invalid floor markers: "+strings.Join(e.InvalidFloorGateIDs, ", "))
	}
	if len(e.InvalidWaivers) > 0 {
		parts = append(parts, "invalid gate waivers: "+strings.Join(e.InvalidWaivers, "; "))
	}
	if len(parts) == 0 {
		return "evaluation contract floor violation"
	}
	return "evaluation contract floor violation: " + strings.Join(parts, "; ")
}

// DefaultFloorHardGates returns an independent copy of the current L1
// machine-executable contract floor in its stable persisted order.
func DefaultFloorHardGates() []HardGate {
	gates := make([]HardGate, 0, len(contractFloorGateIDs))
	for _, id := range contractFloorGateIDs {
		gate := contractFloorGates[id]
		gate.Requirements = append([]string(nil), gate.Requirements...)
		gates = append(gates, gate)
	}
	return gates
}

// IsContractFloorGate reports whether code belongs to the server-owned L1
// default floor.
func IsContractFloorGate(code string) bool {
	_, ok := contractFloorGates[code]
	return ok
}

// ErrBuiltinAssetInBrief reports an asset scope that names a built-in
// platform asset. The reserved "__" name prefix marks platform-owned assets
// (the meta team employees and team, the built-in graph designer, ...); they
// must never enter a build brief, so the write tools can never target them.
var ErrBuiltinAssetInBrief = errors.New("asset scope must not reference built-in platform assets with the reserved __ prefix")

// AssetRef identifies one target asset by kind and either its stable ID or
// its name (create-mode assets may only have a name before they exist).
type AssetRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// AssetScope declares which assets a build task may touch. Exactly one of
// NamePrefix (create-mode naming namespace) or Refs (optimize-mode explicit
// targets) must be set; AllowedKinds always constrains the kinds.
type AssetScope struct {
	AllowedKinds []string   `json:"allowed_kinds"`
	Refs         []AssetRef `json:"refs,omitempty"`
	NamePrefix   string     `json:"name_prefix,omitempty"`
}

// Contains reports whether ref falls inside this scope.
func (scope AssetScope) Contains(ref AssetRef) bool {
	if err := validateAssetRef(ref); err != nil {
		return false
	}
	kindAllowed := false
	for _, kind := range scope.AllowedKinds {
		if kind == ref.Kind {
			kindAllowed = true
			break
		}
	}
	if !kindAllowed {
		return false
	}
	if scope.NamePrefix != "" {
		return ref.Name != "" && strings.HasPrefix(ref.Name, scope.NamePrefix)
	}
	for _, allowed := range scope.Refs {
		if allowed.Kind != ref.Kind {
			continue
		}
		if allowed.ID != "" {
			// Optimize scopes pin assets by stable ID. Older write tools and
			// some workflow macro paths historically passed that exact stable
			// ID in the Name field for name-prefix compatibility. Treat only
			// exact ID equality as compatible; display-name matches remain
			// governed by the Name-only branch below.
			if allowed.ID != ref.ID && allowed.ID != ref.Name {
				continue
			}
			return true
		}
		if allowed.Name != "" && allowed.Name != ref.Name {
			continue
		}
		return true
	}
	return false
}

// Budget bounds one build round or the whole build task. At least one bound
// must be positive; a zero bound means that dimension is unlimited.
type Budget struct {
	MaxInputTokens  int64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	MaxToolCalls    int64 `json:"max_tool_calls,omitempty"`
	// MaxCostUSD is the platform USD cost cap. The platform usage ABI is
	// float64 USD (loom contract.Usage.CostUSD), so the budget bound follows
	// the same type; it must be finite and non-negative.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// BuildBrief is the task brief confirmed once by the workspace admin
// (plan §6.1). Its canonical JSON is the source of the run's brief hash and
// of the derived asset scope and budget columns.
type BuildBrief struct {
	SchemaVersion     int          `json:"schema_version"`
	Mode              string       `json:"mode"`
	BusinessDirection string       `json:"business_direction"`
	Task              string       `json:"task"`
	WorkflowBuildMode string       `json:"workflow_build_mode,omitempty"`
	TemplateGap       string       `json:"template_gap,omitempty"`
	TeamID            string       `json:"team_id,omitempty"`
	NewTeamName       string       `json:"new_team_name,omitempty"`
	ExpectedUsers     string       `json:"expected_users,omitempty"`
	Inputs            []string     `json:"inputs,omitempty"`
	Outputs           []string     `json:"outputs,omitempty"`
	SuccessCriteria   []string     `json:"success_criteria"`
	Constraints       []string     `json:"constraints,omitempty"`
	Prohibitions      []string     `json:"prohibitions,omitempty"`
	WaivedGates       []GateWaiver `json:"waived_gates,omitempty"`
	// UnmeasuredUsageWaiver is the temporary Phase-1 authorization for
	// publishing a candidate whose report explicitly says usage_complete=false.
	// Absence is the fail-closed default; old brief JSON remains readable.
	UnmeasuredUsageWaiver *UnmeasuredUsageWaiver `json:"unmeasured_usage_waiver,omitempty"`
	AllowedAssets         AssetScope             `json:"allowed_assets"`
	RoundBudget           Budget                 `json:"round_budget"`
	TotalBudget           Budget                 `json:"total_budget"`
}

// EffectiveWorkflowBuildMode resolves the backward-compatible zero value to
// the safe default. Custom raw-graph construction is an explicit capability
// granted by the frozen, admin-confirmed brief; it is never inferred from an
// LLM message or conversation history.
func (b BuildBrief) EffectiveWorkflowBuildMode() string {
	if b.WorkflowBuildMode == "" {
		return WorkflowBuildModeBlueprint
	}
	return b.WorkflowBuildMode
}

// Hash returns the sha256 of the strict canonical brief JSON.
func (b BuildBrief) Hash() (string, error) {
	if err := validateBuildBrief(b); err != nil {
		return "", err
	}
	return hashDocument(b)
}

// HardGate is one configuration/runtime gate that must pass without being
// offset by quality scores.
type HardGate struct {
	ID           string   `json:"id"`
	Description  string   `json:"description"`
	Check        string   `json:"check"`
	Requirements []string `json:"requirements,omitempty"`
	Floor        bool     `json:"floor"`
}

// GateWaiver is an explicit, reasoned exception requested in the brief and
// copied server-side into the frozen evaluation contract.
type GateWaiver struct {
	GateID string `json:"gate_id"`
	Reason string `json:"reason"`
}

// UnmeasuredUsageWaiver records an administrator's explicit acceptance of
// usage sources or dimensions that remain unmeasured. Phase 2 narrows this
// waiver to gaps such as a missing CLI receipt, a token-only receipt's cost
// dimension, or CLI-internal tool calls; it never turns unknowns into zero.
type UnmeasuredUsageWaiver struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
}

// RubricDimension is one business-quality dimension with its threshold.
type RubricDimension struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	MaxScore      int    `json:"max_score"`
	PassThreshold int    `json:"pass_threshold"`
}

// Scenario is one public regression scenario.
type Scenario struct {
	ID       string `json:"id"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

// PerturbationScenario is one concrete evaluator-only mutation generated from
// a frozen public scenario under one frozen perturbation rule. Rules describe
// how to vary a case; they are never executable business input by themselves.
// Freezing the materialized input and its matching expectation prevents a
// candidate from being asked to invent its own test while the evaluator keeps
// grading against the unchanged public answer.
type PerturbationScenario struct {
	ID             string `json:"id"`
	BaseScenarioID string `json:"base_scenario_id"`
	Rule           string `json:"rule"`
	Input          string `json:"input"`
	Expected       string `json:"expected"`
}

// EvaluationContract is the confirmed evaluation contract (plan §6.2).
// Its canonical JSON is the source of the run's contract hash, which the
// build authorization receipt binds.
type EvaluationContract struct {
	SchemaVersion             int                    `json:"schema_version"`
	HardGates                 []HardGate             `json:"hard_gates"`
	WaivedGates               []GateWaiver           `json:"waived_gates,omitempty"`
	Rubric                    []RubricDimension      `json:"rubric"`
	PublicScenarios           []Scenario             `json:"public_scenarios"`
	PerturbationRules         []string               `json:"perturbation_rules"`
	PerturbationScenarios     []PerturbationScenario `json:"perturbation_scenarios,omitempty"`
	HiddenScenarioCount       int                    `json:"hidden_scenario_count"`
	HiddenScenarioConstraints []string               `json:"hidden_scenario_constraints,omitempty"`
	SevereDefectDefinition    string                 `json:"severe_defect_definition"`
	RunCount                  int                    `json:"run_count"`
	ModelRequirements         []string               `json:"model_requirements,omitempty"`
	RuntimeRequirements       []string               `json:"runtime_requirements,omitempty"`
	CostCapUSD                int64                  `json:"cost_cap_usd,omitempty"`
	MaxIterations             int                    `json:"max_iterations"`
	PassRules                 []string               `json:"pass_rules"`
	BlockRules                []string               `json:"block_rules"`
	InfraFailureRules         []string               `json:"infra_failure_rules"`
}

// Hash returns the sha256 of the strict canonical contract JSON.
func (c EvaluationContract) Hash() (string, error) {
	if err := validateEvaluationContract(c); err != nil {
		return "", err
	}
	return hashDocument(c)
}

// VersionedRef points at one tested asset with its exact version.
type VersionedRef struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// HardGateResult is the evidence-backed outcome of one hard gate.
type HardGateResult struct {
	GateID       string `json:"gate_id"`
	Passed       bool   `json:"passed"`
	EvidenceRef  string `json:"evidence_ref"`
	Status       string `json:"status,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Floor        bool   `json:"floor"`
	WaiverReason string `json:"waiver_reason,omitempty"`
}

// ScenarioResult records one scenario's real run outcome.
type ScenarioResult struct {
	ScenarioID     string `json:"scenario_id"`
	InputVersion   string `json:"input_version"`
	RunID          string `json:"run_id"`
	TerminalStatus string `json:"terminal_status"`
	ArtifactRef    string `json:"artifact_ref"`
}

// RubricScore is one dimension's score and its written rationale.
type RubricScore struct {
	DimensionID string `json:"dimension_id"`
	Score       int    `json:"score"`
	Reason      string `json:"reason"`
}

// EvaluationReport is one immutable per-round evaluation (plan §6.3). Its
// canonical JSON hash is stored as the round's report_ref.
type EvaluationReport struct {
	SchemaVersion       int              `json:"schema_version"`
	RoundNo             int              `json:"round_no"`
	TestedAgents        []VersionedRef   `json:"tested_agents"`
	TestedTeams         []VersionedRef   `json:"tested_teams"`
	TestedWorkflows     []VersionedRef   `json:"tested_workflows"`
	ConfigChangeSummary string           `json:"config_change_summary"`
	HardGateResults     []HardGateResult `json:"hard_gate_results"`
	ScenarioResults     []ScenarioResult `json:"scenario_results"`
	RubricScores        []RubricScore    `json:"rubric_scores"`
	LatencyMs           int64            `json:"latency_ms,omitempty"`
	InputTokens         int64            `json:"input_tokens,omitempty"`
	OutputTokens        int64            `json:"output_tokens,omitempty"`
	// Tokens is the legacy compatibility summary of one round's token
	// consumption. New reports must set it to InputTokens + OutputTokens;
	// legacy reports that only carry Tokens remain readable.
	Tokens int64 `json:"tokens,omitempty"`
	// CostUSD is the round's platform USD cost (float64, matching the
	// platform usage ABI); it must be finite and non-negative.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// UsageComplete is nil (or true) when the usage numbers above cover the
	// whole candidate run, and explicitly false when some executed node has
	// no measurable usage receipt (candidate fanout legs / CLI node without
	// a receipt). UsageIncompleteReason names the unmeasured part; the
	// budget gate then enforces the measured usage as a lower bound.
	UsageComplete         *bool    `json:"usage_complete,omitempty"`
	UsageIncompleteReason string   `json:"usage_incomplete_reason,omitempty"`
	UsageHasTokens        *bool    `json:"usage_has_tokens,omitempty"`
	UsageHasCost          *bool    `json:"usage_has_cost,omitempty"`
	UsageSources          []string `json:"usage_sources,omitempty"`
	ToolCalls             int64    `json:"tool_calls,omitempty"`
	Regressions           []string `json:"regressions,omitempty"`
	Improvements          []string `json:"improvements,omitempty"`
	SevereDefects         []string `json:"severe_defects,omitempty"`
	FailureSamples        []string `json:"failure_samples,omitempty"`
	InfraErrors           []string `json:"infra_errors,omitempty"`
	// FailureCategory is the primary failure class (F1–F14 or a custom
	// label) for this round. The round controller persists it inside the
	// report JSON so the early-stop rule (same category for two consecutive
	// non-pass rounds) survives crashes.
	FailureCategory string `json:"failure_category,omitempty"`
	Conclusion      string `json:"conclusion"`
	// Baseline marks the optimize-mode baseline evaluation report produced
	// before round 1's Build. The report is persisted as an immutable
	// run-level fact (never a round ledger row) and serves as the
	// regression/improvement anchor for every round report.
	Baseline bool `json:"baseline,omitempty"`
}

// BudgetUsage is the aggregated consumption of one scope: one round or the
// whole build task. The zero value means no usage has been recorded.
type BudgetUsage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	ToolCalls    int64
}

// BudgetCharge is one append-only per-source ledger entry for a build run.
// The identity (round_no, source_kind, source_run_id) is the idempotency
// key: replaying the same source with identical facts succeeds and returns
// the existing row, while the same identity with different facts conflicts
// (ErrBudgetUsageConflict) and leaves the original row untouched.
type BudgetCharge struct {
	WorkspaceID  string
	BuildRunID   string
	RoundNo      int
	SourceKind   string
	SourceRole   string
	SourceRunID  string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	ToolCalls    int64
	CreatedAt    time.Time
}

// BuildUsageSource is one runtime-run association fact for a team build run:
// a built-in Loom meta agent was admitted with the runtime-owned run id
// source_run_id before its graph executed. The identity
// (round_no, source_kind, source_run_id) is the idempotency key: replaying
// the same identity with the same role succeeds and returns the existing
// row, while the same identity with a different role conflicts
// (ErrUsageSourceConflict). The row never carries usage numbers; the T14A
// budget ledger remains the single source of truth for consumption, and this
// table exists so a crash between admission and the terminal marker can be
// reconciled instead of losing attribution.
type BuildUsageSource struct {
	WorkspaceID string
	BuildRunID  string
	RoundNo     int
	SourceKind  string
	SourceRole  string
	SourceRunID string
	CreatedAt   time.Time
}

// BlueprintPatchPlanningAttempt binds one immutable Blueprint revision and
// report to the single runtime-owned planner run permitted to produce its
// patch. Output is persisted before the runtime terminal marker and is reused
// verbatim during controller recovery.
type BlueprintPatchPlanningAttempt struct {
	WorkspaceID      string
	BuildRunID       string
	RevisionNo       int
	SourceRole       string
	SourceReportHash string
	SourceRunID      string
	OutputText       *string
	OutputHash       *string
	CreatedAt        time.Time
	OutputRecordedAt *time.Time
}

// SemanticEvaluationAttempt binds one immutable candidate evidence package to
// the single meta-team evaluator run permitted to judge its semantic rubric.
type SemanticEvaluationAttempt struct {
	WorkspaceID      string
	BuildRunID       string
	RevisionNo       int
	SourceRole       string
	EvidenceHash     string
	SourceRunID      string
	OutputText       *string
	OutputHash       *string
	CreatedAt        time.Time
	OutputRecordedAt *time.Time
}

// BudgetDecision is the budget-gate verdict at one checkpoint. Every caller
// decides exclusively from ExceededDims so exact budget equality remains a
// passing state.
type BudgetDecision struct {
	// ExceededDims lists every round/total dimension whose consumption is
	// strictly over its limit (e.g. "round input_tokens", "total cost_usd").
	// Any exceeded dimension hard-blocks the run regardless of conclusion.
	ExceededDims []string
}

// Hash returns the sha256 of the strict canonical report JSON.
func (r EvaluationReport) Hash() (string, error) {
	if err := validateEvaluationReport(r); err != nil {
		return "", err
	}
	return hashDocument(r)
}

// BaselineTeamRef freezes the target team's full identity and status at the
// pre-task state. The workspace join binds the snapshot to one workspace so a
// cross-workspace restore can never be mistaken for the intended target.
type BaselineTeamRef struct {
	WorkspaceID     string `json:"workspace_id"`
	TeamID          string `json:"team_id"`
	Name            string `json:"name"`
	Objective       string `json:"objective"`
	PrimaryScenario string `json:"primary_scenario"`
	SuccessCriteria string `json:"success_criteria"`
	LeadAvatarID    string `json:"lead_avatar_id,omitempty"`
	Status          string `json:"status"`
	UpdatedAt       string `json:"updated_at"`
}

// BaselineDispatchRules freezes the target team's free-form parallel dispatch
// policy (leg_timeout/group_deadline/quorum) at the pre-task state.
type BaselineDispatchRules struct {
	LegTimeoutSec    int `json:"leg_timeout_sec"`
	GroupDeadlineSec int `json:"group_deadline_sec"`
	Quorum           int `json:"quorum"`
}

// BaselineRosterEntry freezes one roster slot at the pre-task state. Role is
// "lead" for the team's lead avatar and "worker" for every team-worker
// relation; worker-only fields stay empty on the lead entry. The snapshot
// carries the exact mutable relation facts so a restore can rebuild the
// roster without consulting the live database.
type BaselineRosterEntry struct {
	AgentID            string   `json:"agent_id"`
	Role               string   `json:"role"`
	Duty               string   `json:"duty,omitempty"`
	WhenToUse          string   `json:"when_to_use,omitempty"`
	ContextInstruction string   `json:"context_instruction,omitempty"`
	AllowedKinds       []string `json:"allowed_kinds,omitempty"`
	DefaultKind        string   `json:"default_kind,omitempty"`
	ResultRequirement  string   `json:"result_requirement,omitempty"`
	Enabled            bool     `json:"enabled"`
}

// BaselineAgentPin pins one roster agent to an exact immutable version. The
// stable agent_id is the identity; name is captured for validation/display
// only. ContentHash is the sha256 of the frozen version's canonical spec.
type BaselineAgentPin struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	ContentHash string `json:"content_hash"`
	Engine      string `json:"engine,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	Model       string `json:"model,omitempty"`
}

// ExecutionPolicy returns the executor facts frozen with an immutable agent
// version. Empty and loom engines are the legacy in-process tool loop; every
// other supported engine is an external CLI runtime and must remain bound to
// the exact runtime captured by the authorization snapshot.
func (p BaselineAgentPin) ExecutionPolicy() (BlueprintExecutionPolicyV1, error) {
	engine := strings.TrimSpace(p.Engine)
	runtimeID := strings.TrimSpace(p.RuntimeID)
	if engine == "" || engine == "loom" {
		if runtimeID != "" {
			return BlueprintExecutionPolicyV1{}, errors.New("baseline standard agent cannot bind runtime_id")
		}
		return BlueprintExecutionPolicyV1{
			EngineClass: BlueprintEngineStandard, ExecutionMode: BlueprintExecutionToolLoop,
		}, nil
	}
	if engine != "codex" && engine != "opencode" && engine != "claude" {
		return BlueprintExecutionPolicyV1{}, fmt.Errorf("baseline agent has unsupported engine %q", engine)
	}
	if runtimeID == "" {
		return BlueprintExecutionPolicyV1{}, fmt.Errorf("baseline CLI agent %q is missing runtime_id", engine)
	}
	return BlueprintExecutionPolicyV1{
		EngineClass: BlueprintEngineCLI, Engine: engine,
		ExecutionMode: BlueprintExecutionRuntime, RuntimeRef: runtimeID,
	}, nil
}

// BaselineWorkflowDependency freezes one published dependency index row.
type BaselineWorkflowDependency struct {
	OwnerType         string `json:"owner_type"`
	OwnerID           string `json:"owner_id"`
	OwnerAgentVersion *int64 `json:"owner_agent_version,omitempty"`
	DependencyType    string `json:"dependency_type"`
	DependencyKey     string `json:"dependency_key"`
	DependencyVersion *int64 `json:"dependency_version,omitempty"`
	ContentHash       string `json:"content_hash"`
}

// BaselineWorkflowArtifact freezes the immutable published artifact envelope
// facts (metadata + content hash + payload).
type BaselineWorkflowArtifact struct {
	ArtifactSchemaVersion     int             `json:"artifact_schema_version"`
	CanonicalizationAlgorithm string          `json:"canonicalization_algorithm"`
	CanonicalizationVersion   int             `json:"canonicalization_version"`
	HashAlgorithm             string          `json:"hash_algorithm"`
	ContentHash               string          `json:"content_hash"`
	Payload                   json.RawMessage `json:"payload"`
}

// BaselineWorkflowPublished freezes one published version's exact
// trigger/graph, artifact, and dependency index. ContentHash is the sha256 of
// the published version's canonical trigger+graph document.
type BaselineWorkflowPublished struct {
	Version      int                          `json:"version"`
	Trigger      json.RawMessage              `json:"trigger"`
	Graph        json.RawMessage              `json:"graph"`
	ContentHash  string                       `json:"content_hash"`
	Artifact     BaselineWorkflowArtifact     `json:"artifact"`
	Dependencies []BaselineWorkflowDependency `json:"dependencies"`
}

// BaselineWorkflowDraft freezes the exact mutable draft trigger/graph and its
// canonical content hash. It is null when the workflow has no draft.
type BaselineWorkflowDraft struct {
	Version     int             `json:"version"`
	Trigger     json.RawMessage `json:"trigger"`
	Graph       json.RawMessage `json:"graph"`
	UpdatedAt   string          `json:"updated_at"`
	ContentHash string          `json:"content_hash"`
}

// BaselineWorkflowRef freezes one TeamWorkflow's identity plus its published
// and mutable-draft facts at the pre-task state.
type BaselineWorkflowRef struct {
	WorkflowID string                     `json:"workflow_id"`
	TeamID     string                     `json:"team_id"`
	Name       string                     `json:"name"`
	Status     string                     `json:"status"`
	UpdatedAt  string                     `json:"updated_at"`
	Published  *BaselineWorkflowPublished `json:"published,omitempty"`
	Draft      *BaselineWorkflowDraft     `json:"draft,omitempty"`
}

// BaselineSnapshot is the optimize-mode restore point frozen by the server
// before the task mutates anything (plan §7.2 / §10.4). It is captured inside
// the authorize transaction, stored as JSONB, and never changes once the run
// is authorized. The snapshot binds workspace, target team, and the exact
// AssetScope it was captured under; ContentHash is the sha256 of the canonical
// snapshot JSON (computed with the ContentHash field itself empty).
type BaselineSnapshot struct {
	SchemaVersion int                   `json:"schema_version"`
	WorkspaceID   string                `json:"workspace_id"`
	Team          BaselineTeamRef       `json:"team"`
	DispatchRules BaselineDispatchRules `json:"dispatch_rules"`
	Roster        []BaselineRosterEntry `json:"roster"`
	AgentPins     []BaselineAgentPin    `json:"agent_pins"`
	Workflows     []BaselineWorkflowRef `json:"workflows"`
	// WorkflowIdentities and WorkflowVersions are the complete mutable product
	// catalog facts used for the publication CAS. Immutable artifacts remain in
	// Workflows, so publication never reads the artifact store inside its tx.
	WorkflowIdentities []workflow.TeamWorkflow        `json:"workflow_identities"`
	WorkflowVersions   []workflow.TeamWorkflowVersion `json:"workflow_versions"`
	AssetScope         AssetScope                     `json:"asset_scope"`
	CapturedAt         string                         `json:"captured_at"`
	ContentHash        string                         `json:"content_hash,omitempty"`
}

// Hash returns the sha256 of the strict canonical snapshot JSON with the
// ContentHash field omitted. The result is the snapshot's overall canonical
// content hash and is bound to the exact frozen facts.
func (s BaselineSnapshot) Hash() (string, error) {
	value := s
	value.ContentHash = ""
	return hashDocument(value)
}

// RoundResult is the immutable outcome of one round: which frozen candidate
// was evaluated, the EvaluationReport hash, and the conclusion.
type RoundResult struct {
	CandidateRef string `json:"candidate_ref"`
	ReportRef    string `json:"report_ref"`
	Conclusion   string `json:"conclusion"`
}

// Round is one persisted round ledger row.
type Round struct {
	WorkspaceID  string
	BuildRunID   string
	RoundNo      int
	CandidateRef string
	ReportRef    string
	Conclusion   string
	CreatedAt    time.Time
}

// RoundReport is one immutable persisted EvaluationReport row. The row is
// content-addressed: ReportHash is the sha256 of the canonical report JSON
// and the row can never be updated or deleted after insertion.
type RoundReport struct {
	WorkspaceID string
	BuildRunID  string
	RoundNo     int
	ReportHash  string
	Report      EvaluationReport
	CreatedAt   time.Time
}

// FinalRef points at the final publication result.
type FinalRef struct {
	Ref     string `json:"ref"`
	AuditID string `json:"audit_id,omitempty"`
	TeamID  string `json:"team_id,omitempty"`
}

const (
	AuthorizationContinueBuild = "continue_build"
	AuthorizationAutoBuild     = "auto_build"
	AuthorizationTemplateAuto  = "template_auto"
	TemplateAuthorizerSubject  = "platform-template-authorizer"
)

type BlueprintRevisionToken struct {
	RevisionNo    int    `json:"revision_no"`
	BlueprintHash string `json:"blueprint_hash"`
	ChangeSetHash string `json:"change_set_hash"`
}

type BuildAuthorization struct {
	Authority       string                  `json:"authority,omitempty"`
	RevisionToken   *BlueprintRevisionToken `json:"revision_token,omitempty"`
	DecisionSubject string                  `json:"decision_subject,omitempty"`
	DecisionReason  string                  `json:"decision_reason,omitempty"`
}

type AuthorizeOptions struct {
	Authority       string
	RevisionToken   *BlueprintRevisionToken
	DecisionSubject string
	TemplatePolicy  *TemplateAuthorizationPolicy
}

// TemplateAuthorizationPolicy is retained in authorization receipts for
// backward compatibility. Deterministic template materialization does not
// consume or reserve the declared budget for later team runs.
type TemplateAuthorizationPolicy struct {
	AutoBudgetThresholdUSD float64
	DailyBudgetUSD         float64
	MonthlyBudgetUSD       float64
	MaxConcurrent          int
}

// TeamBuildRun is the persisted execution-control record (plan §6.0).
type TeamBuildRun struct {
	WorkspaceID       string
	BuildRunID        string
	Mode              string
	ExecutionStrategy string
	Status            string
	ConversationID    string
	EvaluationTeamID  string
	EvaluationOnly    bool
	Brief             BuildBrief
	BriefHash         string
	Contract          EvaluationContract
	ContractHash      string
	AssetScope        AssetScope
	Baseline          *BaselineSnapshot
	RoundBudget       Budget
	TotalBudget       Budget
	ExpiresAt         time.Time
	PublishEligible   bool
	RollbackStatus    string
	ConfirmedBy       string
	Authorization     BuildAuthorization
	FinalRef          *FinalRef
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DecidedAt         *time.Time
}

// EffectiveExecutionStrategy keeps older optimize records on the legacy
// executor while create-mode records default to the deterministic compiler.
func (run TeamBuildRun) EffectiveExecutionStrategy() string {
	if run.ExecutionStrategy == "" {
		if run.Mode == ModeCreate {
			return ExecutionStrategyCompilerV1
		}
		return ExecutionStrategyLegacy
	}
	return run.ExecutionStrategy
}

func validateBuildBrief(brief BuildBrief) error {
	if brief.SchemaVersion != schemaVersion {
		return fmt.Errorf("invalid build brief: schema_version must be %d", schemaVersion)
	}
	switch brief.Mode {
	case ModeCreate:
		if strings.TrimSpace(brief.NewTeamName) == "" {
			return errors.New("invalid build brief: create mode requires new_team_name")
		}
		if brief.TeamID != "" {
			return errors.New("invalid build brief: create mode must not carry team_id")
		}
	case ModeOptimize:
		if strings.TrimSpace(brief.TeamID) == "" {
			return errors.New("invalid build brief: optimize mode requires team_id")
		}
		if brief.NewTeamName != "" {
			return errors.New("invalid build brief: optimize mode must not carry new_team_name")
		}
	default:
		return errors.New("invalid build brief: mode must be create or optimize")
	}
	if strings.TrimSpace(brief.BusinessDirection) == "" {
		return errors.New("invalid build brief: business_direction is required")
	}
	if strings.TrimSpace(brief.Task) == "" {
		return errors.New("invalid build brief: task is required")
	}
	switch brief.EffectiveWorkflowBuildMode() {
	case WorkflowBuildModeBlueprint:
		if strings.TrimSpace(brief.TemplateGap) != "" {
			return errors.New("invalid build brief: template_gap requires workflow_build_mode custom")
		}
	case WorkflowBuildModeCustom:
		if strings.TrimSpace(brief.TemplateGap) == "" {
			return errors.New("invalid build brief: custom workflow build mode requires template_gap")
		}
	default:
		return errors.New("invalid build brief: workflow_build_mode must be blueprint or custom")
	}
	if len(brief.SuccessCriteria) == 0 {
		return errors.New("invalid build brief: success_criteria is required")
	}
	if _, err := normalizeGateWaivers(brief.WaivedGates); err != nil {
		return fmt.Errorf("invalid build brief: %w", err)
	}
	if waiver := brief.UnmeasuredUsageWaiver; waiver != nil {
		if !waiver.Accepted || strings.TrimSpace(waiver.Reason) == "" {
			return errors.New("invalid build brief: unmeasured_usage_waiver requires accepted=true and a reason")
		}
	}
	if err := validateAssetScope(brief.AllowedAssets); err != nil {
		return fmt.Errorf("invalid build brief: %w", err)
	}
	if brief.Mode == ModeOptimize {
		if err := validateOptimizeTargetInScope(brief.TeamID, brief.AllowedAssets); err != nil {
			return fmt.Errorf("invalid build brief: %w", err)
		}
	}
	if err := validateBudget(brief.RoundBudget); err != nil {
		return fmt.Errorf("invalid build brief round_budget: %w", err)
	}
	if err := validateBudget(brief.TotalBudget); err != nil {
		return fmt.Errorf("invalid build brief total_budget: %w", err)
	}
	return nil
}

func normalizeGateWaivers(waivers []GateWaiver) ([]GateWaiver, error) {
	normalized := make([]GateWaiver, 0, len(waivers))
	seen := make(map[string]bool, len(waivers))
	invalid := make([]string, 0)
	for _, waiver := range waivers {
		gateID := strings.TrimSpace(waiver.GateID)
		reason := strings.TrimSpace(waiver.Reason)
		switch {
		case gateID == "":
			invalid = append(invalid, "gate_id is required")
		case !knownEvaluationGateIDs[gateID]:
			invalid = append(invalid, fmt.Sprintf("unknown gate %q", gateID))
		case gateID == gatecodes.ScopeCompliance || gateID == gatecodes.NoGovernanceViolation:
			invalid = append(invalid, fmt.Sprintf("safety gate %q cannot be waived", gateID))
		case seen[gateID]:
			invalid = append(invalid, fmt.Sprintf("duplicate waiver for %q", gateID))
		case reason == "":
			invalid = append(invalid, fmt.Sprintf("waiver for %q requires a reason", gateID))
		default:
			seen[gateID] = true
			normalized = append(normalized, GateWaiver{GateID: gateID, Reason: reason})
		}
	}
	if len(invalid) > 0 {
		return nil, &ContractFloorViolation{InvalidWaivers: invalid}
	}
	return normalized, nil
}

// normalizeEvaluationContract recomputes every floor marker and copies the
// brief's reasoned waivers into the contract. injectMissing is true only for
// initial creation (and legacy drafts that predate the floor); draft updates
// must round-trip all server-injected floor gates and requirements.
func normalizeEvaluationContract(
	brief BuildBrief,
	contract EvaluationContract,
	injectMissing bool,
) (EvaluationContract, error) {
	waivers, err := normalizeGateWaivers(brief.WaivedGates)
	if err != nil {
		return EvaluationContract{}, err
	}
	contract.WaivedGates = waivers
	contract.HardGates = append([]HardGate(nil), contract.HardGates...)

	violations := &ContractFloorViolation{}
	byID := make(map[string]int, len(contract.HardGates))
	for i := range contract.HardGates {
		gate := &contract.HardGates[i]
		gate.Requirements = append([]string(nil), gate.Requirements...)
		if !injectMissing && gate.Floor && !IsContractFloorGate(gate.ID) {
			violations.InvalidFloorGateIDs = append(violations.InvalidFloorGateIDs, gate.ID)
		}
		gate.Floor = IsContractFloorGate(gate.ID)
		if _, duplicate := byID[gate.ID]; duplicate {
			return EvaluationContract{}, fmt.Errorf("invalid evaluation contract: duplicate hard gate id %q", gate.ID)
		}
		byID[gate.ID] = i
	}

	for _, floor := range DefaultFloorHardGates() {
		i, exists := byID[floor.ID]
		if !exists {
			if injectMissing {
				contract.HardGates = append(contract.HardGates, floor)
				continue
			}
			violations.MissingGateIDs = append(violations.MissingGateIDs, floor.ID)
			continue
		}
		gate := &contract.HardGates[i]
		if len(gate.Requirements) == 0 && injectMissing {
			gate.Requirements = append([]string(nil), floor.Requirements...)
			continue
		}
		if !containsAllStrings(gate.Requirements, floor.Requirements) {
			violations.WeakenedGateIDs = append(violations.WeakenedGateIDs, floor.ID)
		}
	}
	if len(violations.MissingGateIDs) > 0 || len(violations.WeakenedGateIDs) > 0 ||
		len(violations.InvalidFloorGateIDs) > 0 {
		return EvaluationContract{}, violations
	}
	if err := validateEvaluationContract(contract); err != nil {
		return EvaluationContract{}, err
	}
	return contract, nil
}

func containsAllStrings(actual, required []string) bool {
	set := make(map[string]bool, len(actual))
	for _, value := range actual {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func validateAssetScope(scope AssetScope) error {
	if len(scope.AllowedKinds) == 0 {
		return errors.New("asset scope must declare allowed_kinds")
	}
	for _, kind := range scope.AllowedKinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return errors.New("asset scope allowed_kinds must not contain empty values")
		}
		if !isBuildBriefAssetKind(kind) {
			return fmt.Errorf("asset scope allowed_kinds contains unsupported kind %q; allowed kinds: agent, team, workflow", kind)
		}
	}
	if (scope.NamePrefix == "") == (len(scope.Refs) == 0) {
		return errors.New("asset scope must declare exactly one of name_prefix or refs")
	}
	if scope.NamePrefix != "" && strings.TrimSpace(scope.NamePrefix) == "" {
		return errors.New("asset scope name_prefix must not be blank")
	}
	if strings.HasPrefix(scope.NamePrefix, "__") {
		return fmt.Errorf("%w: name_prefix %q", ErrBuiltinAssetInBrief, scope.NamePrefix)
	}
	for _, ref := range scope.Refs {
		if err := validateAssetRef(ref); err != nil {
			return fmt.Errorf("invalid asset scope: %w", err)
		}
	}
	return nil
}

func validateAssetRef(ref AssetRef) error {
	kind := strings.TrimSpace(ref.Kind)
	if kind == "" {
		return errors.New("invalid asset ref: kind is required")
	}
	if !isBuildBriefAssetKind(kind) {
		return fmt.Errorf("invalid asset ref: kind %q is unsupported; allowed kinds: agent, team, workflow", kind)
	}
	if strings.TrimSpace(ref.ID) == "" && strings.TrimSpace(ref.Name) == "" {
		return errors.New("invalid asset ref: id or name is required")
	}
	if strings.HasPrefix(ref.Name, "__") {
		return fmt.Errorf("%w: name %q", ErrBuiltinAssetInBrief, ref.Name)
	}
	return nil
}

func validateOptimizeTargetInScope(teamID string, scope AssetScope) error {
	target := strings.TrimSpace(teamID)
	for _, ref := range scope.Refs {
		if strings.TrimSpace(ref.Kind) != "team" {
			continue
		}
		if strings.TrimSpace(ref.ID) == target {
			return nil
		}
	}
	return errors.New("optimize mode team_id must match an allowed_assets team ref id from tf_list_teams; do not use display names")
}

func isBuildBriefAssetKind(kind string) bool {
	_, ok := buildBriefAssetKinds[kind]
	return ok
}

func validateBudget(budget Budget) error {
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 ||
		budget.MaxToolCalls < 0 {
		return errors.New("budget bounds must not be negative")
	}
	if budget.MaxCostUSD < 0 || math.IsNaN(budget.MaxCostUSD) ||
		math.IsInf(budget.MaxCostUSD, 0) {
		return errors.New("budget max_cost_usd must be finite and non-negative")
	}
	if budget.MaxInputTokens == 0 && budget.MaxOutputTokens == 0 &&
		budget.MaxToolCalls == 0 && budget.MaxCostUSD == 0 {
		return errors.New("budget must declare at least one positive bound")
	}
	return nil
}

func validateEvaluationContract(contract EvaluationContract) error {
	if contract.SchemaVersion != 1 && contract.SchemaVersion != 2 {
		return errors.New("invalid evaluation contract: schema_version must be 1 or 2")
	}
	if contract.MaxIterations != maxContractIterations {
		return fmt.Errorf("invalid evaluation contract: max_iterations must be %d", maxContractIterations)
	}
	if contract.RunCount < 1 {
		return errors.New("invalid evaluation contract: run_count must be positive")
	}
	if contract.HiddenScenarioCount < 0 {
		return errors.New("invalid evaluation contract: hidden_scenario_count must not be negative")
	}
	if contract.CostCapUSD < 0 {
		return errors.New("invalid evaluation contract: cost_cap_usd must not be negative")
	}
	if len(contract.HardGates) == 0 {
		return errors.New("invalid evaluation contract: hard_gates are required")
	}
	seenGateIDs := make(map[string]bool, len(contract.HardGates))
	byID := make(map[string]HardGate, len(contract.HardGates))
	invalidFloorMarkers := make([]string, 0)
	for _, gate := range contract.HardGates {
		if strings.TrimSpace(gate.ID) == "" || strings.TrimSpace(gate.Check) == "" {
			return errors.New("invalid evaluation contract: hard gate id and check are required")
		}
		if seenGateIDs[gate.ID] {
			return fmt.Errorf("invalid evaluation contract: duplicate hard gate id %q", gate.ID)
		}
		seenGateIDs[gate.ID] = true
		byID[gate.ID] = gate
		if gate.Floor != IsContractFloorGate(gate.ID) {
			invalidFloorMarkers = append(invalidFloorMarkers, gate.ID)
		}
	}
	floorViolation := &ContractFloorViolation{InvalidFloorGateIDs: invalidFloorMarkers}
	for _, floor := range DefaultFloorHardGates() {
		gate, exists := byID[floor.ID]
		if !exists {
			floorViolation.MissingGateIDs = append(floorViolation.MissingGateIDs, floor.ID)
			continue
		}
		if !containsAllStrings(gate.Requirements, floor.Requirements) {
			floorViolation.WeakenedGateIDs = append(floorViolation.WeakenedGateIDs, floor.ID)
		}
	}
	if len(floorViolation.MissingGateIDs) > 0 || len(floorViolation.WeakenedGateIDs) > 0 ||
		len(floorViolation.InvalidFloorGateIDs) > 0 {
		return floorViolation
	}
	if _, err := normalizeGateWaivers(contract.WaivedGates); err != nil {
		return err
	}
	if len(contract.Rubric) == 0 {
		return errors.New("invalid evaluation contract: rubric is required")
	}
	for _, dimension := range contract.Rubric {
		if strings.TrimSpace(dimension.ID) == "" || dimension.MaxScore <= 0 ||
			dimension.PassThreshold < 0 || dimension.PassThreshold > dimension.MaxScore {
			return errors.New("invalid evaluation contract: rubric dimension thresholds are out of range")
		}
	}
	if len(contract.PublicScenarios) == 0 {
		return errors.New("invalid evaluation contract: public_scenarios are required")
	}
	publicByID := make(map[string]Scenario, len(contract.PublicScenarios))
	for _, scenario := range contract.PublicScenarios {
		id := strings.TrimSpace(scenario.ID)
		if id == "" || strings.TrimSpace(scenario.Input) == "" || strings.TrimSpace(scenario.Expected) == "" {
			return errors.New("invalid evaluation contract: public scenario id, input, and expected are required")
		}
		if _, exists := publicByID[id]; exists {
			return fmt.Errorf("invalid evaluation contract: duplicate public scenario id %q", id)
		}
		publicByID[id] = scenario
	}
	// Schema v1 is retained only so already frozen BuildRuns can still be read
	// and their original hashes verified. New submission is v2; it must freeze
	// concrete mutations rather than executing abstract rules as prompts.
	if contract.SchemaVersion == 1 {
		return validateEvaluationContractTail(contract)
	}
	if len(contract.PerturbationRules) == 0 {
		return errors.New("invalid evaluation contract: perturbation_rules are required")
	}
	rules := make(map[string]bool, len(contract.PerturbationRules))
	for _, rule := range contract.PerturbationRules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			return errors.New("invalid evaluation contract: perturbation rule must not be empty")
		}
		if rules[rule] {
			return fmt.Errorf("invalid evaluation contract: duplicate perturbation rule %q", rule)
		}
		rules[rule] = true
	}
	wantPerturbations := len(contract.PublicScenarios) * len(contract.PerturbationRules)
	if len(contract.PerturbationScenarios) != wantPerturbations {
		return fmt.Errorf(
			"invalid evaluation contract: perturbation_scenarios must materialize every public-scenario/rule pair (%d required, got %d)",
			wantPerturbations, len(contract.PerturbationScenarios),
		)
	}
	seenScenarioIDs := make(map[string]bool, len(contract.PublicScenarios)+len(contract.PerturbationScenarios))
	for id := range publicByID {
		seenScenarioIDs[id] = true
	}
	seenPairs := make(map[string]bool, wantPerturbations)
	for _, scenario := range contract.PerturbationScenarios {
		id := strings.TrimSpace(scenario.ID)
		baseID := strings.TrimSpace(scenario.BaseScenarioID)
		rule := strings.TrimSpace(scenario.Rule)
		base, baseExists := publicByID[baseID]
		if id == "" || !baseExists || !rules[rule] || strings.TrimSpace(scenario.Input) == "" || strings.TrimSpace(scenario.Expected) == "" {
			return errors.New("invalid evaluation contract: perturbation scenario requires a unique id, known base_scenario_id/rule, input, and expected")
		}
		if seenScenarioIDs[id] {
			return fmt.Errorf("invalid evaluation contract: duplicate scenario id %q", id)
		}
		seenScenarioIDs[id] = true
		pair := baseID + "\x00" + rule
		if seenPairs[pair] {
			return fmt.Errorf("invalid evaluation contract: duplicate perturbation pair for base %q", baseID)
		}
		seenPairs[pair] = true
		if strings.TrimSpace(scenario.Input) == strings.TrimSpace(base.Input) {
			return fmt.Errorf("invalid evaluation contract: perturbation scenario %q does not change its public input", id)
		}
	}
	for baseID := range publicByID {
		for rule := range rules {
			if !seenPairs[baseID+"\x00"+rule] {
				return fmt.Errorf("invalid evaluation contract: missing perturbation for base %q and rule %q", baseID, rule)
			}
		}
	}
	return validateEvaluationContractTail(contract)
}

func validateEvaluationContractTail(contract EvaluationContract) error {
	if strings.TrimSpace(contract.SevereDefectDefinition) == "" {
		return errors.New("invalid evaluation contract: severe_defect_definition is required")
	}
	if len(contract.PassRules) == 0 || len(contract.BlockRules) == 0 ||
		len(contract.InfraFailureRules) == 0 {
		return errors.New("invalid evaluation contract: pass_rules, block_rules, and infra_failure_rules are required")
	}
	return nil
}

func validateEvaluationReport(report EvaluationReport) error {
	if report.SchemaVersion != schemaVersion {
		return fmt.Errorf("invalid evaluation report: schema_version must be %d", schemaVersion)
	}
	if report.RoundNo < 1 {
		return errors.New("invalid evaluation report: round_no must be positive")
	}
	switch report.Conclusion {
	case ConclusionPass, ConclusionRevise, ConclusionBlocked:
	default:
		return errors.New("invalid evaluation report: conclusion must be pass, revise, or blocked")
	}
	if report.LatencyMs < 0 || report.InputTokens < 0 || report.OutputTokens < 0 ||
		report.Tokens < 0 || report.ToolCalls < 0 {
		return errors.New("invalid evaluation report: usage fields must not be negative")
	}
	if report.CostUSD < 0 || math.IsNaN(report.CostUSD) || math.IsInf(report.CostUSD, 0) {
		return errors.New("invalid evaluation report: cost_usd must be finite and non-negative")
	}
	// New-style reports carry the split and must keep the legacy summary
	// consistent; a legacy report that only carries Tokens stays valid.
	if (report.InputTokens != 0 || report.OutputTokens != 0) &&
		report.Tokens != report.InputTokens+report.OutputTokens {
		return errors.New(
			"invalid evaluation report: tokens must equal input_tokens + output_tokens",
		)
	}
	if report.UsageComplete != nil && !*report.UsageComplete &&
		report.UsageIncompleteReason == "" {
		return errors.New(
			"invalid evaluation report: usage_incomplete_reason is required when usage_complete is false",
		)
	}
	if report.UsageIncompleteReason != "" &&
		(report.UsageComplete == nil || *report.UsageComplete) {
		return errors.New(
			"invalid evaluation report: usage_incomplete_reason requires usage_complete=false",
		)
	}
	if (report.UsageHasTokens == nil) != (report.UsageHasCost == nil) {
		return errors.New(
			"invalid evaluation report: usage_has_tokens and usage_has_cost must be present together",
		)
	}
	if report.UsageHasTokens != nil && (!*report.UsageHasTokens || !*report.UsageHasCost) &&
		(report.UsageComplete == nil || *report.UsageComplete) {
		return errors.New(
			"invalid evaluation report: incomplete usage dimension requires usage_complete=false",
		)
	}
	seenUsageSources := make(map[string]bool, len(report.UsageSources))
	for _, source := range report.UsageSources {
		if strings.TrimSpace(source) == "" || seenUsageSources[source] {
			return errors.New("invalid evaluation report: usage_sources must be non-empty and unique")
		}
		seenUsageSources[source] = true
	}
	for _, result := range report.HardGateResults {
		if strings.TrimSpace(result.GateID) == "" {
			return errors.New("invalid evaluation report: hard gate result gate_id is required")
		}
	}
	for _, result := range report.ScenarioResults {
		if strings.TrimSpace(result.ScenarioID) == "" || strings.TrimSpace(result.RunID) == "" {
			return errors.New("invalid evaluation report: scenario result scenario_id and run_id are required")
		}
	}
	for _, score := range report.RubricScores {
		if strings.TrimSpace(score.DimensionID) == "" || score.Score < 0 {
			return errors.New("invalid evaluation report: rubric score dimension and non-negative score are required")
		}
	}
	return nil
}

func validateBaselineSnapshot(snapshot BaselineSnapshot) error {
	if snapshot.SchemaVersion != schemaVersion {
		return fmt.Errorf("invalid baseline snapshot: schema_version must be %d", schemaVersion)
	}
	if strings.TrimSpace(snapshot.WorkspaceID) == "" {
		return errors.New("invalid baseline snapshot: workspace_id is required")
	}
	if strings.TrimSpace(snapshot.Team.TeamID) == "" {
		return errors.New("invalid baseline snapshot: team_id is required")
	}
	if strings.TrimSpace(snapshot.Team.WorkspaceID) == "" {
		return errors.New("invalid baseline snapshot: team workspace_id is required")
	}
	if strings.TrimSpace(snapshot.Team.Name) == "" {
		return errors.New("invalid baseline snapshot: team name is required")
	}
	if strings.TrimSpace(snapshot.Team.Status) == "" {
		return errors.New("invalid baseline snapshot: team status is required")
	}
	if strings.TrimSpace(snapshot.Team.UpdatedAt) == "" {
		return errors.New("invalid baseline snapshot: team updated_at is required")
	}
	if snapshot.DispatchRules.LegTimeoutSec < 0 ||
		snapshot.DispatchRules.GroupDeadlineSec < 0 ||
		snapshot.DispatchRules.Quorum < 0 {
		return errors.New("invalid baseline snapshot: dispatch rule values must be non-negative")
	}
	if strings.TrimSpace(snapshot.CapturedAt) == "" {
		return errors.New("invalid baseline snapshot: captured_at is required")
	}
	if len(snapshot.Roster) == 0 {
		return errors.New("invalid baseline snapshot: roster is required")
	}
	leadSeen := false
	workerSeen := make(map[string]struct{}, len(snapshot.Roster))
	rosterSeen := make(map[string]struct{}, len(snapshot.Roster))
	enabledWorkers := 0
	for _, entry := range snapshot.Roster {
		if strings.TrimSpace(entry.AgentID) == "" {
			return errors.New("invalid baseline snapshot: roster entry agent_id is required")
		}
		if _, duplicate := rosterSeen[entry.AgentID]; duplicate {
			return fmt.Errorf(
				"invalid baseline snapshot: roster agent %q appears more than once",
				entry.AgentID,
			)
		}
		rosterSeen[entry.AgentID] = struct{}{}
		switch entry.Role {
		case "lead":
			if leadSeen {
				return errors.New("invalid baseline snapshot: roster must contain exactly one lead")
			}
			leadSeen = true
		case "worker":
			if _, duplicate := workerSeen[entry.AgentID]; duplicate {
				return fmt.Errorf(
					"invalid baseline snapshot: worker agent %q appears more than once",
					entry.AgentID,
				)
			}
			workerSeen[entry.AgentID] = struct{}{}
			if entry.Enabled {
				enabledWorkers++
			}
		default:
			return fmt.Errorf(
				"invalid baseline snapshot: roster role must be lead or worker, got %q",
				entry.Role,
			)
		}
		if entry.Role == "worker" && len(entry.AllowedKinds) == 0 {
			return fmt.Errorf(
				"invalid baseline snapshot: worker %q must declare allowed_kinds",
				entry.AgentID,
			)
		}
	}
	if !leadSeen {
		return errors.New("invalid baseline snapshot: roster must contain exactly one lead")
	}
	if enabledWorkers == 0 {
		return errors.New("invalid baseline snapshot: roster must contain at least one enabled worker")
	}
	pinSeen := make(map[string]struct{}, len(snapshot.AgentPins))
	for _, pin := range snapshot.AgentPins {
		if strings.TrimSpace(pin.AgentID) == "" {
			return errors.New("invalid baseline snapshot: agent pin agent_id is required")
		}
		if _, duplicate := pinSeen[pin.AgentID]; duplicate {
			return fmt.Errorf("invalid baseline snapshot: agent pin %q appears more than once", pin.AgentID)
		}
		pinSeen[pin.AgentID] = struct{}{}
		if strings.TrimSpace(pin.Name) == "" || pin.Version < 1 {
			return errors.New(
				"invalid baseline snapshot: agent pin name and positive version are required",
			)
		}
		if !validContentHash(pin.ContentHash) {
			return fmt.Errorf(
				"invalid baseline snapshot: agent pin %q content_hash is invalid",
				pin.AgentID,
			)
		}
	}
	for agentID := range rosterSeen {
		if _, pinned := pinSeen[agentID]; !pinned {
			return fmt.Errorf(
				"invalid baseline snapshot: roster agent %q has no version pin",
				agentID,
			)
		}
	}
	for pinID := range pinSeen {
		if _, inRoster := rosterSeen[pinID]; !inRoster {
			return fmt.Errorf(
				"invalid baseline snapshot: agent pin %q has no roster entry",
				pinID,
			)
		}
	}
	workflowSeen := make(map[string]struct{}, len(snapshot.Workflows))
	for _, workflow := range snapshot.Workflows {
		if strings.TrimSpace(workflow.WorkflowID) == "" {
			return errors.New("invalid baseline snapshot: workflow id is required")
		}
		if _, duplicate := workflowSeen[workflow.WorkflowID]; duplicate {
			return fmt.Errorf(
				"invalid baseline snapshot: workflow %q appears more than once",
				workflow.WorkflowID,
			)
		}
		workflowSeen[workflow.WorkflowID] = struct{}{}
		if strings.TrimSpace(workflow.TeamID) == "" ||
			strings.TrimSpace(workflow.Name) == "" ||
			strings.TrimSpace(workflow.Status) == "" ||
			strings.TrimSpace(workflow.UpdatedAt) == "" {
			return fmt.Errorf(
				"invalid baseline snapshot: workflow %q identity is incomplete",
				workflow.WorkflowID,
			)
		}
		if workflow.Published != nil {
			if workflow.Published.Version < 1 {
				return fmt.Errorf(
					"invalid baseline snapshot: workflow %q published version must be positive",
					workflow.WorkflowID,
				)
			}
			if !validContentHash(workflow.Published.ContentHash) ||
				!validContentHash(workflow.Published.Artifact.ContentHash) {
				return fmt.Errorf(
					"invalid baseline snapshot: workflow %q published content hash is invalid",
					workflow.WorkflowID,
				)
			}
			if workflow.Published.Artifact.ArtifactSchemaVersion < 1 ||
				strings.TrimSpace(workflow.Published.Artifact.CanonicalizationAlgorithm) == "" ||
				strings.TrimSpace(workflow.Published.Artifact.HashAlgorithm) == "" ||
				len(workflow.Published.Artifact.Payload) == 0 {
				return fmt.Errorf(
					"invalid baseline snapshot: workflow %q published artifact is incomplete",
					workflow.WorkflowID,
				)
			}
			for _, dependency := range workflow.Published.Dependencies {
				if strings.TrimSpace(dependency.OwnerType) == "" ||
					strings.TrimSpace(dependency.OwnerID) == "" ||
					strings.TrimSpace(dependency.DependencyType) == "" ||
					strings.TrimSpace(dependency.DependencyKey) == "" ||
					!validContentHash(dependency.ContentHash) {
					return fmt.Errorf(
						"invalid baseline snapshot: workflow %q dependency is incomplete",
						workflow.WorkflowID,
					)
				}
			}
		}
		if workflow.Draft != nil {
			if workflow.Draft.Version < 1 ||
				strings.TrimSpace(workflow.Draft.UpdatedAt) == "" ||
				!validContentHash(workflow.Draft.ContentHash) ||
				len(workflow.Draft.Trigger) == 0 ||
				len(workflow.Draft.Graph) == 0 {
				return fmt.Errorf(
					"invalid baseline snapshot: workflow %q draft is incomplete",
					workflow.WorkflowID,
				)
			}
		}
	}
	if err := validateAssetScope(snapshot.AssetScope); err != nil {
		return fmt.Errorf("invalid baseline snapshot asset scope: %w", err)
	}
	if snapshot.ContentHash != "" {
		if !validContentHash(snapshot.ContentHash) {
			return errors.New("invalid baseline snapshot: content_hash is invalid")
		}
		recomputed, err := snapshot.Hash()
		if err != nil {
			return fmt.Errorf("invalid baseline snapshot: recompute content hash: %w", err)
		}
		if snapshot.ContentHash != recomputed {
			return errors.New("invalid baseline snapshot: content_hash does not match frozen content")
		}
	}
	return nil
}

// validateBaselineClosure enforces the optimize-mode closure rules: the
// baseline target team must equal the brief's team, every baseline asset must
// be exactly covered by the brief's AssetScope (and vice versa for the
// baseline kinds), every workflow must belong to the target team, agent pins
// must never reference built-in "__" assets, and duplicates/cross-workspace/
// weak references fail closed.
func validateBaselineClosure(
	snapshot BaselineSnapshot,
	brief BuildBrief,
	workspaceID string,
) error {
	if brief.Mode != ModeOptimize {
		return nil
	}
	if snapshot.Team.TeamID != brief.TeamID {
		return fmt.Errorf(
			"%w: baseline target team %q does not match brief team %q",
			ErrBaselineScopeNotClosed,
			snapshot.Team.TeamID,
			brief.TeamID,
		)
	}
	if snapshot.Team.WorkspaceID != workspaceID {
		return fmt.Errorf(
			"%w: baseline team workspace %q does not match run workspace %q",
			ErrBaselineScopeNotClosed,
			snapshot.Team.WorkspaceID,
			workspaceID,
		)
	}

	scope := brief.AllowedAssets
	if !scope.Contains(AssetRef{Kind: "team", ID: snapshot.Team.TeamID}) {
		return fmt.Errorf(
			"%w: target team %q is not covered by the asset scope",
			ErrBaselineScopeNotClosed,
			snapshot.Team.TeamID,
		)
	}
	baselineAgents := make(map[string]struct{}, len(snapshot.Roster))
	for _, entry := range snapshot.Roster {
		baselineAgents[entry.AgentID] = struct{}{}
		if !scope.Contains(AssetRef{Kind: "agent", ID: entry.AgentID}) {
			return fmt.Errorf(
				"%w: roster agent %q is not covered by the asset scope",
				ErrBaselineScopeNotClosed,
				entry.AgentID,
			)
		}
	}
	baselineWorkflows := make(map[string]struct{}, len(snapshot.Workflows))
	for _, workflow := range snapshot.Workflows {
		baselineWorkflows[workflow.WorkflowID] = struct{}{}
		if workflow.TeamID != brief.TeamID {
			return fmt.Errorf(
				"%w: workflow %q belongs to team %q, not the target team %q",
				ErrBaselineScopeNotClosed,
				workflow.WorkflowID,
				workflow.TeamID,
				brief.TeamID,
			)
		}
		if !scope.Contains(AssetRef{Kind: "workflow", ID: workflow.WorkflowID}) {
			return fmt.Errorf(
				"%w: workflow %q is not covered by the asset scope",
				ErrBaselineScopeNotClosed,
				workflow.WorkflowID,
			)
		}
	}

	scopedRefs := make(map[[2]string]struct{}, len(scope.Refs))
	for _, ref := range scope.Refs {
		switch ref.Kind {
		case "team", "agent", "workflow":
			if ref.ID == "" {
				return fmt.Errorf(
					"%w: %s scope reference without a stable id is not allowed in optimize mode",
					ErrBaselineScopeNotClosed,
					ref.Kind,
				)
			}
			key := [2]string{ref.Kind, ref.ID}
			if _, duplicate := scopedRefs[key]; duplicate {
				return fmt.Errorf(
					"%w: duplicate %s scope reference %q",
					ErrBaselineScopeNotClosed,
					ref.Kind,
					ref.ID,
				)
			}
			scopedRefs[key] = struct{}{}
			switch ref.Kind {
			case "team":
				if ref.ID != snapshot.Team.TeamID {
					return fmt.Errorf(
						"%w: %s scope reference %q is not a baseline asset",
						ErrBaselineScopeNotClosed,
						ref.Kind,
						ref.ID,
					)
				}
			case "agent":
				if _, exists := baselineAgents[ref.ID]; !exists {
					return fmt.Errorf(
						"%w: %s scope reference %q is not a baseline roster asset",
						ErrBaselineScopeNotClosed,
						ref.Kind,
						ref.ID,
					)
				}
			case "workflow":
				if _, exists := baselineWorkflows[ref.ID]; !exists {
					if len(baselineWorkflows) == 0 && ref.ID == FirstOptimizeWorkflowID(snapshot.Team.TeamID) {
						break
					}
					return fmt.Errorf(
						"%w: %s scope reference %q is not a baseline workflow",
						ErrBaselineScopeNotClosed,
						ref.Kind,
						ref.ID,
					)
				}
			}
		}
	}

	for _, pin := range snapshot.AgentPins {
		if strings.HasPrefix(pin.Name, "__") {
			return fmt.Errorf(
				"%w: agent pin %q references built-in platform asset %q",
				ErrBaselineSnapshotInvalid,
				pin.AgentID,
				pin.Name,
			)
		}
	}
	return nil
}

func validContentHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func hashDocument(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal build document: %w", err)
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize build document: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func cloneAssetScope(scope AssetScope) AssetScope {
	cloned := scope
	cloned.AllowedKinds = append([]string(nil), scope.AllowedKinds...)
	cloned.Refs = append([]AssetRef(nil), scope.Refs...)
	return cloned
}
