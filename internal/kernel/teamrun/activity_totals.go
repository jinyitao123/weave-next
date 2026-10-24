package teamrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ActivityStageTotals sums one step's activity over the whole run, unlike the
// bounded recent window: how many times the step started (each round of a
// check loop starts it again), the tokens of its finished invocations, and
// the models its calls were actually sent to.
type ActivityStageTotals struct {
	NodeID       string
	MemberID     string
	Rounds       int
	InputTokens  int64
	OutputTokens int64
	Models       []string
}

// StageTotals aggregates every step of a run in one statement. Models come
// from the request a model call wrote, not the caller's label for it.
func (store *PGActivityStore) StageTotals(ctx context.Context, workspaceID, runID string) ([]ActivityStageTotals, error) {
	if store == nil || store.Transactions == nil {
		return nil, errors.New("team run activity store is unavailable")
	}
	query := `SELECT node_id, COALESCE(member_id,''),
		count(*) FILTER (WHERE kind='member_started'),
		COALESCE(sum((detail->>'input_tokens')::bigint) FILTER (WHERE kind IN ('member_completed','member_failed') AND jsonb_typeof(detail->'input_tokens')='number'),0),
		COALESCE(sum((detail->>'output_tokens')::bigint) FILTER (WHERE kind IN ('member_completed','member_failed') AND jsonb_typeof(detail->'output_tokens')='number'),0),
		COALESCE(array_agg(DISTINCT detail->'request'->>'model' ORDER BY detail->'request'->>'model')
			FILTER (WHERE kind='model_completed' AND jsonb_typeof(detail->'request'->'model')='string' AND detail->'request'->>'model'<>''),'{}')
		FROM weave_team_run_activity_events
		WHERE workspace_id=$1 AND run_id=$2 AND node_id IS NOT NULL AND node_id<>''
		GROUP BY node_id, member_id`
	var rows pgx.Rows
	var err error
	if querier, ok := store.Transactions.(interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}); ok {
		rows, err = querier.Query(ctx, query, workspaceID, runID)
	} else {
		tx, beginErr := store.Transactions.Begin(ctx)
		if beginErr != nil {
			return nil, fmt.Errorf("begin total team run activity: %w", beginErr)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err = tx.Query(ctx, query, workspaceID, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("total team run activity: %w", err)
	}
	defer rows.Close()
	totals := []ActivityStageTotals{}
	for rows.Next() {
		var total ActivityStageTotals
		if err := rows.Scan(&total.NodeID, &total.MemberID, &total.Rounds, &total.InputTokens, &total.OutputTokens, &total.Models); err != nil {
			return nil, fmt.Errorf("scan team run activity totals: %w", err)
		}
		totals = append(totals, total)
	}
	return totals, rows.Err()
}
