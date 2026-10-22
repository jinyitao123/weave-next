package taskqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ClaimBatch claims workflow tasks using the same eligibility and fencing as
// Claim. Each physical claim has its own worker identity, including its epoch.
// Runtime-pinned engine tasks must continue through their runtime claim gate.
func (s *Store) ClaimBatch(ctx context.Context, process string, filter ClaimFilter, limit int) ([]*Task, error) {
	if process == "" || filter.Kind != "team_workflow" || filter.IdentityKind != IdentityTeamWorkflow || limit < 1 || limit > 64 {
		return nil, errors.New("workflow batch claim requires a process identity and limit 1..64")
	}
	now, expiry := s.leaseTimes()
	rows, err := s.pool.Query(ctx, `UPDATE weave_task_queue
		SET status='running', worker_id=$1 || ':' || id || ':' || (claim_epoch+1)::text,
		    started_at=$2, lease_expires_at=$3, updated_at=$2,
		    claim_epoch=claim_epoch+1, unreported_attempts=unreported_attempts+1
		WHERE id IN (
		 SELECT id FROM weave_task_queue
		 WHERE status='queued' AND available_at <= $2 AND kind='team_workflow' AND identity_kind=$4
		   AND ($5='' OR workspace_id=$5) AND ($6='' OR run_snapshot_id=$6)
		   AND (parent_task_id IS NULL OR EXISTS (
		     SELECT 1 FROM weave_task_queue parent
		     WHERE parent.workspace_id=weave_task_queue.workspace_id AND parent.id=weave_task_queue.parent_task_id
		       AND parent.actor_subject=weave_task_queue.actor_subject AND parent.status IN ('queued','dispatched','running')))
		 ORDER BY priority DESC, created_at, id FOR UPDATE SKIP LOCKED LIMIT $7
		) RETURNING `+taskColumns, process, now, expiry, filter.IdentityKind, filter.WorkspaceID, filter.RunSnapshotID, limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflow batch: %w", err)
	}
	defer rows.Close()
	tasks := make([]*Task, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

type LeaseClaim struct {
	ID, WorkerID string
	Epoch        int64
}

// RenewBatch returns only the claims still owned and unexpired. Missing claims
// must stop; neither cancellation nor a stale epoch can acquire a fresh lease.
func (s *Store) RenewBatch(ctx context.Context, claims []LeaseClaim) (map[string]bool, error) {
	ids, workers, epochs := make([]string, 0, len(claims)), make([]string, 0, len(claims)), make([]int64, 0, len(claims))
	for _, claim := range claims {
		if claim.ID == "" || claim.WorkerID == "" || claim.Epoch < 1 {
			return nil, errors.New("invalid lease claim")
		}
		ids, workers, epochs = append(ids, claim.ID), append(workers, claim.WorkerID), append(epochs, claim.Epoch)
	}
	renewed := make(map[string]bool, len(claims))
	if len(claims) == 0 {
		return renewed, nil
	}
	now, expiry := s.leaseTimes()
	rows, err := s.pool.Query(ctx, `UPDATE weave_task_queue q SET lease_expires_at=$4, updated_at=$5
		FROM unnest($1::text[],$2::text[],$3::bigint[]) AS owner(id,worker,epoch)
		WHERE q.id=owner.id AND q.worker_id=owner.worker AND q.claim_epoch=owner.epoch
		  AND q.status='running' AND q.lease_expires_at >= $5 RETURNING q.id`, ids, workers, epochs, expiry, now)
	if err != nil {
		return nil, fmt.Errorf("renew workflow batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		renewed[id] = true
	}
	return renewed, rows.Err()
}

// ReadyWorkspaces lets each executor rotate across workspaces rather than let
// the oldest workspace backlog occupy all newly available execution slots.
func (s *Store) ReadyWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT workspace_id FROM weave_task_queue
		WHERE status='queued' AND kind='team_workflow' AND identity_kind=$1 AND available_at <= $2 ORDER BY workspace_id`, IdentityTeamWorkflow, s.clock.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workspaces []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, id)
	}
	return workspaces, rows.Err()
}

// ListenWorkflowTasks uses one dedicated connection. Notifications are hints;
// polling and the durable lease predicate remain authoritative after reconnects.
func (s *Store) ListenWorkflowTasks(ctx context.Context, notify func(string, string)) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN weave_task_ready; LISTEN weave_task_cancel"); err != nil {
		return err
	}
	for {
		event, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		notify(event.Channel, event.Payload)
	}
}
