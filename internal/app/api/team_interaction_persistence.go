package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/teamcompiler"
)

const maxTeamInteractionAttemptsPerRoute = 2

var errTeamInteractionRetryExhausted = errors.New("delegate_retry_exhausted")

type persistentTeamInteractionAssembler struct {
	inner teamcompiler.TeamInteractionAssembler
	pool  *pgxpool.Pool
}

func (a *persistentTeamInteractionAssembler) Assemble(
	ctx context.Context,
	input teamcompiler.TeamInteractionInput,
) (teamcompiler.TeamInteractionCatalog, error) {
	return a.inner.Assemble(ctx, input)
}

func (a *persistentTeamInteractionAssembler) AuthorizeInteraction(
	ctx context.Context,
	catalog teamcompiler.TeamInteractionCatalog,
	req teamcompiler.InteractionAuthorizationRequest,
) (teamcompiler.AuthorizedWorker, error) {
	worker, err := a.inner.AuthorizeInteraction(ctx, catalog, req)
	if err != nil || a.pool == nil {
		return worker, err
	}
	routeKey := interactionRouteKey(req)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return teamcompiler.AuthorizedWorker{}, fmt.Errorf("record team interaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var attempts int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM weave_team_run_interactions
		WHERE workspace_id=$1 AND run_id=$2 AND route_key=$3
	`, req.WorkspaceID, req.RunID, routeKey).Scan(&attempts); err != nil {
		return teamcompiler.AuthorizedWorker{}, fmt.Errorf("record team interaction: %w", err)
	}
	if attempts >= maxTeamInteractionAttemptsPerRoute {
		return teamcompiler.AuthorizedWorker{}, errTeamInteractionRetryExhausted
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_run_interactions (
			workspace_id, run_id, route_key, worker_agent_id, kind,
			attempt_no, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
	`, req.WorkspaceID, req.RunID, routeKey, req.WorkerAgentID, string(req.Kind), attempts+1, now); err != nil {
		return teamcompiler.AuthorizedWorker{}, fmt.Errorf("record team interaction: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return teamcompiler.AuthorizedWorker{}, fmt.Errorf("record team interaction: %w", err)
	}
	return worker, nil
}

func (a *persistentTeamInteractionAssembler) RecordInteractionFailure(
	ctx context.Context,
	_ teamcompiler.TeamInteractionCatalog,
	req teamcompiler.InteractionAuthorizationRequest,
	record teamcompiler.InteractionFailureRecord,
) error {
	if a == nil || a.pool == nil {
		return nil
	}
	failureClass := strings.TrimSpace(record.FailureClass)
	if failureClass == "" {
		failureClass = "infra"
	}
	routeKey := interactionRouteKey(req)
	_, err := a.pool.Exec(ctx, `
		UPDATE weave_team_run_interactions
		SET failure_class=$4, failure_cause=$5, updated_at=$6
		WHERE workspace_id=$1 AND run_id=$2 AND route_key=$3
		  AND attempt_no = (
		    SELECT max(attempt_no)
		    FROM weave_team_run_interactions
		    WHERE workspace_id=$1 AND run_id=$2 AND route_key=$3
		  )
	`, req.WorkspaceID, req.RunID, routeKey, failureClass, strings.TrimSpace(record.FailureCause), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record team interaction failure: %w", err)
	}
	return nil
}

func interactionRouteKey(req teamcompiler.InteractionAuthorizationRequest) string {
	if strings.TrimSpace(req.RouteKey) != "" {
		return req.RouteKey
	}
	return string(req.Kind) + ":" + req.WorkerAgentID
}
