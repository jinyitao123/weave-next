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

// liveWorkflowInvocationsSQL selects the published-workflow invocations of
// workspace $1 that are in flight, driven from live owners so the cost follows
// in-flight work rather than history. It is exactly the set where the
// invocation is not kernel-closed and either its TeamRun is queued, running,
// parked or cancel_requested; or it has no TeamRun and its task is queued,
// dispatched, running or cancel_requested; or it has neither and its admission
// has no receipt. The three branches are disjoint (TeamRun present; TeamRun
// absent with task; neither), and the admission foreign key guarantees the
// third branch finds every invocation it must. extra filters the invocation
// row (alias i) and may use later parameters.
func liveWorkflowInvocationsSQL(extra string) string {
	return `SELECT i.* FROM weave_team_runs r
	  JOIN weave_capability_workflow_invocations i ON i.workspace_id=r.workspace_id AND i.run_id=r.run_id
	  WHERE r.workspace_id=$1 AND r.status IN ('queued','running','parked','cancel_requested') AND ` + extra + `
	 UNION ALL
	 SELECT i.* FROM weave_task_queue q
	  JOIN weave_capability_workflow_invocations i ON i.workspace_id=q.workspace_id AND i.task_id=q.id
	  WHERE q.workspace_id=$1 AND q.status IN ('queued','dispatched','running','cancel_requested') AND ` + extra + `
	    AND NOT EXISTS (SELECT 1 FROM weave_team_runs r WHERE r.workspace_id=i.workspace_id AND r.run_id=i.run_id)
	 UNION ALL
	 SELECT i.* FROM weave_workflow_admission_requests a
	  JOIN weave_capability_workflow_invocations i ON i.workspace_id=a.workspace_id AND i.admission_request_id=a.request_id
	  WHERE a.workspace_id=$1 AND a.receipt IS NULL AND ` + extra + `
	    AND NOT EXISTS (SELECT 1 FROM weave_task_queue q WHERE q.workspace_id=i.workspace_id AND q.id=i.task_id)
	    AND NOT EXISTS (SELECT 1 FROM weave_team_runs r WHERE r.workspace_id=i.workspace_id AND r.run_id=i.run_id)`
}

// notKernelClosed keeps a live invocation whose admission the kernel has not
// closed; c is the live invocation row.
const notKernelClosed = `NOT EXISTS (SELECT 1 FROM weave_kernel_publication_requests k
	  WHERE k.workspace_id=c.workspace_id AND k.request_id=c.admission_request_id AND k.operation='published_closed')`

// countActiveCapabilityInvocations reads each execution owner once. Inline
// activity remains taskqueue-owned; published workflow activity is projected
// from its TeamRun/task association, excluding kernel-closed admissions. Each
// part starts from live rows (partial indexes in migration 0200), so the
// count does not grow with the workspace's history. Only capability tasks
// carry a capability_invocation_id (weave_task_queue_identity_shape_check),
// so the identity_kind filter changes no result; it lets the inline part read
// live capability tasks alone instead of every live workflow task.
func countActiveCapabilityInvocations(ctx context.Context, tx pgx.Tx, workspaceID, applicationID, requestID string) (int, error) {
	var active int
	err := tx.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM weave_task_queue q
	  JOIN weave_capability_invocations i ON i.workspace_id=q.workspace_id AND i.task_id=q.id
	    AND i.invocation_id=q.capability_invocation_id
	  WHERE q.workspace_id=$1 AND q.identity_kind='capability'
	    AND (q.status IN ('queued','running','cancel_requested') OR (q.status='failed' AND q.worker_id IS NOT NULL))
	    AND NOT (i.application_id=$2 AND i.request_id=$3))
	 +
	 (SELECT count(*) FROM weave_capability_invocations i
	  JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
	    AND q.capability_invocation_id=i.invocation_id
	  WHERE i.workspace_id=$1 AND i.status='waiting' AND q.status='completed'
	    AND NOT (i.application_id=$2 AND i.request_id=$3))
	 +
	 (SELECT count(*) FROM (`+liveWorkflowInvocationsSQL(`NOT (i.application_id=$2 AND i.request_id=$3)`)+`) c
	  WHERE `+notKernelClosed+`)`, workspaceID, applicationID, requestID).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("count active capability invocations: %w", err)
	}
	return active, nil
}

// workflowLaneActive reports whether another invocation of the application
// is in flight on lane, using the same live-owner definition as the quota.
func workflowLaneActive(ctx context.Context, tx pgx.Tx, workspaceID, applicationID, lane, invocationID string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (`+
		liveWorkflowInvocationsSQL(`i.application_id=$2 AND i.lane=$3 AND i.invocation_id<>$4`)+`) c
	  WHERE `+notKernelClosed+`)`, workspaceID, applicationID, lane, invocationID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active capability workflow lane: %w", err)
	}
	return active, nil
}
