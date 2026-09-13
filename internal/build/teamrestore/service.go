package teamrestore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// TxBeginner supplies the caller-owned transaction used by every restore step.
type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// BuildRollbackStore is the team build control-record surface used by the
// rollback service.
type BuildRollbackStore interface {
	ClaimRollback(context.Context, string, string, string) (teambuild.TeamBuildRun, error)
	CompleteRollback(context.Context, string, string) (teambuild.TeamBuildRun, error)
}

// OrgRestoreStore reads the current team CAS token and restores the
// baseline-only team fields inside a caller-owned transaction.
type OrgRestoreStore interface {
	GetTeam(context.Context, string, string) (org.Team, error)
	RestoreTeamTx(context.Context, pgx.Tx, string, string, org.RestoreTeamState) (org.Team, error)
}

// RosterRestoreStore applies the baseline-only complete roster command inside
// a caller-owned transaction.
type RosterRestoreStore interface {
	ApplyTeamBuildRollbackRosterTx(
		context.Context,
		pgx.Tx,
		registry.TeamRosterCommand,
	) (*registry.TeamRosterResult, error)
}

// AgentRestoreStore restores one agent head from its baseline pin inside a
// caller-owned transaction.
type AgentRestoreStore interface {
	RestoreVersionTx(
		context.Context,
		pgx.Tx,
		string, // workspaceID
		string, // agentID
		string, // operatorID
		int, // pinnedVersion
		string, // expectedContentHash
	) (*registry.AgentVersionRestoreResult, error)
}

// WorkflowRestorer materializes and publishes one baseline workflow through
// the product publication boundary. Builder owns only the rollback sequence;
// product draft transactions and kernel receipts stay in the app adapter.
type WorkflowRestorer interface {
	RestoreWorkflow(context.Context, string, string, string, string, teambuild.BaselineWorkflowRef) (WorkflowRestoreResult, error)
}

// AuditRecorder persists one best-effort audit entry.
type AuditRecorder interface {
	Record(context.Context, string, string, string, string, string) error
}

// Service executes the fixed stepwise baseline rollback. It accepts only
// (workspaceID, buildRunID, operatorID); every restore target comes from the
// frozen baseline snapshot claimed for the run.
type Service struct {
	pool      TxBeginner
	builds    BuildRollbackStore
	orgStore  OrgRestoreStore
	roster    RosterRestoreStore
	agents    AgentRestoreStore
	workflows WorkflowRestorer
	audit     AuditRecorder
}

// New creates the rollback service. All dependencies are required; nil values
// fail fast at construction so wiring mistakes surface before any claim.
func New(
	pool TxBeginner,
	builds BuildRollbackStore,
	orgStore OrgRestoreStore,
	roster RosterRestoreStore,
	agents AgentRestoreStore,
	workflows WorkflowRestorer,
	auditRecorder AuditRecorder,
) *Service {
	if pool == nil || builds == nil || orgStore == nil || roster == nil ||
		agents == nil || workflows == nil {
		panic("teamrestore: all service dependencies are required")
	}
	return &Service{
		pool: pool, builds: builds, orgStore: orgStore, roster: roster,
		agents: agents, workflows: workflows, audit: auditRecorder,
	}
}

// Rollback performs the admin-confirmed rollback of one optimize build run:
//
//  1. Agents: each baseline agent pin is restored to a new head version.
//  2. Team + Roster: one baseline-only complete command restores lead, status,
//     complete workers (with removal allowed) and the team fields, with the
//     CAS token read at restore start and the fixed idempotency key
//     team-build-rollback:<build_run_id>:roster.
//  3. Workflows: each baseline workflow gets a MAX(version)+1 draft from the
//     frozen trigger/graph, is validated by the candidate builder, and is
//     published with the updated_at CAS.
//  4. Complete: rollback_status → rolled_back with the result in the audit
//     detail.
//
// Every step owns its transaction; a failed step rolls back only itself and
// stops the sequence. The run's rollback_status is 'rollback_failed' from the
// moment the claim commits until the final step flips it to 'rolled_back'.
func (s *Service) Rollback(
	ctx context.Context,
	workspaceID, buildRunID, operatorID string,
) (RollbackResult, error) {
	run, err := s.builds.ClaimRollback(ctx, workspaceID, buildRunID, operatorID)
	if err != nil {
		return RollbackResult{}, err
	}
	if run.Baseline == nil {
		return RollbackResult{}, fmt.Errorf(
			"rollback %s: %w",
			buildRunID,
			ErrRollbackBaselineMissing,
		)
	}
	baseline := run.Baseline

	result := RollbackResult{
		BuildRunID:     buildRunID,
		RollbackStatus: teambuild.RollbackFailed,
		Steps:          initialSteps(),
	}

	// Step 1: Agents — one transaction per pinned agent.
	agentResults, stepErr := s.restoreAgents(ctx, workspaceID, baseline, operatorID)
	result.AgentVersions = agentResults
	if stepErr != nil {
		result = setStep(result, StepAgents, StepFailed, stepErr.Error())
		return s.failRollback(ctx, workspaceID, buildRunID, result, stepErr)
	}
	result = setStep(result, StepAgents, StepDone, fmt.Sprintf(
		"restored %d agent(s)", len(agentResults),
	))
	result = s.recordStepAudit(ctx, workspaceID, buildRunID, "rollback.step.agents", "ok", result)

	// Step 2: Team + Roster — one baseline-only command plus team fields in
	// one transaction.
	rosterResult, stepErr := s.restoreTeamRoster(ctx, workspaceID, buildRunID, baseline, operatorID)
	result.Roster = rosterResult
	if stepErr != nil {
		result = setStep(result, StepTeamRoster, StepFailed, stepErr.Error())
		return s.failRollback(ctx, workspaceID, buildRunID, result, stepErr)
	}
	result = setStep(result, StepTeamRoster, StepDone, fmt.Sprintf(
		"restored team %s and full roster (lead %s, %d worker(s))",
		baseline.Team.TeamID,
		rosterResult.LeadAgentID,
		len(rosterResult.Workers),
	))
	result = s.recordStepAudit(ctx, workspaceID, buildRunID, "rollback.step.roster", "ok", result)

	// Step 3: Workflows — one transaction per baseline workflow.
	workflowResults, stepErr := s.restoreWorkflows(ctx, workspaceID, buildRunID, baseline, operatorID)
	result.WorkflowResults = workflowResults
	if stepErr != nil {
		result = setStep(result, StepWorkflows, StepFailed, stepErr.Error())
		return s.failRollback(ctx, workspaceID, buildRunID, result, stepErr)
	}
	result = setStep(result, StepWorkflows, StepDone, fmt.Sprintf(
		"restored %d workflow(s)", len(workflowResults),
	))
	result = s.recordStepAudit(ctx, workspaceID, buildRunID, "rollback.step.workflows", "ok", result)

	// Step 4: Complete — flip the claim to rolled_back.
	if _, err := s.builds.CompleteRollback(ctx, workspaceID, buildRunID); err != nil {
		result = setStep(result, StepComplete, StepFailed, err.Error())
		return s.failRollback(ctx, workspaceID, buildRunID, result, err)
	}
	result.RollbackStatus = teambuild.RollbackRolledBack
	result = setStep(result, StepComplete, StepDone, "rollback completed")
	result = s.recordStepAudit(ctx, workspaceID, buildRunID, "rollback.complete", "ok", result)
	return result, nil
}

// restoreAgents replays each baseline agent pin in one transaction per agent.
// An earlier agent's restore stays committed when a later agent fails.
func (s *Service) restoreAgents(
	ctx context.Context,
	workspaceID string,
	baseline *teambuild.BaselineSnapshot,
	operatorID string,
) ([]AgentRestoreResult, error) {
	results := make([]AgentRestoreResult, 0, len(baseline.AgentPins))
	pins := append([]teambuild.BaselineAgentPin(nil), baseline.AgentPins...)
	sort.Slice(pins, func(i, j int) bool { return pins[i].AgentID < pins[j].AgentID })
	for _, pin := range pins {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return results, fmt.Errorf("begin agent restore: %w", err)
		}
		restored, err := s.agents.RestoreVersionTx(
			ctx, tx, workspaceID, pin.AgentID, operatorID, pin.Version, pin.ContentHash,
		)
		if err != nil {
			_ = tx.Rollback(ctx)
			return results, fmt.Errorf("restore agent %q: %w", pin.AgentID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return results, fmt.Errorf("commit agent restore %q: %w", pin.AgentID, err)
		}
		results = append(results, AgentRestoreResult{
			AgentID: restored.AgentID, Name: restored.Name,
			Version: restored.Version, Restored: restored.Restored,
		})
	}
	return results, nil
}

// restoreTeamRoster restores the complete baseline roster and team fields in
// one transaction: the baseline-only roster command runs first (its CAS token
// is the updated_at read at restore start), then the team field restore in
// the same lock order.
func (s *Service) restoreTeamRoster(
	ctx context.Context,
	workspaceID, buildRunID string,
	baseline *teambuild.BaselineSnapshot,
	operatorID string,
) (*registry.TeamRosterResult, error) {
	teamID := baseline.Team.TeamID
	team, err := s.orgStore.GetTeam(ctx, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("read team %q: %w", teamID, err)
	}
	workers := make([]registry.TeamRosterWorkerInput, 0, len(baseline.Roster))
	for _, entry := range baseline.Roster {
		if entry.Role != "worker" {
			continue
		}
		workers = append(workers, registry.TeamRosterWorkerInput{
			WorkerAgentID:      entry.AgentID,
			Duty:               entry.Duty,
			WhenToUse:          entry.WhenToUse,
			ContextInstruction: entry.ContextInstruction,
			AllowedKinds:       append([]string(nil), entry.AllowedKinds...),
			DefaultKind:        entry.DefaultKind,
			ResultRequirement:  entry.ResultRequirement,
			Enabled:            entry.Enabled,
		})
	}
	command := registry.TeamRosterCommand{
		WorkspaceID:       workspaceID,
		TeamID:            teamID,
		IdempotencyKey:    fmt.Sprintf("team-build-rollback:%s:roster", buildRunID),
		ExpectedUpdatedAt: team.UpdatedAt,
		DesiredTeamStatus: baseline.Team.Status,
		LeadAgentID:       baseline.Team.LeadAvatarID,
		Workers:           workers,
		OperatorID:        operatorID,
		Reason:            "team build rollback " + buildRunID,
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin team roster restore: %w", err)
	}
	rosterResult, err := s.roster.ApplyTeamBuildRollbackRosterTx(ctx, tx, command)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("apply baseline roster command: %w", err)
	}
	if _, err := s.orgStore.RestoreTeamTx(ctx, tx, workspaceID, teamID, org.RestoreTeamState{
		Name:            baseline.Team.Name,
		Objective:       baseline.Team.Objective,
		PrimaryScenario: baseline.Team.PrimaryScenario,
		SuccessCriteria: baseline.Team.SuccessCriteria,
		Status:          baseline.Team.Status,
		DispatchRules: org.TeamDispatchRules{
			TeamID:           teamID,
			LegTimeoutSec:    baseline.DispatchRules.LegTimeoutSec,
			GroupDeadlineSec: baseline.DispatchRules.GroupDeadlineSec,
			Quorum:           baseline.DispatchRules.Quorum,
		},
	}); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("restore team fields: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("commit team roster restore: %w", err)
	}
	return rosterResult, nil
}

// restoreWorkflows replays each baseline workflow in one transaction per
// workflow. A workflow without published baseline content is skipped; any
// other failure stops the step and leaves earlier workflows published.
func (s *Service) restoreWorkflows(
	ctx context.Context,
	workspaceID, buildRunID string,
	baseline *teambuild.BaselineSnapshot,
	operatorID string,
) ([]WorkflowRestoreResult, error) {
	results := make([]WorkflowRestoreResult, 0, len(baseline.Workflows))
	refs := append([]teambuild.BaselineWorkflowRef(nil), baseline.Workflows...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].WorkflowID < refs[j].WorkflowID })
	for _, ref := range refs {
		if ref.Published == nil {
			results = append(results, WorkflowRestoreResult{
				WorkflowID: ref.WorkflowID,
				Skipped:    true,
				Reason:     "no published baseline content",
			})
			continue
		}
		restored, err := s.workflows.RestoreWorkflow(ctx, workspaceID, buildRunID, baseline.Team.TeamID, operatorID, ref)
		if err != nil {
			return results, fmt.Errorf("restore workflow %q: %w", ref.WorkflowID, err)
		}
		results = append(results, restored)
	}
	return results, nil
}

// failRollback writes the failure audit detail (done/failed/pending with the
// reason) and returns the result together with the step error. The claim
// already left rollback_status at 'rollback_failed', so no status write is
// needed here.
func (s *Service) failRollback(
	ctx context.Context,
	workspaceID, buildRunID string,
	result RollbackResult,
	cause error,
) (RollbackResult, error) {
	result = s.recordStepAudit(ctx, workspaceID, buildRunID, "rollback.failed", "error", result)
	return result, cause
}

// recordStepAudit persists one best-effort audit entry carrying the step
// detail (or the full result for the final record). Audit failures never
// block the restore; they are surfaced as warnings on the result.
func (s *Service) recordStepAudit(
	ctx context.Context,
	workspaceID, buildRunID, tool, status string,
	result RollbackResult,
) RollbackResult {
	if s.audit == nil {
		return result
	}
	detail := result
	if status == "ok" {
		detail = RollbackResult{BuildRunID: result.BuildRunID, Steps: result.Steps}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return appendAuditWarning(result, fmt.Sprintf("audit %s encode failed: %v", tool, err))
	}
	if err := s.audit.Record(
		ctx,
		workspaceID,
		"team-build-rollback",
		tool,
		status,
		string(encoded),
	); err != nil {
		return appendAuditWarning(result, fmt.Sprintf("audit %s failed: %v", tool, err))
	}
	return result
}
