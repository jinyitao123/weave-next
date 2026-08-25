package teambuild

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrTemplateAuthorizationRequired = errors.New("template build requires manual authorization")
	ErrTemplateDailyQuotaExceeded    = errors.New("template daily budget quota exceeded")
	ErrTemplateMonthlyQuotaExceeded  = errors.New("template monthly budget quota exceeded")
	ErrTemplateConcurrencyExceeded   = errors.New("template concurrent build limit exceeded")
)

// AuthorizeTemplateBuildRun is the only server-owned template_auto entry.
// The real authenticated user remains confirmed_by; the platform decision
// principal and reason are generated and persisted by AuthorizeBuildRun.
func (s *Store) AuthorizeTemplateBuildRun(
	ctx context.Context,
	workspaceID, buildRunID, confirmedBy string,
	revisionToken BlueprintRevisionToken,
	policy TemplateAuthorizationPolicy,
) (TeamBuildRun, BuildAuthorizationReceipt, error) {
	return s.AuthorizeBuildRun(ctx, workspaceID, buildRunID, confirmedBy, nil, AuthorizeOptions{
		Authority:       AuthorizationTemplateAuto,
		RevisionToken:   &revisionToken,
		DecisionSubject: TemplateAuthorizerSubject,
		TemplatePolicy:  &policy,
	})
}

func validateTemplateAuthorizationPolicy(policy TemplateAuthorizationPolicy) error {
	values := []float64{policy.AutoBudgetThresholdUSD, policy.DailyBudgetUSD, policy.MonthlyBudgetUSD}
	for _, value := range values {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("template authorization monetary limits must be finite and positive")
		}
	}
	if policy.DailyBudgetUSD < policy.AutoBudgetThresholdUSD {
		return errors.New("template daily budget must cover the per-build automatic threshold")
	}
	if policy.MonthlyBudgetUSD < policy.DailyBudgetUSD {
		return errors.New("template monthly budget must cover the daily budget")
	}
	if policy.MaxConcurrent < 1 {
		return errors.New("template max concurrent builds must be positive")
	}
	return nil
}

func (s *Store) reserveTemplateAuthorizationTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	requestedBudget float64,
	policy TemplateAuthorizationPolicy,
	now time.Time,
) (string, error) {
	if err := validateTemplateAuthorizationPolicy(policy); err != nil {
		return "", err
	}
	if requestedBudget <= 0 || math.IsNaN(requestedBudget) || math.IsInf(requestedBudget, 0) {
		return "", errors.New("template build declared budget must be finite and positive")
	}
	if requestedBudget > policy.AutoBudgetThresholdUSD {
		return "", fmt.Errorf(
			"%w: requested $%.2f exceeds automatic threshold $%.2f",
			ErrTemplateAuthorizationRequired, requestedBudget, policy.AutoBudgetThresholdUSD,
		)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, workspaceID+":template_auto"); err != nil {
		return "", fmt.Errorf("lock template authorization quota: %w", err)
	}

	var concurrent int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id<>$2
		  AND execution_strategy='template_instantiate'
		  AND status IN ('authorized','round_running')
	`, workspaceID, buildRunID).Scan(&concurrent); err != nil {
		return "", fmt.Errorf("read template concurrent quota: %w", err)
	}
	if concurrent >= policy.MaxConcurrent {
		return "", fmt.Errorf("%w: %d active, limit %d", ErrTemplateConcurrencyExceeded, concurrent, policy.MaxConcurrent)
	}

	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var dailyReserved, monthlyReserved float64
	if err := tx.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM((total_budget_json->>'max_cost_usd')::double precision)
		    FILTER (WHERE authorization_decided_at >= $2), 0),
		  COALESCE(SUM((total_budget_json->>'max_cost_usd')::double precision)
		    FILTER (WHERE authorization_decided_at >= $3), 0)
		FROM weave_team_build_runs
		WHERE workspace_id=$1
		  AND execution_strategy='template_instantiate'
		  AND authorization_authority IS NOT NULL
		  AND authorization_decided_at >= $3
	`, workspaceID, dayStart, monthStart).Scan(&dailyReserved, &monthlyReserved); err != nil {
		return "", fmt.Errorf("read template aggregate quota: %w", err)
	}
	if dailyReserved+requestedBudget > policy.DailyBudgetUSD {
		return "", fmt.Errorf(
			"%w: reserved $%.2f + requested $%.2f exceeds $%.2f",
			ErrTemplateDailyQuotaExceeded, dailyReserved, requestedBudget, policy.DailyBudgetUSD,
		)
	}
	if monthlyReserved+requestedBudget > policy.MonthlyBudgetUSD {
		return "", fmt.Errorf(
			"%w: reserved $%.2f + requested $%.2f exceeds $%.2f",
			ErrTemplateMonthlyQuotaExceeded, monthlyReserved, requestedBudget, policy.MonthlyBudgetUSD,
		)
	}
	return fmt.Sprintf(
		"declared budget $%.2f is within automatic threshold $%.2f; workspace reservations after approval: daily $%.2f/$%.2f, monthly $%.2f/$%.2f, concurrent %d/%d",
		requestedBudget, policy.AutoBudgetThresholdUSD,
		dailyReserved+requestedBudget, policy.DailyBudgetUSD,
		monthlyReserved+requestedBudget, policy.MonthlyBudgetUSD,
		concurrent+1, policy.MaxConcurrent,
	), nil
}
