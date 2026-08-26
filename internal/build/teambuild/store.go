package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

var (
	ErrBuildRunNotFound         = errors.New("build run not found")
	ErrBuildRunNotPlanning      = errors.New("build run is not in planning")
	ErrBuildRunNotRoundRunning  = errors.New("build run is not round_running")
	ErrBuildRunDraftInvalid     = errors.New("build run draft is invalid")
	ErrInvalidTransition        = errors.New("invalid build run transition")
	ErrInvalidRoundConclusion   = errors.New("invalid round conclusion")
	ErrRoundReportNotFound      = errors.New("round report not found")
	ErrRoundReportExists        = errors.New("round report already exists")
	ErrRoundReportInvalid       = errors.New("round report is invalid")
	ErrOptimizeBaselineRequired = errors.New("optimize build run requires a baseline snapshot")
	ErrBaselineNotAllowed       = errors.New("create build run cannot carry a baseline snapshot")
	// ErrBuildRunNotOptimize rejects rollback of a create-mode run: only the
	// server-frozen optimize baseline is a valid restore source.
	ErrBuildRunNotOptimize = errors.New("build run is not optimize mode")
	// ErrBuildRunNotTerminal rejects rollback before the run reached one of the
	// terminal statuses (passed, blocked, cancelled).
	ErrBuildRunNotTerminal = errors.New("build run is not in a terminal status")
	// ErrBuildRunRollbackClaimed reports that the run's one-shot rollback was
	// already claimed (in progress), completed (rolled_back), or failed
	// (rollback_failed). A run is never rolled back twice.
	ErrBuildRunRollbackClaimed = errors.New("build run rollback already claimed or completed")
	// ErrBaselineSourceUnavailable reports that the server-side baseline
	// builder is not bound to the exact-read stores it needs.
	ErrBaselineSourceUnavailable = errors.New("baseline snapshot sources are unavailable")
	// ErrBaselineTargetNotFound reports that the optimize brief references a
	// target team that does not exist (or is not readable) in the workspace.
	ErrBaselineTargetNotFound = errors.New("baseline target team not found")
	// ErrBaselineScopeNotClosed reports that the baseline assets are not
	// exactly covered by the frozen AssetScope (missing asset, weak reference,
	// duplicate, or cross-workspace target).
	ErrBaselineScopeNotClosed = errors.New("baseline assets are not exactly covered by the asset scope")
	// ErrBaselineSnapshotInvalid reports that the captured baseline violates a
	// structural or closure invariant (missing lead, duplicate worker, built-in
	// asset pin, or content-hash mismatch).
	ErrBaselineSnapshotInvalid = errors.New("baseline snapshot is invalid")
	// ErrBaselineSnapshotFailed reports an infrastructure failure while
	// reading the live state the snapshot depends on.
	ErrBaselineSnapshotFailed = errors.New("baseline snapshot capture failed")
	// ErrCompilerRevisionRequired reports that a create-mode build run cannot
	// be authorized until its deterministic compiler blueprint is persisted.
	ErrCompilerRevisionRequired = errors.New("compiler blueprint revision is required")
	// ErrBlueprintRevisionMismatch reports that an authorization referenced a
	// blueprint revision that is not the latest executable compiler plan.
	ErrBlueprintRevisionMismatch = errors.New("blueprint revision authorization token mismatch")

	ErrReceiptInvalid              = errors.New("build authorization receipt is invalid")
	ErrReceiptWorkspaceMismatch    = errors.New("build authorization receipt workspace mismatch")
	ErrReceiptExpired              = errors.New("build authorization receipt expired")
	ErrReceiptContractHashMismatch = errors.New("build authorization receipt contract hash mismatch")
	ErrReceiptBriefHashMismatch    = errors.New("build authorization receipt brief hash mismatch")
	ErrReceiptModeMismatch         = errors.New("build authorization receipt mode mismatch")
	ErrReceiptBudgetMismatch       = errors.New("build authorization receipt budget mismatch")
	ErrReceiptExpiryMismatch       = errors.New("build authorization receipt expiry mismatch")
	ErrReceiptTerminal             = errors.New("build authorization receipt is invalid after terminal status")
	ErrReceiptNotIssued            = errors.New("build run has not been authorized yet")
	ErrReceiptAssetOutOfScope      = errors.New("asset is outside the build authorization scope")
	// ErrBudgetUsageConflict reports a non-idempotent replay of one budget
	// source identity: the source already has a ledger row with different
	// facts. The original row is never modified.
	ErrBudgetUsageConflict = errors.New("budget usage conflict")
	// ErrUsageSourceConflict reports a non-idempotent replay of one usage
	// source identity: the identity already exists with a different role.
	// The original association row is never modified.
	ErrUsageSourceConflict = errors.New("usage source conflict")
	// ErrActiveTeamEvaluation reports the database-enforced single active
	// evaluation run for one team.
	ErrActiveTeamEvaluation = errors.New("team already has an active evaluation run")
)

// Clock supplies timestamps for build run transitions.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Store persists TeamBuildRun control records and mints build authorization
// receipts.
type Store struct {
	pool       *pgxpool.Pool
	clock      Clock
	baselineMu sync.RWMutex
	orgStore   *org.Store
	agents     *registry.AgentRegistry
	workflows  *workflow.Store
}

// BuildRunFilter constrains the BuildRun list projection. Limit defaults to
// 50 and is capped at 200; rows are always returned by updated_at descending.
type BuildRunFilter struct {
	Status string
	Mode   string
	Limit  int
	Offset int
}

// TeamBuildRunSummary is the list projection for BuildRun back-office views.
// It deliberately excludes the large Brief and Contract JSON documents.
type TeamBuildRunSummary struct {
	BuildRunID            string
	Mode                  string
	Status                string
	ConversationID        string
	TargetTeamID          string
	TargetTeamName        string
	NewTeamName           string
	CurrentRound          int
	MaxRounds             int
	LatestConclusion      string
	LatestFailureCategory string
	BudgetUsage           BudgetUsage
	PublishEligible       bool
	RollbackStatus        string
	FinalRef              *FinalRef
	ConfirmedBy           string
	ExpiresAt             time.Time
	UpdatedAt             time.Time
}

// New creates a teambuild store. A nil clock falls back to RealClock.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

const buildRunColumns = `
	workspace_id, build_run_id, mode, execution_strategy, status, conversation_id,
	evaluation_team_id,
	brief_json, brief_hash, contract_json, contract_hash,
	asset_scope_json, baseline_snapshot_json,
	round_budget_json, total_budget_json,
	expires_at, publish_eligible, rollback_status,
	confirmed_by, authorization_authority, authorized_revision_no,
	authorized_blueprint_hash, authorized_change_set_hash,
	authorization_decision_subject, authorization_decision_reason,
	final_ref_json, created_at, updated_at, decided_at
`

// CreateRunParams carries the initial drafts of one build task.
type CreateRunParams struct {
	Brief             BuildBrief
	Contract          EvaluationContract
	ExpiresAt         time.Time
	CreatedBy         string
	ExecutionStrategy string
	// ConversationID optionally binds the control record to the chat
	// conversation that drove the build task. It is nullable and does not
	// constrain the run lifecycle.
	ConversationID string
	// EvaluationTeamID marks the dedicated post-template evaluation run. It is
	// persisted separately from brief.team_id so concurrency can be enforced by
	// a partial unique index without classifying ordinary optimize runs.
	EvaluationTeamID string
}

// ValidateBuildRunDrafts validates and canonicalizes the two documents used
// to create a build run. The returned contract is the exact server-normalized
// document persisted by CreateBuildRun, so callers can perform hash-based
// idempotency checks without duplicating validation rules.
func ValidateBuildRunDrafts(
	brief BuildBrief,
	contract EvaluationContract,
) (string, EvaluationContract, string, error) {
	briefHash, err := brief.Hash()
	if err != nil {
		return "", EvaluationContract{}, "", fmt.Errorf("%w: %w", ErrBuildRunDraftInvalid, err)
	}
	normalizedContract, err := normalizeEvaluationContract(brief, contract, true)
	if err != nil {
		return "", EvaluationContract{}, "", fmt.Errorf("%w: %w", ErrBuildRunDraftInvalid, err)
	}
	contractHash, err := normalizedContract.Hash()
	if err != nil {
		return "", EvaluationContract{}, "", fmt.Errorf("%w: %w", ErrBuildRunDraftInvalid, err)
	}
	return briefHash, normalizedContract, contractHash, nil
}

// CreateBuildRun persists a new run in planning with its initial drafts and
// the genesis transition.
func (s *Store) CreateBuildRun(
	ctx context.Context,
	workspaceID, buildRunID string,
	params CreateRunParams,
) (TeamBuildRun, error) {
	if workspaceID == "" || buildRunID == "" {
		return TeamBuildRun{}, errors.New("create build run: workspace_id and build_run_id are required")
	}
	if strings.TrimSpace(params.CreatedBy) == "" {
		return TeamBuildRun{}, errors.New("create build run: created_by is required")
	}
	var err error
	params.Brief = expandCreateAssetScope(params.Brief)
	params.Brief, err = s.expandOptimizeAssetScope(ctx, workspaceID, params.Brief)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: %w", err)
	}
	briefHash, normalizedContract, contractHash, err := ValidateBuildRunDrafts(params.Brief, params.Contract)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: %w", err)
	}
	briefJSON, err := json.Marshal(params.Brief)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: encode brief: %w", err)
	}
	contractJSON, err := json.Marshal(normalizedContract)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: encode contract: %w", err)
	}
	scopeJSON, err := json.Marshal(params.Brief.AllowedAssets)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: encode asset scope: %w", err)
	}
	roundBudgetJSON, err := json.Marshal(params.Brief.RoundBudget)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: encode round budget: %w", err)
	}
	totalBudgetJSON, err := json.Marshal(params.Brief.TotalBudget)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: encode total budget: %w", err)
	}
	executionStrategy, err := initialExecutionStrategy(params.Brief.Mode, params.ExecutionStrategy)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run: %w", err)
	}

	now := s.clock.Now()
	if params.ExpiresAt.IsZero() || !params.ExpiresAt.After(now) {
		return TeamBuildRun{}, fmt.Errorf("create build run: %w: expires_at must be in the future", ErrBuildRunDraftInvalid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("begin create build run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := scanBuildRun(tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_runs (
			workspace_id, build_run_id, mode, status, conversation_id,
			evaluation_team_id,
			brief_json, brief_hash, contract_json, contract_hash,
			asset_scope_json, baseline_snapshot_json,
			round_budget_json, total_budget_json,
			expires_at, publish_eligible, rollback_status, execution_strategy,
			confirmed_by, final_ref_json, created_at, updated_at, decided_at
		) VALUES ($1,$2,$3,'planning',$4,$5,$6,$7,$8,$9,$10,NULL,$11,$12,$13,false,'none',$14,NULL,NULL,$15,$15,NULL)
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, params.Brief.Mode,
		nullableString(params.ConversationID),
		nullableString(params.EvaluationTeamID),
		briefJSON, briefHash, contractJSON, contractHash,
		scopeJSON, roundBudgetJSON, totalBudgetJSON,
		params.ExpiresAt, executionStrategy, now))
	if err != nil {
		if params.EvaluationTeamID != "" && isActiveEvaluationUniqueViolation(err) {
			return TeamBuildRun{}, fmt.Errorf("create build run: %w", ErrActiveTeamEvaluation)
		}
		return TeamBuildRun{}, fmt.Errorf("create build run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,1,NULL,'planning','build run created',$3,$4)
	`, workspaceID, buildRunID, params.CreatedBy, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("create build run transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, fmt.Errorf("commit create build run: %w", err)
	}
	return run, nil
}

func isActiveEvaluationUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "weave_team_build_runs_active_evaluation_unique_idx"
}

func initialExecutionStrategy(mode, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == ExecutionStrategyTemplateInstantiate {
		if mode != ModeCreate {
			return "", errors.New("template_instantiate execution strategy requires create mode")
		}
		return requested, nil
	}
	if requested == ExecutionStrategyCompilerV1 {
		return requested, nil
	}
	if requested != "" {
		return "", fmt.Errorf("unsupported explicit execution strategy %q", requested)
	}
	if mode == ModeCreate {
		return ExecutionStrategyCompilerV1, nil
	}
	return ExecutionStrategyLegacy, nil
}

func expandCreateAssetScope(brief BuildBrief) BuildBrief {
	if brief.Mode != ModeCreate || strings.TrimSpace(brief.AllowedAssets.NamePrefix) == "" {
		return brief
	}
	scope := cloneAssetScope(brief.AllowedAssets)
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "agent")
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "team")
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "workflow")
	brief.AllowedAssets = scope
	return brief
}

func (s *Store) expandOptimizeAssetScope(
	ctx context.Context,
	workspaceID string,
	brief BuildBrief,
) (BuildBrief, error) {
	if brief.Mode != ModeOptimize || strings.TrimSpace(brief.TeamID) == "" {
		return brief, nil
	}
	orgStore, agents, workflows := s.baselineSources()
	if orgStore == nil || agents == nil || workflows == nil {
		return brief, nil
	}
	team, err := orgStore.GetTeam(ctx, workspaceID, brief.TeamID)
	if err != nil {
		if errors.Is(err, org.ErrTeamNotFound) {
			return brief, nil
		}
		return BuildBrief{}, fmt.Errorf("expand optimize asset scope: read team: %w", err)
	}
	scope := cloneAssetScope(brief.AllowedAssets)
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "team")
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "agent")
	scope.AllowedKinds = appendAllowedKind(scope.AllowedKinds, "workflow")
	scope.Refs = appendAssetRef(scope.Refs, AssetRef{Kind: "team", ID: team.ID})
	if strings.TrimSpace(team.LeadAvatarID) != "" {
		scope.Refs = appendAssetRef(scope.Refs, AssetRef{Kind: "agent", ID: team.LeadAvatarID})
	}
	workers, err := registry.NewTeamWorkerRepository(s.pool).ListByTeam(ctx, workspaceID, brief.TeamID)
	if err != nil {
		return BuildBrief{}, fmt.Errorf("expand optimize asset scope: read roster: %w", err)
	}
	for _, worker := range workers {
		if strings.TrimSpace(worker.WorkerAgentID) == "" {
			continue
		}
		scope.Refs = appendAssetRef(scope.Refs, AssetRef{Kind: "agent", ID: worker.WorkerAgentID})
	}
	teamWorkflows, err := workflows.ListByTeam(ctx, workspaceID, brief.TeamID)
	if err != nil {
		return BuildBrief{}, fmt.Errorf("expand optimize asset scope: read workflows: %w", err)
	}
	canonicalRefs := scope.Refs[:0]
	for _, ref := range scope.Refs {
		if strings.TrimSpace(ref.Kind) != "workflow" {
			canonicalRefs = append(canonicalRefs, ref)
		}
	}
	scope.Refs = canonicalRefs
	hasPublishedWorkflow := false
	for _, workflow := range teamWorkflows {
		if workflow.PublishedVersion != nil {
			scope.Refs = appendAssetRef(scope.Refs, AssetRef{Kind: "workflow", ID: workflow.ID})
			hasPublishedWorkflow = true
		}
	}
	// Compiler baselines contain only published workflows. Draft-only workflow
	// identities therefore do not provide an update target: remove any client
	// draft refs above and authorize the same deterministic first-published
	// workflow identity the compiler will emit.
	if !hasPublishedWorkflow {
		scope.Refs = appendAssetRef(scope.Refs, AssetRef{
			Kind: "workflow",
			ID:   FirstOptimizeWorkflowID(brief.TeamID),
		})
	}
	brief.AllowedAssets = scope
	return brief, nil
}

func appendAllowedKind(kinds []string, kind string) []string {
	for _, existing := range kinds {
		if existing == kind {
			return kinds
		}
	}
	return append(kinds, kind)
}

func appendAssetRef(refs []AssetRef, ref AssetRef) []AssetRef {
	for _, existing := range refs {
		if existing.Kind == ref.Kind && existing.ID == ref.ID && existing.Name == ref.Name {
			return refs
		}
		if existing.Kind == ref.Kind && existing.ID != "" && existing.ID == ref.ID {
			return refs
		}
	}
	return append(refs, ref)
}

// UpdateDraftContracts replaces the brief and contract drafts while the run
// is still in planning. The derived scope and budget columns follow the
// brief, and every write recomputes the frozen hashes.
func (s *Store) UpdateDraftContracts(
	ctx context.Context,
	workspaceID, buildRunID string,
	brief BuildBrief,
	contract EvaluationContract,
) (TeamBuildRun, error) {
	existing, err := s.GetBuildRun(ctx, workspaceID, buildRunID)
	if errors.Is(err, ErrBuildRunNotFound) {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", err)
	}
	if existing.Status != StatusPlanning || existing.Mode != brief.Mode {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", ErrBuildRunNotPlanning)
	}
	brief, err = s.expandOptimizeAssetScope(ctx, workspaceID, brief)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", err)
	}
	briefHash, err := brief.Hash()
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w: %w", ErrBuildRunDraftInvalid, err)
	}
	normalizedContract, err := normalizeEvaluationContract(
		brief,
		contract,
		!contractHasCompleteFloor(existing.Contract),
	)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w: %w", ErrBuildRunDraftInvalid, err)
	}
	contractHash, err := normalizedContract.Hash()
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w: %w", ErrBuildRunDraftInvalid, err)
	}
	briefJSON, err := json.Marshal(brief)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: encode brief: %w", err)
	}
	contractJSON, err := json.Marshal(normalizedContract)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: encode contract: %w", err)
	}
	scopeJSON, err := json.Marshal(brief.AllowedAssets)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: encode asset scope: %w", err)
	}
	roundBudgetJSON, err := json.Marshal(brief.RoundBudget)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: encode round budget: %w", err)
	}
	totalBudgetJSON, err := json.Marshal(brief.TotalBudget)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: encode total budget: %w", err)
	}

	run, err := scanBuildRun(s.pool.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET brief_json=$4, brief_hash=$5, contract_json=$6, contract_hash=$7,
			asset_scope_json=$8, round_budget_json=$9, total_budget_json=$10,
			updated_at=$11
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='planning'
		  AND mode=$3
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, brief.Mode,
		briefJSON, briefHash, contractJSON, contractHash,
		scopeJSON, roundBudgetJSON, totalBudgetJSON,
		s.clock.Now()))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.GetBuildRun(ctx, workspaceID, buildRunID); getErr != nil {
			return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", ErrBuildRunNotFound)
		}
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", ErrBuildRunNotPlanning)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("update build run drafts: %w", err)
	}
	return run, nil
}

func contractHasCompleteFloor(contract EvaluationContract) bool {
	byID := make(map[string]HardGate, len(contract.HardGates))
	for _, gate := range contract.HardGates {
		byID[gate.ID] = gate
	}
	for _, floor := range DefaultFloorHardGates() {
		gate, ok := byID[floor.ID]
		if !ok || !containsAllStrings(gate.Requirements, floor.Requirements) {
			return false
		}
	}
	return true
}

func cloneBlueprintRevisionToken(token *BlueprintRevisionToken) *BlueprintRevisionToken {
	if token == nil {
		return nil
	}
	cloned := *token
	return &cloned
}

// ClaimRollback atomically claims the one-shot rollback of one build run.
// The claim is the concurrency gate: the UPDATE transitions
// rollback_status 'none' → 'rollback_failed' inside the claim transaction and
// only one caller can win that transition. Preconditions (optimize mode,
// terminal status, frozen baseline present with a recomputable hash and the
// bound workspace) are validated in the same transaction; a validation
// failure rolls the claim back so the run stays at rollback_status 'none'.
// The run's updated_at is deliberately left untouched: terminal runs are
// constrained to updated_at <= decided_at, and a rollback does not change the
// run's decision time.
func (s *Store) ClaimRollback(
	ctx context.Context,
	workspaceID, buildRunID, operatorID string,
) (TeamBuildRun, error) {
	if workspaceID == "" || buildRunID == "" {
		return TeamBuildRun{}, errors.New("claim build run rollback: workspace_id and build_run_id are required")
	}
	if strings.TrimSpace(operatorID) == "" {
		return TeamBuildRun{}, errors.New("claim build run rollback: operator is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("begin claim build run rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET rollback_status='rollback_failed'
		WHERE workspace_id=$1 AND build_run_id=$2 AND rollback_status='none'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID))
	if errors.Is(err, pgx.ErrNoRows) {
		// The atomic claim lost (or the run does not exist). Re-read the row
		// under the same lock to report the exact precondition violation.
		var mode, status, rollbackStatus string
		readErr := tx.QueryRow(ctx, `
			SELECT mode, status, rollback_status
			FROM weave_team_build_runs
			WHERE workspace_id=$1 AND build_run_id=$2
			FOR UPDATE
		`, workspaceID, buildRunID).Scan(&mode, &status, &rollbackStatus)
		if errors.Is(readErr, pgx.ErrNoRows) {
			return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunNotFound)
		}
		if readErr != nil {
			return TeamBuildRun{}, fmt.Errorf("claim build run rollback: reread run: %w", readErr)
		}
		if rollbackStatus != RollbackNone {
			return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunRollbackClaimed)
		}
		if mode != ModeOptimize {
			return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunNotOptimize)
		}
		if !terminalStatuses[status] {
			return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunNotTerminal)
		}
		return TeamBuildRun{}, fmt.Errorf("claim build run rollback: unexpected claim failure")
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", err)
	}

	if run.Mode != ModeOptimize {
		return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunNotOptimize)
	}
	if !terminalStatuses[run.Status] {
		return TeamBuildRun{}, fmt.Errorf("claim build run rollback: %w", ErrBuildRunNotTerminal)
	}
	if run.Baseline == nil {
		return TeamBuildRun{}, fmt.Errorf(
			"claim build run rollback: optimize run has no baseline: %w",
			ErrBaselineSnapshotInvalid,
		)
	}
	if run.Baseline.WorkspaceID != workspaceID {
		return TeamBuildRun{}, fmt.Errorf(
			"claim build run rollback: baseline workspace %q does not match run workspace %q: %w",
			run.Baseline.WorkspaceID,
			workspaceID,
			ErrBaselineSnapshotInvalid,
		)
	}
	if err := validateBaselineSnapshot(*run.Baseline); err != nil {
		return TeamBuildRun{}, fmt.Errorf(
			"claim build run rollback: %w: %v",
			ErrBaselineSnapshotInvalid,
			err,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, fmt.Errorf("commit claim build run rollback: %w", err)
	}
	return run, nil
}

// CompleteRollback finalizes a claimed rollback as rolled_back. It is the
// last step of the restore sequence and only succeeds for a run whose claim
// is still in flight (rollback_status='rollback_failed'). updated_at is left
// untouched so the terminal-run decided_at ordering constraint holds.
func (s *Store) CompleteRollback(
	ctx context.Context,
	workspaceID, buildRunID string,
) (TeamBuildRun, error) {
	if workspaceID == "" || buildRunID == "" {
		return TeamBuildRun{}, errors.New("complete build run rollback: workspace_id and build_run_id are required")
	}
	run, err := scanBuildRun(s.pool.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET rollback_status='rolled_back'
		WHERE workspace_id=$1 AND build_run_id=$2 AND rollback_status='rollback_failed'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.GetBuildRun(ctx, workspaceID, buildRunID); getErr != nil {
			return TeamBuildRun{}, fmt.Errorf("complete build run rollback: %w", ErrBuildRunNotFound)
		}
		return TeamBuildRun{}, fmt.Errorf(
			"complete build run rollback: %w",
			ErrBuildRunRollbackClaimed,
		)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("complete build run rollback: %w", err)
	}
	return run, nil
}

// BeginTx opens a transaction for multi-row round persistence (evaluation
// report + round ledger row). The caller owns the transaction lifecycle and
// must commit (or roll back) before discarding it.
func (s *Store) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return s.pool.Begin(ctx)
}

// GetBuildRun returns one control record from one workspace.
func (s *Store) GetBuildRun(ctx context.Context, workspaceID, buildRunID string) (TeamBuildRun, error) {
	run, err := scanBuildRun(s.pool.QueryRow(ctx, `
		SELECT `+buildRunColumns+`
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("get build run: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("get build run: %w", err)
	}
	return run, nil
}

// ListBuildRuns returns one page of list summaries plus the total number of
// rows matching the same workspace/filter. The query pulls latest round/report
// and budget totals with lateral aggregates, so list rendering does not issue
// a query per run.
func (s *Store) ListBuildRuns(
	ctx context.Context,
	workspaceID string,
	filter BuildRunFilter,
) ([]TeamBuildRunSummary, int, error) {
	if workspaceID == "" {
		return nil, 0, errors.New("list build runs: workspace_id is required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if filter.Offset < 0 {
		return nil, 0, errors.New("list build runs: offset must be non-negative")
	}
	where, args, err := buildRunListWhere(workspaceID, filter)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_team_build_runs b
		`+where,
		args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("list build runs: count: %w", err)
	}
	args = append(args, limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT
			b.build_run_id,
			b.mode,
			b.status,
			COALESCE(b.conversation_id, ''),
			COALESCE(b.brief_json->>'team_id', ''),
			COALESCE(b.baseline_snapshot_json->'team'->>'name', ''),
			COALESCE(b.brief_json->>'new_team_name', ''),
			COALESCE(latest.round_no, 0),
			COALESCE((b.contract_json->>'max_iterations')::int, 0),
			COALESCE(latest.conclusion, ''),
			COALESCE(latest.failure_category, ''),
			COALESCE(usage.input_tokens, 0)::bigint,
			COALESCE(usage.output_tokens, 0)::bigint,
			COALESCE(usage.cost_usd, 0),
			COALESCE(usage.tool_calls, 0)::bigint,
			b.publish_eligible,
			b.rollback_status,
			b.final_ref_json,
			COALESCE(b.confirmed_by, ''),
			b.expires_at,
			b.updated_at
		FROM weave_team_build_runs b
		LEFT JOIN LATERAL (
			SELECT r.round_no, r.conclusion, rr.report_json->>'failure_category' AS failure_category
			FROM weave_team_build_run_rounds r
			LEFT JOIN weave_team_build_run_reports rr
			  ON rr.workspace_id = r.workspace_id
			 AND rr.build_run_id = r.build_run_id
			 AND rr.round_no = r.round_no
			WHERE r.workspace_id = b.workspace_id
			  AND r.build_run_id = b.build_run_id
			ORDER BY r.round_no DESC
			LIMIT 1
		) latest ON TRUE
		LEFT JOIN LATERAL (
			SELECT SUM(input_tokens) AS input_tokens,
				SUM(output_tokens) AS output_tokens,
				SUM(cost_usd) AS cost_usd,
				SUM(tool_calls) AS tool_calls
			FROM weave_team_build_run_budget_ledger l
			WHERE l.workspace_id = b.workspace_id
			  AND l.build_run_id = b.build_run_id
		) usage ON TRUE
		`+where+`
		ORDER BY b.updated_at DESC, b.build_run_id DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)),
		args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list build runs: %w", err)
	}
	defer rows.Close()
	summaries := make([]TeamBuildRunSummary, 0)
	for rows.Next() {
		var summary TeamBuildRunSummary
		var finalRefJSON []byte
		if err := rows.Scan(
			&summary.BuildRunID,
			&summary.Mode,
			&summary.Status,
			&summary.ConversationID,
			&summary.TargetTeamID,
			&summary.TargetTeamName,
			&summary.NewTeamName,
			&summary.CurrentRound,
			&summary.MaxRounds,
			&summary.LatestConclusion,
			&summary.LatestFailureCategory,
			&summary.BudgetUsage.InputTokens,
			&summary.BudgetUsage.OutputTokens,
			&summary.BudgetUsage.CostUSD,
			&summary.BudgetUsage.ToolCalls,
			&summary.PublishEligible,
			&summary.RollbackStatus,
			&finalRefJSON,
			&summary.ConfirmedBy,
			&summary.ExpiresAt,
			&summary.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("list build runs: scan: %w", err)
		}
		if len(finalRefJSON) > 0 {
			var finalRef FinalRef
			if err := json.Unmarshal(finalRefJSON, &finalRef); err != nil {
				return nil, 0, fmt.Errorf("list build runs: decode final_ref: %w", err)
			}
			summary.FinalRef = &finalRef
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list build runs: %w", err)
	}
	return summaries, total, nil
}

func buildRunListWhere(
	workspaceID string,
	filter BuildRunFilter,
) (string, []any, error) {
	clauses := []string{"b.workspace_id=$1"}
	args := []any{workspaceID}
	if filter.Status != "" {
		if !validStatuses[filter.Status] {
			return "", nil, errors.New("list build runs: invalid status")
		}
		args = append(args, filter.Status)
		clauses = append(clauses, "b.status=$"+strconv.Itoa(len(args)))
	}
	if filter.Mode != "" {
		switch filter.Mode {
		case ModeCreate, ModeOptimize:
		default:
			return "", nil, errors.New("list build runs: invalid mode")
		}
		args = append(args, filter.Mode)
		clauses = append(clauses, "b.mode=$"+strconv.Itoa(len(args)))
	}
	return "WHERE " + strings.Join(clauses, " AND "), args, nil
}

// GetActiveBuildRunByConversation returns the active build run bound to one
// conversation, or ErrBuildRunNotFound when no active run exists. Keep this
// explicit status set aligned with the database's partial unique index.
func (s *Store) GetActiveBuildRunByConversation(
	ctx context.Context,
	workspaceID, conversationID string,
) (TeamBuildRun, error) {
	if workspaceID == "" || conversationID == "" {
		return TeamBuildRun{}, fmt.Errorf("get active build run by conversation: %w", ErrBuildRunNotFound)
	}
	run, err := scanBuildRun(s.pool.QueryRow(ctx, `
		SELECT `+buildRunColumns+`
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND conversation_id=$2
		  AND status IN ('planning', 'authorized', 'round_running', 'publishing')
		ORDER BY created_at DESC, build_run_id DESC
		LIMIT 1
	`, workspaceID, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("get active build run by conversation: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("get active build run by conversation: %w", err)
	}
	return run, nil
}

// GetBuildRunSummary returns the newest conversation-bound run, including a
// terminal run. Conversation progress must retain passed, blocked, and
// cancelled outcomes instead of falling back to stale transient UI state.
func (s *Store) GetBuildRunSummary(
	ctx context.Context,
	workspaceID, conversationID string,
) (string, string, error) {
	if s == nil || workspaceID == "" || conversationID == "" {
		return "", "", fmt.Errorf("get build run summary: %w", ErrBuildRunNotFound)
	}
	var buildRunID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT build_run_id, status
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND conversation_id=$2
		ORDER BY created_at DESC, build_run_id DESC
		LIMIT 1
	`, workspaceID, conversationID).Scan(&buildRunID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("get build run summary: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return "", "", fmt.Errorf("get build run summary: %w", err)
	}
	return buildRunID, status, nil
}

// GetActiveBuildRunSummary returns only the build_run_id and status for a
// conversation-bound active run. It satisfies
// conversation.ActiveBuildRunReader and is designed for batch attachment in
// the conversation list path where full TeamBuildRun fields are unnecessary.
func (s *Store) GetActiveBuildRunSummary(
	ctx context.Context,
	workspaceID, conversationID string,
) (string, string, error) {
	if s == nil || workspaceID == "" || conversationID == "" {
		return "", "", fmt.Errorf("get active build run summary: %w", ErrBuildRunNotFound)
	}
	var buildRunID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT build_run_id, status
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND conversation_id=$2
		  AND status IN ('planning', 'authorized', 'round_running', 'publishing')
		ORDER BY created_at DESC, build_run_id DESC
		LIMIT 1
	`, workspaceID, conversationID).Scan(&buildRunID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("get active build run summary: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return "", "", fmt.Errorf("get active build run summary: %w", err)
	}
	return buildRunID, status, nil
}

// GetLatestBuildRunByConversation returns the newest build run for one
// conversation regardless of status. Conversation tool wiring uses it to
// distinguish a brand-new discovery conversation (no run yet) from a
// completed build conversation, whose terminal run must keep all teamforge
// tools closed.
func (s *Store) GetLatestBuildRunByConversation(
	ctx context.Context,
	workspaceID, conversationID string,
) (TeamBuildRun, error) {
	if workspaceID == "" || conversationID == "" {
		return TeamBuildRun{}, fmt.Errorf("get latest build run by conversation: %w", ErrBuildRunNotFound)
	}
	run, err := scanBuildRun(s.pool.QueryRow(ctx, `
		SELECT `+buildRunColumns+`
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND conversation_id=$2
		ORDER BY created_at DESC, build_run_id DESC
		LIMIT 1
	`, workspaceID, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("get latest build run by conversation: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("get latest build run by conversation: %w", err)
	}
	return run, nil
}

// ResolveAssetScopeRef maps a to-be-written asset reference onto the frozen
// scope entry that authorizes it. Create runs (name_prefix) and same-ID
// optimize assets pass through unchanged. The only rewrite is the optimize
// new-workflow slot: the run's brief pins it as FirstOptimizeWorkflowID
// (teamID+"-workflow") while the build produces the same asset under the
// team's persisted UUID, so a <teamUUID>-workflow write resolves to that slot
// when the frozen target team owns no baseline workflow.
func ResolveAssetScopeRef(run TeamBuildRun, assetRef AssetRef) AssetRef {
	if run.Mode != ModeOptimize || assetRef.Kind != "workflow" || assetRef.ID == "" {
		return assetRef
	}
	if run.AssetScope.Contains(assetRef) {
		return assetRef
	}
	slot := FirstOptimizeWorkflowID(run.Brief.TeamID)
	if assetRef.ID == slot || !run.AssetScope.Contains(AssetRef{Kind: "workflow", ID: slot}) {
		return assetRef
	}
	if run.Baseline == nil || strings.TrimSpace(run.Baseline.Team.TeamID) == "" ||
		len(run.Baseline.Workflows) != 0 {
		return assetRef
	}
	// The immutable baseline binds the placeholder to exactly one persisted
	// team identity. A different team's UUID-shaped workflow stays out of scope.
	producedID := FirstOptimizeWorkflowID(run.Baseline.Team.TeamID)
	if assetRef.ID != producedID {
		return assetRef
	}
	return AssetRef{Kind: "workflow", ID: slot}
}

type rowScanner interface {
	Scan(...any) error
}

func scanBuildRun(row rowScanner) (TeamBuildRun, error) {
	var run TeamBuildRun
	var briefRaw, contractRaw, scopeRaw []byte
	var baselineRaw, finalRefRaw, roundBudgetRaw, totalBudgetRaw []byte
	var confirmedBy *string
	var authorizationAuthority, authorizedBlueprintHash, authorizedChangeSetHash *string
	var authorizationDecisionSubject, authorizationDecisionReason *string
	var authorizedRevisionNo *int
	var conversationID, evaluationTeamID *string
	var decidedAt *time.Time
	if err := row.Scan(
		&run.WorkspaceID, &run.BuildRunID, &run.Mode, &run.ExecutionStrategy, &run.Status, &conversationID,
		&evaluationTeamID,
		&briefRaw, &run.BriefHash, &contractRaw, &run.ContractHash,
		&scopeRaw, &baselineRaw,
		&roundBudgetRaw, &totalBudgetRaw,
		&run.ExpiresAt, &run.PublishEligible, &run.RollbackStatus,
		&confirmedBy, &authorizationAuthority, &authorizedRevisionNo,
		&authorizedBlueprintHash, &authorizedChangeSetHash,
		&authorizationDecisionSubject, &authorizationDecisionReason,
		&finalRefRaw, &run.CreatedAt, &run.UpdatedAt, &decidedAt,
	); err != nil {
		return TeamBuildRun{}, err
	}
	if err := json.Unmarshal(briefRaw, &run.Brief); err != nil {
		return TeamBuildRun{}, fmt.Errorf("decode build run brief: %w", err)
	}
	if err := json.Unmarshal(contractRaw, &run.Contract); err != nil {
		return TeamBuildRun{}, fmt.Errorf("decode build run contract: %w", err)
	}
	if err := json.Unmarshal(scopeRaw, &run.AssetScope); err != nil {
		return TeamBuildRun{}, fmt.Errorf("decode build run asset scope: %w", err)
	}
	if len(baselineRaw) > 0 {
		baseline := &BaselineSnapshot{}
		if err := json.Unmarshal(baselineRaw, baseline); err != nil {
			return TeamBuildRun{}, fmt.Errorf("decode build run baseline snapshot: %w", err)
		}
		run.Baseline = baseline
	}
	if len(finalRefRaw) > 0 {
		finalRef := &FinalRef{}
		if err := json.Unmarshal(finalRefRaw, finalRef); err != nil {
			return TeamBuildRun{}, fmt.Errorf("decode build run final ref: %w", err)
		}
		run.FinalRef = finalRef
	}
	if err := json.Unmarshal(roundBudgetRaw, &run.RoundBudget); err != nil {
		return TeamBuildRun{}, fmt.Errorf("decode build run round budget: %w", err)
	}
	if err := json.Unmarshal(totalBudgetRaw, &run.TotalBudget); err != nil {
		return TeamBuildRun{}, fmt.Errorf("decode build run total budget: %w", err)
	}
	if confirmedBy != nil {
		run.ConfirmedBy = *confirmedBy
	}
	if authorizationAuthority != nil {
		run.Authorization.Authority = *authorizationAuthority
	}
	if authorizationDecisionSubject != nil {
		run.Authorization.DecisionSubject = *authorizationDecisionSubject
	}
	if authorizationDecisionReason != nil {
		run.Authorization.DecisionReason = *authorizationDecisionReason
	}
	if authorizedRevisionNo != nil && authorizedBlueprintHash != nil && authorizedChangeSetHash != nil {
		run.Authorization.RevisionToken = &BlueprintRevisionToken{
			RevisionNo:    *authorizedRevisionNo,
			BlueprintHash: *authorizedBlueprintHash,
			ChangeSetHash: *authorizedChangeSetHash,
		}
	}
	if conversationID != nil {
		run.ConversationID = *conversationID
	}
	if evaluationTeamID != nil {
		run.EvaluationTeamID = *evaluationTeamID
	}
	run.DecidedAt = decidedAt
	return run, nil
}

// nullableString converts a possibly-empty string into a pgx-compatible
// nullable value.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
