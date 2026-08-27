package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ReauthorizeBudgetBlockedRun restores only a run whose newest terminal
// transition is the structured budget_exhausted reason. The frozen brief and
// its hash stay unchanged; the newly minted receipt binds the effective
// increased budgets persisted on the BuildRun.
func (s *Store) ReauthorizeBudgetBlockedRun(
	ctx context.Context,
	workspaceID, buildRunID, confirmedBy string,
	roundBudget, totalBudget Budget,
) (TeamBuildRun, BuildAuthorizationReceipt, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", ErrBuildRunNotFound,
		)
	}
	if strings.TrimSpace(confirmedBy) == "" {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w: confirmed_by is required",
			ErrBudgetReauthorizationInvalid,
		)
	}
	if err := validateBudget(roundBudget); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w: round budget: %v",
			ErrBudgetReauthorizationInvalid, err,
		)
	}
	if err := validateBudget(totalBudget); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w: total budget: %v",
			ErrBudgetReauthorizationInvalid, err,
		)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"begin reauthorize budget-blocked run: %w", err,
		)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := s.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", err,
		)
	}
	if locked.Status != StatusBlocked || strings.TrimSpace(locked.ConfirmedBy) == "" {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", ErrBudgetReauthorizationRequired,
		)
	}
	var latestReason string
	if err := tx.QueryRow(ctx, `
		SELECT reason
		FROM weave_team_build_run_transitions
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY seq DESC
		LIMIT 1
	`, workspaceID, buildRunID).Scan(&latestReason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
				"reauthorize budget-blocked run: %w", ErrBudgetReauthorizationRequired,
			)
		}
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: load terminal reason: %w", err,
		)
	}
	if latestReason != BudgetExhaustedReason {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", ErrBudgetReauthorizationRequired,
		)
	}
	if !s.clock.Now().Before(locked.ExpiresAt) {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", ErrReceiptExpired,
		)
	}
	if !budgetCovers(roundBudget, locked.RoundBudget) ||
		!budgetCovers(totalBudget, locked.TotalBudget) ||
		(!budgetStrictlyIncreased(roundBudget, locked.RoundBudget) &&
			!budgetStrictlyIncreased(totalBudget, locked.TotalBudget)) {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w: budgets must be non-decreasing and increase at least one bound",
			ErrBudgetReauthorizationInvalid,
		)
	}

	roundNo := 1
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(round_no), 1)
		FROM weave_team_build_run_budget_ledger
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID).Scan(&roundNo); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: find current round: %w", err,
		)
	}
	decision, err := s.EvaluateBudgetTx(
		ctx, tx, workspaceID, buildRunID, roundNo, roundBudget, totalBudget,
	)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: evaluate increased budget: %w", err,
		)
	}
	if len(decision.ExceededDims) > 0 {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w: increased budget still exceeded: %s",
			ErrBudgetReauthorizationInvalid, strings.Join(decision.ExceededDims, ", "),
		)
	}

	roundBudgetJSON, err := json.Marshal(roundBudget)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, err
	}
	totalBudgetJSON, err := json.Marshal(totalBudget)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, err
	}
	now := s.clock.Now()
	run, err := scanBuildRun(tx.QueryRow(ctx, `
		UPDATE weave_team_build_runs
		SET status='authorized', round_budget_json=$3::jsonb,
			total_budget_json=$4::jsonb, confirmed_by=$5,
			authorization_decided_at=$6, decided_at=NULL,
			publish_eligible=false, final_ref_json=NULL, updated_at=$6
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='blocked'
		RETURNING `+buildRunColumns+`
	`, workspaceID, buildRunID, string(roundBudgetJSON), string(totalBudgetJSON), confirmedBy, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", ErrBudgetReauthorizationRequired,
		)
	}
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: update run: %w", err,
		)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_build_operation_steps
		SET status='pending', lease_owner=NULL, lease_until=NULL,
			error_class=NULL, error_code=NULL, evidence_json=NULL,
			output_hash=NULL, completed_at=NULL, updated_at=$3
		WHERE workspace_id=$1 AND build_run_id=$2
		  AND status='failed' AND error_class='budget_exhausted'
	`, workspaceID, buildRunID, now); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: reset budget operation: %w", err,
		)
	}
	nextSeq, err := nextTransitionSeq(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run: %w", err,
		)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_build_run_transitions (
			workspace_id, build_run_id, seq, from_status, to_status,
			reason, actor, created_at
		) VALUES ($1,$2,$3,'blocked','authorized',
			'workspace admin reauthorized increased budget',$4,$5)
	`, workspaceID, buildRunID, nextSeq, confirmedBy, now); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"reauthorize budget-blocked run transition: %w", err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamBuildRun{}, BuildAuthorizationReceipt{}, fmt.Errorf(
			"commit reauthorize budget-blocked run: %w", err,
		)
	}

	receipt := BuildAuthorizationReceipt{
		workspaceID: workspaceID, buildRunID: buildRunID,
		contractHash: run.ContractHash, mode: run.Mode,
		authority:       run.Authorization.Authority,
		revisionToken:   cloneBlueprintRevisionToken(run.Authorization.RevisionToken),
		decisionSubject: run.Authorization.DecisionSubject,
		decisionReason:  run.Authorization.DecisionReason,
		assetScope:      cloneAssetScope(run.AssetScope),
		roundBudget:     roundBudget, totalBudget: totalBudget,
		expiresAt: run.ExpiresAt, confirmedBy: confirmedBy, createdAt: now,
	}
	return run, receipt, nil
}

func budgetCovers(increased, previous Budget) bool {
	return capCoversInt(increased.MaxInputTokens, previous.MaxInputTokens) &&
		capCoversInt(increased.MaxOutputTokens, previous.MaxOutputTokens) &&
		capCoversInt(increased.MaxToolCalls, previous.MaxToolCalls) &&
		capCoversFloat(increased.MaxCostUSD, previous.MaxCostUSD)
}

func budgetStrictlyIncreased(increased, previous Budget) bool {
	return capGreaterInt(increased.MaxInputTokens, previous.MaxInputTokens) ||
		capGreaterInt(increased.MaxOutputTokens, previous.MaxOutputTokens) ||
		capGreaterInt(increased.MaxToolCalls, previous.MaxToolCalls) ||
		capGreaterFloat(increased.MaxCostUSD, previous.MaxCostUSD)
}

func capCoversInt(increased, previous int64) bool {
	return increased == 0 || (previous != 0 && increased >= previous)
}

func capGreaterInt(increased, previous int64) bool {
	return previous != 0 && (increased == 0 || increased > previous)
}

func capCoversFloat(increased, previous float64) bool {
	return increased == 0 || (previous != 0 && increased >= previous)
}

func capGreaterFloat(increased, previous float64) bool {
	return previous != 0 && (increased == 0 || increased > previous)
}
