package capabilities

import "context"

// BindRuntime records the selected host only while this exact claim is active.
// A stale worker must not overwrite a newer attempt's runtime attribution.
func (s *PGStore) BindRuntime(ctx context.Context, task InvocationTask, runtimeID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE weave_capability_invocations i SET runtime_id=$5
 FROM weave_capability_invocation_tasks t
 WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3
 AND t.workspace_id=i.workspace_id AND t.invocation_id=i.invocation_id AND t.task_id=i.task_id
 AND t.claim_token=$4 AND i.status='running' AND t.status='running' AND t.deadline_at>now()`,
		task.WorkspaceID, task.InvocationID, task.TaskID, task.ClaimToken, runtimeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *PGStore) RecordStepRun(ctx context.Context, task InvocationTask, stepID, runID string) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO weave_capability_step_runs(workspace_id,invocation_id,step_id,run_id)
 SELECT i.workspace_id,i.invocation_id,$5,$6
 FROM weave_capability_invocations i JOIN weave_capability_invocation_tasks t
 ON t.workspace_id=i.workspace_id AND t.invocation_id=i.invocation_id AND t.task_id=i.task_id
 WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3
 AND i.status='running' AND t.status='running' AND t.claim_token=$4 AND t.deadline_at>now()
 ON CONFLICT DO NOTHING`, task.WorkspaceID, task.InvocationID, task.TaskID, task.ClaimToken, stepID, runID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}
