package taskqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ResumeTaskTx reopens a settled task after the caller has authorized its next
// attempt. A retained execution owner means the previous outcome is unresolved.
// Identity and payload remain frozen; Claim allocates the next attempt epoch.
func (s *Store) ResumeTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, id, kind, contextKey string) (*Task, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	task, err := scanTask(tx.QueryRow(ctx, `UPDATE weave_task_queue
		SET status=$5,result=NULL,error=NULL,run_id=NULL,
			lease_expires_at=NULL,started_at=NULL,completed_at=NULL,updated_at=$6,available_at=$6
			WHERE workspace_id=$1 AND id=$2 AND kind=$3 AND context_key=$4
				AND status IN ($7,$8) AND worker_id IS NULL AND stopped_epoch=claim_epoch AND (deadline_at IS NULL OR deadline_at>$6) AND ($9::jsonb IS NULL OR actor_subject=$9::jsonb)
				AND (parent_task_id IS NULL OR EXISTS (
					SELECT 1 FROM weave_task_queue parent WHERE parent.workspace_id=weave_task_queue.workspace_id
					AND parent.id=weave_task_queue.parent_task_id AND parent.actor_subject=weave_task_queue.actor_subject
					AND parent.status IN ('queued','dispatched','running')))
		RETURNING `+taskColumns, workspaceID, id, kind, contextKey,
		StatusQueued, s.clock.Now(), StatusCompleted, StatusFailed, subjectFilter(ctx)))
	if err != nil {
		return nil, fmt.Errorf("task has not settled or cannot be resumed: %w", err)
	}
	return task, nil
}

// ReopenStoppedWorkflowTaskTx returns a workflow task whose execution lease
// expired to the queue, only once that exact execution's stop is proven: the
// task failed awaiting outcome reconciliation, claimEpoch is still its latest
// claim, and stoppedWorkerID acknowledged stopping it. Lease expiry alone is
// not that proof. A task past its deadline stays failed. The caller decides
// that the next attempt is safe, for example by resuming a checkpoint; the
// next Claim allocates its epoch. It reports whether the task was reopened.
func (s *Store) ReopenStoppedWorkflowTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, id string, claimEpoch int64, stoppedWorkerID string) (bool, error) {
	if tx == nil {
		return false, errors.New("transaction is required")
	}
	if workspaceID == "" || id == "" || stoppedWorkerID == "" || claimEpoch < 1 {
		return false, errors.New("workspace, task, stopped worker, and positive claim epoch are required")
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_task_queue
		SET status=$5,result=NULL,error=NULL,run_id=NULL,
			lease_expires_at=NULL,started_at=NULL,completed_at=NULL,updated_at=$6,available_at=$6
		WHERE workspace_id=$1 AND id=$2 AND kind='team_workflow' AND identity_kind=$7
			AND status=$8 AND error=$9 AND worker_id IS NULL
			AND claim_epoch=$3 AND stopped_epoch=claim_epoch AND stopped_worker_id=$4
			AND (deadline_at IS NULL OR deadline_at>$6)`,
		workspaceID, id, claimEpoch, stoppedWorkerID, StatusQueued, s.clock.Now(),
		IdentityTeamWorkflow, StatusFailed, ExecutionLeaseExpiredError)
	if err != nil {
		return false, fmt.Errorf("reopen stopped workflow task: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
