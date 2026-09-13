package capabilities

import (
	"context"
	"errors"
	"fmt"
)

var ErrActivationIdentityMissing = errors.New("capability activation identity missing")

// BindRuntime records the selected host only while this exact claim is active.
// A stale worker must not overwrite a newer attempt's runtime attribution.
func (s *PGStore) BindRuntime(ctx context.Context, task InvocationTask, runtimeID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE weave_capability_invocations i SET runtime_id=$5
 FROM weave_task_queue t
 WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3
 AND t.workspace_id=i.workspace_id AND t.capability_invocation_id=i.invocation_id AND t.id=i.task_id
 AND t.worker_id=$4 AND t.claim_epoch=$6 AND i.status='running' AND t.status='running'
 AND t.lease_expires_at>now() AND (t.deadline_at IS NULL OR t.deadline_at>now())`,
		task.WorkspaceID, task.InvocationID, task.TaskID, task.WorkerID, runtimeID, task.ClaimEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *PGStore) RecordStepRun(ctx context.Context, task InvocationTask, stepID, runID string) error {
	activationID := fmt.Sprintf("%s/%d", task.TaskID, task.ClaimEpoch)
	if task.ClaimEpoch < 1 || runID == "" {
		return ErrActivationIdentityMissing
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO weave_capability_step_runs(workspace_id,invocation_id,step_id,activation_id,run_id)
	SELECT i.workspace_id,i.invocation_id,$5,$6,$7
	FROM weave_capability_invocations i JOIN weave_task_queue t
	ON t.workspace_id=i.workspace_id AND t.capability_invocation_id=i.invocation_id AND t.id=i.task_id
	WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3
	AND i.status='running' AND t.status='running' AND t.worker_id=$4 AND t.claim_epoch=$8
	AND t.lease_expires_at>now() AND (t.deadline_at IS NULL OR t.deadline_at>now())
	ON CONFLICT DO NOTHING`, task.WorkspaceID, task.InvocationID, task.TaskID, task.WorkerID, stepID, activationID, runID, task.ClaimEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Exact receipt replay is idempotent. A run attached to another activation,
	// or a new receipt from a stale claim, remains rejected.
	var exact bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_step_runs
	 WHERE workspace_id=$1 AND invocation_id=$2 AND step_id=$3 AND activation_id=$4 AND run_id=$5)`,
		task.WorkspaceID, task.InvocationID, stepID, activationID, runID).Scan(&exact); err != nil {
		return err
	}
	if exact {
		return nil
	}
	return ErrClaimLost
}
