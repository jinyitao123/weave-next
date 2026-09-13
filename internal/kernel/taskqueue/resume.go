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
			AND status IN ($7,$8) AND worker_id IS NULL
		RETURNING `+taskColumns, workspaceID, id, kind, contextKey,
		StatusQueued, s.clock.Now(), StatusCompleted, StatusFailed))
	if err != nil {
		return nil, fmt.Errorf("task has not settled or cannot be resumed: %w", err)
	}
	return task, nil
}
