package capabilities

import "context"

func (s *PGStore) TaskActive(ctx context.Context, task InvocationTask) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM weave_capability_invocation_tasks t JOIN weave_capability_invocations i ON i.workspace_id=t.workspace_id AND i.invocation_id=t.invocation_id
 WHERE t.task_id=$1 AND t.workspace_id=$2 AND t.invocation_id=$3 AND t.claim_token=$4 AND t.status='running' AND i.status='running' AND t.deadline_at>now())`,
		task.TaskID, task.WorkspaceID, task.InvocationID, task.ClaimToken).Scan(&active)
	return active, err
}

// Expired executions fail closed; they are never blindly retried after a crash.
func (s *PGStore) expireTasks(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT i.workspace_id,i.invocation_id,t.task_id,i.status
 FROM weave_capability_invocations i JOIN weave_capability_invocation_tasks t ON t.workspace_id=i.workspace_id AND t.invocation_id=i.invocation_id
 WHERE i.status IN ('running','cancel_requested') AND t.status='running' AND (t.deadline_at IS NULL OR t.deadline_at<=now())
 ORDER BY i.workspace_id,i.invocation_id FOR UPDATE OF i SKIP LOCKED`)
	if err != nil {
		return err
	}
	type expired struct{ workspace, id, task, status string }
	items := []expired{}
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.workspace, &item.id, &item.task, &item.status); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		status := "failed"
		if item.status == "cancel_requested" {
			status = "cancelled"
		}
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$3,result_state='unavailable',result=NULL,error='execution_deadline_expired' WHERE workspace_id=$1 AND invocation_id=$2`, item.workspace, item.id, status); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status=$2 WHERE task_id=$1`, item.task, status); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
