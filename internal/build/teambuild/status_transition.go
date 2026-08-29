package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var validStatuses = map[string]bool{
	StatusPlanning:     true,
	StatusAuthorized:   true,
	StatusRoundRunning: true,
	StatusPublishing:   true,
	StatusPassed:       true,
	StatusBlocked:      true,
	StatusCancelled:    true,
}

var allowedTransitions = map[[2]string]bool{
	{StatusPlanning, StatusAuthorized}:     true,
	{StatusPlanning, StatusBlocked}:        true,
	{StatusPlanning, StatusCancelled}:      true,
	{StatusAuthorized, StatusRoundRunning}: true,
	{StatusAuthorized, StatusBlocked}:      true,
	{StatusAuthorized, StatusCancelled}:    true,
	{StatusRoundRunning, StatusAuthorized}: true,
	{StatusRoundRunning, StatusPublishing}: true,
	{StatusRoundRunning, StatusBlocked}:    true,
	{StatusRoundRunning, StatusCancelled}:  true,
	{StatusPublishing, StatusPassed}:       true,
	{StatusPublishing, StatusBlocked}:      true,
	{StatusPublishing, StatusCancelled}:    true,
}

// TransitionStatus performs one optimistic status transition and appends the
// matching ledger row. Terminal transitions stamp decided_at; entering
// publishing marks the run publish-eligible.
func (s *Store) TransitionStatus(
	ctx context.Context,
	workspaceID, buildRunID, from, to, actor, reason string,
) (TeamBuildRun, error) {
	if err := validateStatusTransition(from, to); err != nil {
		return TeamBuildRun{}, fmt.Errorf("transition build run: %w", err)
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return TeamBuildRun{}, errors.New("transition build run: actor and reason are required")
	}

	now := s.clock.Now()
	terminal := terminalStatuses[to]
	var decidedAt any
	if terminal {
		decidedAt = now
	}
	publishEligible := to == StatusPublishing || to == StatusPassed

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("begin transition build run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status=$4, updated_at=$5, decided_at=$6, publish_eligible=$7
		WHERE workspace_id=$1 AND build_run_id=$2 AND status=$3
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, from, to, now, decidedAt, publishEligible))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("transition build run: build run %q is not %q or does not exist", buildRunID, from)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("transition build run: %w", err)
	}

	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("transition build run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, workspaceID, buildRunID, nextSeq, from, to, reason, actor, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("transition build run ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, fmt.Errorf("commit transition build run: %w", err)
	}
	return run, nil
}

// MarkPublished finalizes a publishing run as passed and records the final
// publication reference in the same transition.
func (s *Store) MarkPublished(
	ctx context.Context,
	workspaceID, buildRunID, actor string,
	finalRef FinalRef,
) (TeamBuildRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("begin mark build run published: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := s.MarkPublishedTx(ctx, tx, workspaceID, buildRunID, actor, finalRef, "")
	if err != nil {
		return TeamBuildRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, fmt.Errorf("commit mark build run published: %w", err)
	}
	return run, nil
}

// MarkPublishedTx finalizes publication inside the caller-owned transaction.
// Evaluation runs additionally flip the unevaluated team and provenance with
// a CAS bound to the baseline proof returned by VerifyEvaluationBaselineTx.
func (s *Store) MarkPublishedTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID, actor string,
	finalRef FinalRef,
	verifiedBaselineHash string,
) (TeamBuildRun, error) {
	if tx == nil {
		return TeamBuildRun{}, errors.New("mark build run published tx: transaction is required")
	}
	if strings.TrimSpace(actor) == "" {
		return TeamBuildRun{}, errors.New("mark build run published tx: actor is required")
	}
	if strings.TrimSpace(finalRef.Ref) == "" {
		return TeamBuildRun{}, errors.New("mark build run published tx: final ref is required")
	}
	locked, err := s.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w", err)
	}
	if locked.Status != StatusPublishing {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w: run is not publishing", ErrInvalidTransition)
	}
	now := s.clock.Now()
	if locked.EvaluationOnly {
		if locked.Baseline == nil || verifiedBaselineHash == "" ||
			verifiedBaselineHash != locked.Baseline.ContentHash ||
			finalRef.TeamID != locked.EvaluationTeamID {
			return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w: evaluation proof mismatch", ErrEvaluationPublishCAS)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE weave_teams
			SET evaluation='evaluated', evaluation_build_run_id=$3,
				evaluation_contract_hash=$4, evaluated_at=$5, updated_at=$5
			WHERE workspace_id=$1 AND id=$2 AND evaluation='unevaluated'
		`, workspaceID, locked.EvaluationTeamID, buildRunID, locked.ContractHash, now)
		if err != nil {
			return TeamBuildRun{}, fmt.Errorf("mark build run published tx: certify team: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w: team state changed", ErrEvaluationPublishCAS)
		}
	}
	finalRefJSON, err := json.Marshal(finalRef)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: encode final ref: %w", err)
	}
	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='passed', updated_at=$3, decided_at=$3,
			publish_eligible=true, final_ref_json=$4::jsonb
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='publishing'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, now, string(finalRefJSON)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w", ErrInvalidTransition)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w", err)
	}
	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'publishing','passed',
			'final publication completed',$4,$5)
	`, workspaceID, buildRunID, nextSeq, actor, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark build run published tx ledger: %w", err)
	}
	return run, nil
}

// BlockPublishingBudgetTx performs G5's terminal rejection in the same
// transaction that holds the BuildRun lock, settles usage, and reads budget.
func (s *Store) BlockPublishingBudgetTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID, actor string,
) (TeamBuildRun, error) {
	if tx == nil || strings.TrimSpace(actor) == "" {
		return TeamBuildRun{}, errors.New("block publishing budget tx: transaction and actor are required")
	}
	locked, err := s.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("block publishing budget tx: %w", err)
	}
	if locked.Status != StatusPublishing {
		return TeamBuildRun{}, fmt.Errorf("block publishing budget tx: %w: run is not publishing", ErrInvalidTransition)
	}
	now := s.clock.Now()
	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='blocked', updated_at=$3, decided_at=$3,
			publish_eligible=false, final_ref_json=NULL
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='publishing'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, now))
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("block publishing budget tx: %w", err)
	}
	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("block publishing budget tx: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'publishing','blocked','budget_exhausted',$4,$5)
	`, workspaceID, buildRunID, nextSeq, actor, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("block publishing budget tx ledger: %w", err)
	}
	return run, nil
}

// MarkTemplateInstantiated finalizes a materialization-only template build.
// Unlike publication, it deliberately leaves publish_eligible false while
// recording the ready unevaluated Team as the final result.
func (s *Store) MarkTemplateInstantiated(
	ctx context.Context,
	workspaceID, buildRunID, actor string,
	finalRef FinalRef,
) (TeamBuildRun, error) {
	if strings.TrimSpace(actor) == "" {
		return TeamBuildRun{}, errors.New("mark template instantiated: actor is required")
	}
	if strings.TrimSpace(finalRef.Ref) == "" || strings.TrimSpace(finalRef.TeamID) == "" {
		return TeamBuildRun{}, errors.New("mark template instantiated: team final ref is required")
	}
	finalRefJSON, err := json.Marshal(finalRef)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template instantiated: encode final ref: %w", err)
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("begin mark template instantiated: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='passed', updated_at=$3, decided_at=$3,
			publish_eligible=false, final_ref_json=$4::jsonb
		WHERE workspace_id=$1 AND build_run_id=$2
		  AND status='round_running' AND execution_strategy='template_instantiate'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, now, string(finalRefJSON)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("mark template instantiated: build run %q is not a running template build", buildRunID)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template instantiated: %w", err)
	}
	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template instantiated: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'round_running','passed',
			'template assets materialized; evaluation deferred',$4,$5)
	`, workspaceID, buildRunID, nextSeq, actor, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template instantiated ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, fmt.Errorf("commit mark template instantiated: %w", err)
	}
	return run, nil
}

// MarkTemplatePublishedTx finalizes a template fast-path build inside the
// caller-owned publication transaction. The workflow publication and Team
// activation must already be staged in the same transaction; this method
// records the exact immutable artifact as the BuildRun final reference.
func (s *Store) MarkTemplatePublishedTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID, actor string,
	finalRef FinalRef,
) (TeamBuildRun, error) {
	if tx == nil {
		return TeamBuildRun{}, errors.New("mark template published: transaction is required")
	}
	if strings.TrimSpace(actor) == "" {
		return TeamBuildRun{}, errors.New("mark template published: actor is required")
	}
	if strings.TrimSpace(finalRef.Ref) == "" || strings.TrimSpace(finalRef.TeamID) == "" {
		return TeamBuildRun{}, errors.New("mark template published: artifact final ref is required")
	}
	locked, err := s.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template published: %w", err)
	}
	if locked.Status != StatusRoundRunning || locked.EffectiveExecutionStrategy() != ExecutionStrategyTemplateInstantiate {
		return TeamBuildRun{}, fmt.Errorf("mark template published: build run %q is not a running template build", buildRunID)
	}
	finalRefJSON, err := json.Marshal(finalRef)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template published: encode final ref: %w", err)
	}
	now := s.clock.Now()
	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='passed', updated_at=$3, decided_at=$3,
			publish_eligible=false, final_ref_json=$4::jsonb
		WHERE workspace_id=$1 AND build_run_id=$2
		  AND status='round_running' AND execution_strategy='template_instantiate'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, now, string(finalRefJSON)))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, fmt.Errorf("mark template published: build run %q changed", buildRunID)
	}
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template published: %w", err)
	}
	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template published: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'round_running','passed',
			'template workflow published and team activated',$4,$5)
	`, workspaceID, buildRunID, nextSeq, actor, now); err != nil {
		return TeamBuildRun{}, fmt.Errorf("mark template published ledger: %w", err)
	}
	return run, nil
}

// GetLatestBuildRunTransitionReason returns the reason recorded by the newest
// transition for one build run. Terminal presentation uses the append-only
// ledger as its source of truth instead of inferring a reason from model text.
func (s *Store) GetLatestBuildRunTransitionReason(
	ctx context.Context,
	workspaceID, buildRunID string,
) (string, error) {
	if workspaceID == "" || buildRunID == "" {
		return "", fmt.Errorf("get latest build run transition reason: %w", ErrBuildRunNotFound)
	}
	var reason string
	err := s.pool.QueryRow(ctx, `
		SELECT reason
		FROM weave_team_build_run_transitions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY seq DESC
		LIMIT 1
	`, workspaceID, buildRunID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("get latest build run transition reason: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("get latest build run transition reason: %w", err)
	}
	return reason, nil
}

func validateStatusTransition(from, to string) error {
	if !validStatuses[from] || !validStatuses[to] {
		return fmt.Errorf("%w: status must be one of planning, authorized, round_running, publishing, passed, blocked, cancelled", ErrInvalidTransition)
	}
	if !allowedTransitions[[2]string{from, to}] {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

func nextTransitionSeq(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
) (int64, error) {
	var nextSeq int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(seq),0)+1
		FROM weave_team_build_run_transitions
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID).Scan(&nextSeq); err != nil {
		return 0, fmt.Errorf("derive transition sequence: %w", err)
	}
	return nextSeq, nil
}
