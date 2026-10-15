package capabilities

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// lockWorkspaceCapabilityQuota serializes inline and workflow-backed capability
// admissions in one workspace. Both paths acquire this lock after their shared
// request/invocation identity locks and hold it through their admission commit.
func lockWorkspaceCapabilityQuota(ctx context.Context, tx pgx.Tx, workspaceID string) (Quota, error) {
	if workspaceID == "" {
		return Quota{}, fmt.Errorf("workspace is required for capability quota")
	}
	lock := invocationLockKey("capability-quota", workspaceID, "", "active")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return Quota{}, fmt.Errorf("lock workspace capability quota: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quotas(workspace_id) VALUES($1) ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return Quota{}, fmt.Errorf("ensure workspace capability quota: %w", err)
	}
	var quota Quota
	err := tx.QueryRow(ctx, `SELECT max_steps_per_invocation,max_active_invocations
	 FROM weave_capability_quotas WHERE workspace_id=$1 FOR SHARE`, workspaceID).Scan(
		&quota.MaxStepsPerInvocation, &quota.MaxActiveInvocations)
	if err != nil {
		return Quota{}, fmt.Errorf("read workspace capability quota: %w", err)
	}
	return quota, nil
}

// countActiveCapabilityInvocations reads each execution owner once. Inline
// activity remains taskqueue-owned; published workflow activity is projected
// from its TeamRun/task association, excluding kernel-closed admissions.
func countActiveCapabilityInvocations(ctx context.Context, tx pgx.Tx, workspaceID, applicationID, requestID string) (int, error) {
	var active int
	err := tx.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM weave_capability_invocations i
	  JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
	    AND q.capability_invocation_id=i.invocation_id
	  WHERE i.workspace_id=$1
	    AND (q.status IN ('queued','running','cancel_requested')
	      OR (q.status='completed' AND i.status='waiting')
	      OR (q.status='failed' AND q.worker_id IS NOT NULL))
	    AND NOT (i.application_id=$2 AND i.request_id=$3))
	 +
	 (SELECT count(*) FROM weave_capability_workflow_invocations i
	  LEFT JOIN weave_workflow_admission_requests a
	    ON a.workspace_id=i.workspace_id AND a.request_id=i.admission_request_id
	  LEFT JOIN weave_kernel_publication_requests k
	    ON k.workspace_id=i.workspace_id AND k.request_id=i.admission_request_id
	  LEFT JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.run_id
	  LEFT JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
	  WHERE i.workspace_id=$1
	    AND NOT (i.application_id=$2 AND i.request_id=$3)
	    AND k.operation IS DISTINCT FROM 'published_closed'
	    AND (
	      (a.receipt IS NULL AND q.id IS NULL AND r.run_id IS NULL)
	      OR r.status IN ('queued','running','parked','cancel_requested')
	      OR (r.run_id IS NULL AND q.status IN ('queued','dispatched','running','cancel_requested'))
	    ))`, workspaceID, applicationID, requestID).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("count active capability invocations: %w", err)
	}
	return active, nil
}
