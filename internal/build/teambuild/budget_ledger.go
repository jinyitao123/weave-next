package teambuild

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
)

const budgetLedgerColumns = `
	workspace_id, build_run_id, round_no, source_kind, source_role,
	source_run_id, input_tokens, output_tokens, cost_usd, tool_calls,
	created_at
`

func validateBudgetCharge(charge BudgetCharge) error {
	if charge.WorkspaceID == "" || charge.BuildRunID == "" {
		return errors.New("budget charge: workspace_id and build_run_id are required")
	}
	if charge.RoundNo < 1 {
		return errors.New("budget charge: round_no must be positive")
	}
	if strings.TrimSpace(charge.SourceKind) == "" ||
		strings.TrimSpace(charge.SourceRole) == "" ||
		strings.TrimSpace(charge.SourceRunID) == "" {
		return errors.New(
			"budget charge: source_kind, source_role, and source_run_id are required",
		)
	}
	if charge.InputTokens < 0 || charge.OutputTokens < 0 || charge.ToolCalls < 0 {
		return errors.New("budget charge: token and tool-call counts must be non-negative")
	}
	if charge.CostUSD < 0 || math.IsNaN(charge.CostUSD) || math.IsInf(charge.CostUSD, 0) {
		return errors.New("budget charge: cost_usd must be finite and non-negative")
	}
	return nil
}

// RecordBudgetUsage persists one source's usage charge in a new transaction.
// It is the standalone path; callers that persist budget charges inside a
// multi-row transaction should use RecordBudgetUsageTx instead.
func (s *Store) RecordBudgetUsage(
	ctx context.Context,
	workspaceID, buildRunID string,
	charge BudgetCharge,
) (BudgetCharge, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BudgetCharge{}, fmt.Errorf("begin record budget usage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recorded, err := s.RecordBudgetUsageTx(ctx, tx, workspaceID, buildRunID, charge)
	if err != nil {
		return BudgetCharge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BudgetCharge{}, fmt.Errorf("commit record budget usage: %w", err)
	}
	return recorded, nil
}

// RecordBudgetUsageTx appends one budget ledger row inside the caller-owned
// transaction. The identity (round_no, source_kind, source_run_id) is
// idempotent: replaying the same source with identical facts succeeds and
// returns the existing row; the same identity with different facts returns
// ErrBudgetUsageConflict and leaves the original row untouched. Different
// sources never contend, so concurrent writers cannot lose charges.
func (s *Store) RecordBudgetUsageTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	charge BudgetCharge,
) (BudgetCharge, error) {
	if tx == nil {
		return BudgetCharge{}, errors.New("record budget usage: transaction is required")
	}
	if err := validateBudgetCharge(charge); err != nil {
		return BudgetCharge{}, err
	}
	now := s.clock.Now().UTC()

	var recorded BudgetCharge
	err := tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_run_budget_ledger (
			workspace_id, build_run_id, round_no, source_kind, source_role,
			source_run_id, input_tokens, output_tokens, cost_usd, tool_calls,
			created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (
			workspace_id, build_run_id, round_no, source_kind, source_run_id
		) DO NOTHING
		RETURNING `+budgetLedgerColumns,
		workspaceID, buildRunID, charge.RoundNo, charge.SourceKind,
		charge.SourceRole, charge.SourceRunID, charge.InputTokens,
		charge.OutputTokens, charge.CostUSD, charge.ToolCalls, now,
	).Scan(
		&recorded.WorkspaceID, &recorded.BuildRunID, &recorded.RoundNo,
		&recorded.SourceKind, &recorded.SourceRole, &recorded.SourceRunID,
		&recorded.InputTokens, &recorded.OutputTokens, &recorded.CostUSD,
		&recorded.ToolCalls, &recorded.CreatedAt,
	)
	if err == nil {
		return recorded, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BudgetCharge{}, fmt.Errorf("record budget usage: %w", err)
	}

	// The source identity already exists. A replay must carry the exact same
	// facts (including role); anything else is a conflict and the original
	// row stays untouched.
	var existing BudgetCharge
	if err := tx.QueryRow(ctx, `
		SELECT `+budgetLedgerColumns+`
		FROM weave_team_build_run_budget_ledger
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
		  AND source_kind=$4 AND source_run_id=$5
	`, workspaceID, buildRunID, charge.RoundNo, charge.SourceKind,
		charge.SourceRunID).Scan(
		&existing.WorkspaceID, &existing.BuildRunID, &existing.RoundNo,
		&existing.SourceKind, &existing.SourceRole, &existing.SourceRunID,
		&existing.InputTokens, &existing.OutputTokens, &existing.CostUSD,
		&existing.ToolCalls, &existing.CreatedAt,
	); err != nil {
		return BudgetCharge{}, fmt.Errorf("record budget usage: read existing charge: %w", err)
	}
	if existing.SourceRole != charge.SourceRole ||
		existing.InputTokens != charge.InputTokens ||
		existing.OutputTokens != charge.OutputTokens ||
		existing.CostUSD != charge.CostUSD ||
		existing.ToolCalls != charge.ToolCalls {
		return BudgetCharge{}, fmt.Errorf(
			"record budget usage: %w: source %s/%s of round %d already recorded with different facts",
			ErrBudgetUsageConflict, charge.SourceKind, charge.SourceRunID,
			charge.RoundNo,
		)
	}
	return existing, nil
}

// GetBudgetUsage returns the whole-task consumption across every round.
// The sums come only from append-only ledger rows, so replay-safe writes can
// never double-count.
func (s *Store) GetBudgetUsage(
	ctx context.Context,
	workspaceID, buildRunID string,
) (BudgetUsage, error) {
	if workspaceID == "" || buildRunID == "" {
		return BudgetUsage{}, errors.New("get budget usage: workspace_id and build_run_id are required")
	}
	var usage BudgetUsage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(input_tokens),0)::bigint,
			COALESCE(SUM(output_tokens),0)::bigint,
			COALESCE(SUM(cost_usd),0),
			COALESCE(SUM(tool_calls),0)::bigint
		FROM weave_team_build_run_budget_ledger
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID).Scan(
		&usage.InputTokens, &usage.OutputTokens, &usage.CostUSD, &usage.ToolCalls,
	)
	if err != nil {
		return BudgetUsage{}, fmt.Errorf("get budget usage: %w", err)
	}
	return usage, nil
}

// GetRoundBudgetUsage returns one round's consumption from the budget
// ledger. The round-controller budget gate compares this against the
// round budget and the whole-run total against the total budget.
func (s *Store) GetRoundBudgetUsage(
	ctx context.Context,
	workspaceID, buildRunID string,
	roundNo int,
) (BudgetUsage, error) {
	if workspaceID == "" || buildRunID == "" || roundNo < 1 {
		return BudgetUsage{}, errors.New(
			"get round budget usage: workspace_id, build_run_id, and positive round_no are required",
		)
	}
	var usage BudgetUsage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(input_tokens),0)::bigint,
			COALESCE(SUM(output_tokens),0)::bigint,
			COALESCE(SUM(cost_usd),0),
			COALESCE(SUM(tool_calls),0)::bigint
		FROM weave_team_build_run_budget_ledger
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
	`, workspaceID, buildRunID, roundNo).Scan(
		&usage.InputTokens, &usage.OutputTokens, &usage.CostUSD, &usage.ToolCalls,
	)
	if err != nil {
		return BudgetUsage{}, fmt.Errorf("get round budget usage: %w", err)
	}
	return usage, nil
}

// EvaluateBudget compares the current round's and the whole-task's ledger
// consumption against their budgets at one checkpoint. A limit of zero means
// that dimension is unlimited. Exact equality is not exceeded and therefore
// never blocks a passing run.
func (s *Store) EvaluateBudget(
	ctx context.Context,
	workspaceID, buildRunID string,
	roundNo int,
	roundBudget, totalBudget Budget,
) (BudgetDecision, error) {
	if workspaceID == "" || buildRunID == "" || roundNo < 1 {
		return BudgetDecision{}, errors.New(
			"evaluate budget: workspace_id, build_run_id, and positive round_no are required",
		)
	}
	roundUsage, err := s.GetRoundBudgetUsage(ctx, workspaceID, buildRunID, roundNo)
	if err != nil {
		return BudgetDecision{}, fmt.Errorf("evaluate budget: %w", err)
	}
	totalUsage, err := s.GetBudgetUsage(ctx, workspaceID, buildRunID)
	if err != nil {
		return BudgetDecision{}, fmt.Errorf("evaluate budget: %w", err)
	}
	return evaluateBudgetUsage(roundUsage, totalUsage, roundBudget, totalBudget), nil
}

func evaluateBudgetUsage(
	roundUsage, totalUsage BudgetUsage,
	roundBudget, totalBudget Budget,
) BudgetDecision {
	decision := BudgetDecision{}
	compareBudgetIntDimension(&decision, "round", "input_tokens",
		roundUsage.InputTokens, roundBudget.MaxInputTokens)
	compareBudgetIntDimension(&decision, "round", "output_tokens",
		roundUsage.OutputTokens, roundBudget.MaxOutputTokens)
	compareBudgetIntDimension(&decision, "round", "tool_calls",
		roundUsage.ToolCalls, roundBudget.MaxToolCalls)
	compareBudgetFloatDimension(&decision, "round", "cost_usd",
		roundUsage.CostUSD, roundBudget.MaxCostUSD)
	compareBudgetIntDimension(&decision, "total", "input_tokens",
		totalUsage.InputTokens, totalBudget.MaxInputTokens)
	compareBudgetIntDimension(&decision, "total", "output_tokens",
		totalUsage.OutputTokens, totalBudget.MaxOutputTokens)
	compareBudgetIntDimension(&decision, "total", "tool_calls",
		totalUsage.ToolCalls, totalBudget.MaxToolCalls)
	compareBudgetFloatDimension(&decision, "total", "cost_usd",
		totalUsage.CostUSD, totalBudget.MaxCostUSD)
	return decision
}

// compareBudgetIntDimension classifies one integer dimension's consumption
// against its cap with exact integer semantics.
func compareBudgetIntDimension(
	decision *BudgetDecision,
	scope, name string,
	consumed, limit int64,
) {
	if limit <= 0 {
		return
	}
	label := scope + " " + name
	if consumed > limit {
		decision.ExceededDims = append(decision.ExceededDims, label)
	}
}

// compareBudgetFloatDimension classifies the USD cost dimension against its
// cap. A zero cap means unlimited, so the dimension is skipped entirely.
func compareBudgetFloatDimension(
	decision *BudgetDecision,
	scope, name string,
	consumed, limit float64,
) {
	if limit <= 0 {
		return
	}
	label := scope + " " + name
	if consumed > limit {
		decision.ExceededDims = append(decision.ExceededDims, label)
	}
}

const usageSourceColumns = `
	workspace_id, build_run_id, round_no, source_kind, source_role,
	source_run_id, created_at
`

func validateUsageSource(source BuildUsageSource) error {
	if source.WorkspaceID == "" || source.BuildRunID == "" {
		return errors.New("usage source: workspace_id and build_run_id are required")
	}
	if source.RoundNo < 1 {
		return errors.New("usage source: round_no must be positive")
	}
	if strings.TrimSpace(source.SourceKind) == "" ||
		strings.TrimSpace(source.SourceRole) == "" ||
		strings.TrimSpace(source.SourceRunID) == "" {
		return errors.New(
			"usage source: source_kind, source_role, and source_run_id are required",
		)
	}
	return nil
}

func scanUsageSource(row pgx.Rows) (BuildUsageSource, error) {
	var source BuildUsageSource
	err := row.Scan(
		&source.WorkspaceID, &source.BuildRunID, &source.RoundNo,
		&source.SourceKind, &source.SourceRole, &source.SourceRunID,
		&source.CreatedAt,
	)
	return source, err
}

// RecordUsageSource persists one runtime-run association in a new
// transaction. It is the standalone path used by the admission-time usage
// attribution hook; callers that persist usage sources inside a multi-row
// transaction should use RecordUsageSourceTx instead.
func (s *Store) RecordUsageSource(
	ctx context.Context,
	workspaceID, buildRunID string,
	source BuildUsageSource,
) (BuildUsageSource, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BuildUsageSource{}, fmt.Errorf("begin record usage source: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recorded, err := s.RecordUsageSourceTx(ctx, tx, workspaceID, buildRunID, source)
	if err != nil {
		return BuildUsageSource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BuildUsageSource{}, fmt.Errorf("commit record usage source: %w", err)
	}
	return recorded, nil
}

// RecordUsageSourceTx appends one usage source row inside the caller-owned
// transaction. The identity (round_no, source_kind, source_run_id) is
// idempotent: replaying the same identity with the same role succeeds and
// returns the existing row; the same identity with a different role returns
// ErrUsageSourceConflict and leaves the original row untouched. Different
// roles never contend, so the admission hook can never lose an association.
func (s *Store) RecordUsageSourceTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	source BuildUsageSource,
) (BuildUsageSource, error) {
	if tx == nil {
		return BuildUsageSource{}, errors.New("record usage source: transaction is required")
	}
	if err := validateUsageSource(source); err != nil {
		return BuildUsageSource{}, err
	}
	now := s.clock.Now().UTC()

	var recorded BuildUsageSource
	err := tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_run_usage_sources (
			workspace_id, build_run_id, round_no, source_kind, source_role,
			source_run_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (
			workspace_id, build_run_id, round_no, source_kind, source_run_id
		) DO NOTHING
		RETURNING `+usageSourceColumns,
		workspaceID, buildRunID, source.RoundNo, source.SourceKind,
		source.SourceRole, source.SourceRunID, now,
	).Scan(
		&recorded.WorkspaceID, &recorded.BuildRunID, &recorded.RoundNo,
		&recorded.SourceKind, &recorded.SourceRole, &recorded.SourceRunID,
		&recorded.CreatedAt,
	)
	if err == nil {
		return recorded, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BuildUsageSource{}, fmt.Errorf("record usage source: %w", err)
	}

	// The identity already exists. A replay must carry the exact same role;
	// anything else is a conflict and the original row stays untouched.
	var existing BuildUsageSource
	if err := tx.QueryRow(ctx, `
		SELECT `+usageSourceColumns+`
		FROM weave_team_build_run_usage_sources
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
		  AND source_kind=$4 AND source_run_id=$5
	`, workspaceID, buildRunID, source.RoundNo, source.SourceKind,
		source.SourceRunID).Scan(
		&existing.WorkspaceID, &existing.BuildRunID, &existing.RoundNo,
		&existing.SourceKind, &existing.SourceRole, &existing.SourceRunID,
		&existing.CreatedAt,
	); err != nil {
		return BuildUsageSource{}, fmt.Errorf("record usage source: read existing association: %w", err)
	}
	if existing.SourceRole != source.SourceRole {
		return BuildUsageSource{}, fmt.Errorf(
			"record usage source: %w: source %s/%s of round %d already associated with role %q",
			ErrUsageSourceConflict, source.SourceKind, source.SourceRunID,
			source.RoundNo, existing.SourceRole,
		)
	}
	return existing, nil
}

// ListUsageSources returns every recorded usage source of one build round in
// creation order. Build uses it to decide which roles are already associated
// and therefore must not be started again.
func (s *Store) ListUsageSources(
	ctx context.Context,
	workspaceID, buildRunID string,
	roundNo int,
) ([]BuildUsageSource, error) {
	if workspaceID == "" || buildRunID == "" || roundNo < 1 {
		return nil, errors.New(
			"list usage sources: workspace_id, build_run_id, and positive round_no are required",
		)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+usageSourceColumns+`
		FROM weave_team_build_run_usage_sources
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
		ORDER BY created_at, source_role, source_run_id
	`, workspaceID, buildRunID, roundNo)
	if err != nil {
		return nil, fmt.Errorf("list usage sources: %w", err)
	}
	defer rows.Close()
	sources := make([]BuildUsageSource, 0)
	for rows.Next() {
		source, err := scanUsageSource(rows)
		if err != nil {
			return nil, fmt.Errorf("list usage sources: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage sources: %w", err)
	}
	return sources, nil
}

// ListUnchargedUsageSources returns the usage sources of one build round that
// have no T14A budget ledger row yet. Reconcile drains this list: each source
// either gets backfilled from its final terminal marker or stays pending.
func (s *Store) ListUnchargedUsageSources(
	ctx context.Context,
	workspaceID, buildRunID string,
	roundNo int,
) ([]BuildUsageSource, error) {
	if workspaceID == "" || buildRunID == "" || roundNo < 1 {
		return nil, errors.New(
			"list uncharged usage sources: workspace_id, build_run_id, and positive round_no are required",
		)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.workspace_id, s.build_run_id, s.round_no, s.source_kind,
			s.source_role, s.source_run_id, s.created_at
		FROM weave_team_build_run_usage_sources s
		LEFT JOIN weave_team_build_run_budget_ledger l
		  ON l.workspace_id = s.workspace_id
		 AND l.build_run_id = s.build_run_id
		 AND l.round_no = s.round_no
		 AND l.source_kind = s.source_kind
		 AND l.source_run_id = s.source_run_id
		WHERE s.workspace_id=$1 AND s.build_run_id=$2 AND s.round_no=$3
		  AND l.workspace_id IS NULL
		ORDER BY s.created_at, s.source_role, s.source_run_id
	`, workspaceID, buildRunID, roundNo)
	if err != nil {
		return nil, fmt.Errorf("list uncharged usage sources: %w", err)
	}
	defer rows.Close()
	sources := make([]BuildUsageSource, 0)
	for rows.Next() {
		source, err := scanUsageSource(rows)
		if err != nil {
			return nil, fmt.Errorf("list uncharged usage sources: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list uncharged usage sources: %w", err)
	}
	return sources, nil
}
