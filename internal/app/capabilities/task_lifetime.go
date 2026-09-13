package capabilities

import "context"

func (s *PGStore) TaskActive(ctx context.Context, task InvocationTask) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM weave_capability_invocation_tasks t JOIN weave_capability_invocations i ON i.workspace_id=t.workspace_id AND i.invocation_id=t.invocation_id
 WHERE t.task_id=$1 AND t.workspace_id=$2 AND t.invocation_id=$3 AND t.claim_token=$4 AND t.status='running' AND i.status='running' AND t.deadline_at>now()
 AND t.claim_started_at IS NOT NULL
 AND t.execution_consumed_ms+GREATEST(0,EXTRACT(EPOCH FROM (now()-t.claim_started_at))*1000)<t.execution_budget_ms
 AND (i.caller_kind='developer' OR EXISTS(SELECT 1 FROM weave_capability_apps a JOIN weave_capability_grants g ON g.workspace_id=a.workspace_id AND g.app_id=a.id
 WHERE a.workspace_id=i.workspace_id AND a.id=i.application_id AND a.enabled AND g.enabled AND g.capability_id=i.capability_id AND g.revision=i.revision)))`,
		task.TaskID, task.WorkspaceID, task.InvocationID, task.ClaimToken).Scan(&active)
	return active, err
}

func (s *PGStore) RenewTask(ctx context.Context, task InvocationTask) error {
	// Renewal extends a live claim; it cannot resurrect an expired lease or
	// grant more time after cancellation or application authorization revocation.
	tag, err := s.pool.Exec(ctx, `UPDATE weave_capability_invocation_tasks t
	 SET deadline_at=LEAST(now()+interval '150 seconds',t.claim_started_at+((t.execution_budget_ms-t.execution_consumed_ms)*interval '1 millisecond'))
 FROM weave_capability_invocations i
 WHERE t.task_id=$1 AND t.workspace_id=$2 AND t.invocation_id=$3 AND t.claim_token=$4
 AND i.workspace_id=t.workspace_id AND i.invocation_id=t.invocation_id AND i.task_id=t.task_id
 AND t.status='running' AND i.status='running' AND t.deadline_at>now() AND t.claim_started_at IS NOT NULL
 AND t.execution_consumed_ms+GREATEST(0,EXTRACT(EPOCH FROM (now()-t.claim_started_at))*1000)<t.execution_budget_ms
 AND (i.caller_kind='developer' OR EXISTS(SELECT 1 FROM weave_capability_apps a JOIN weave_capability_grants g ON g.workspace_id=a.workspace_id AND g.app_id=a.id
 WHERE a.workspace_id=i.workspace_id AND a.id=i.application_id AND a.enabled AND g.enabled AND g.capability_id=i.capability_id AND g.revision=i.revision))`, task.TaskID, task.WorkspaceID, task.InvocationID, task.ClaimToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s *PGStore) AbandonTask(ctx context.Context, task InvocationTask) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='queued',claim_token=NULL,deadline_at=NULL WHERE task_id=$1 AND workspace_id=$2 AND invocation_id=$3 AND claim_token=$4 AND status='running'`, task.TaskID, task.WorkspaceID, task.InvocationID, task.ClaimToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='queued',error=NULL WHERE workspace_id=$1 AND invocation_id=$2 AND status='running'`, task.WorkspaceID, task.InvocationID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_invocation_events(workspace_id,invocation_id,event_type,detail) VALUES($1,$2,'execution_interrupted','{}')`, task.WorkspaceID, task.InvocationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A lost lease does not prove the previous attempt stopped before its external
// effects. Expiration must not automatically dispatch that work a second time.
// Confirmed human waits and cooperative shutdown keep their existing resume paths.
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
		errorText := "capability_execution_lease_expired"
		if item.status == "cancel_requested" {
			status, errorText = "cancelled", "cancelled"
		}
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$3,result_state='unavailable',result=NULL,error=NULLIF($4,''),completed_at=now() WHERE workspace_id=$1 AND invocation_id=$2`, item.workspace, item.id, status, errorText); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status=$2,claim_token=NULL,deadline_at=NULL WHERE task_id=$1`, item.task, status); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
