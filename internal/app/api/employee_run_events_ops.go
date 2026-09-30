package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmployeeRunEventFailure is one outbox event Forge permanently rejected. The
// worker never retries it on its own: a 4xx other than 429 means the request or
// the deployment is wrong, so an operator fixes the cause and requeues it.
type EmployeeRunEventFailure struct {
	EventID     string    `json:"event_id"`
	WorkspaceID string    `json:"workspace_id"`
	RunID       string    `json:"run_id"`
	Attempts    int       `json:"delivery_attempts"`
	LastError   string    `json:"last_error"`
	FailedAt    time.Time `json:"failed_at"`
}

// ListEmployeeRunEventFailures returns every permanently failed event, oldest
// first. Employees never see these results, so this is the operator's only view.
func ListEmployeeRunEventFailures(ctx context.Context, pool *pgxpool.Pool) ([]EmployeeRunEventFailure, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	rows, err := pool.Query(ctx, `SELECT event_id::text,workspace_id,run_id,delivery_attempts,COALESCE(last_error,''),updated_at
		FROM weave_employee_run_event_outbox WHERE delivery_state='permanent_failure' ORDER BY updated_at,event_id`)
	if err != nil {
		return nil, fmt.Errorf("list permanent employee run event failures: %w", err)
	}
	defer rows.Close()
	failures := []EmployeeRunEventFailure{}
	for rows.Next() {
		var failure EmployeeRunEventFailure
		if err := rows.Scan(&failure.EventID, &failure.WorkspaceID, &failure.RunID, &failure.Attempts, &failure.LastError, &failure.FailedAt); err != nil {
			return nil, fmt.Errorf("scan permanent employee run event failure: %w", err)
		}
		failures = append(failures, failure)
	}
	return failures, rows.Err()
}

// RequeueEmployeeRunEventFailures returns permanently failed events to pending so
// the worker delivers them again, and reports how many it moved. With no IDs it
// requeues every permanent failure. Only permanent_failure rows move: delivered,
// pending and in-flight events are left alone, and the event facts stay
// immutable. Forge deduplicates by the event's idempotency key, so a requeue of
// an event Forge already accepted is answered as a duplicate, not delivered twice.
func RequeueEmployeeRunEventFailures(ctx context.Context, pool *pgxpool.Pool, eventIDs []string) (int64, error) {
	if pool == nil {
		return 0, errors.New("database pool is required")
	}
	for _, id := range eventIDs {
		if _, err := uuid.Parse(id); err != nil {
			return 0, fmt.Errorf("invalid event ID %q: event IDs are UUIDs as printed by list-failed", id)
		}
	}
	const requeue = `UPDATE weave_employee_run_event_outbox SET delivery_state='pending',claimed_at=NULL,
		next_attempt_at=statement_timestamp(),updated_at=statement_timestamp()
		WHERE delivery_state='permanent_failure'`
	var tag interface{ RowsAffected() int64 }
	var err error
	if len(eventIDs) == 0 {
		tag, err = pool.Exec(ctx, requeue)
	} else {
		tag, err = pool.Exec(ctx, requeue+` AND event_id = ANY($1::uuid[])`, eventIDs)
	}
	if err != nil {
		return 0, fmt.Errorf("requeue permanent employee run event failures: %w", err)
	}
	return tag.RowsAffected(), nil
}
