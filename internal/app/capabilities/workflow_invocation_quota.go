package capabilities

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A workspace admits at most max_active_invocations capability invocations in
// flight. Each in-flight invocation holds one row of
// weave_capability_quota_slots; an admission takes a free slot with
// FOR UPDATE SKIP LOCKED inside its own transaction, so admissions that find a
// free slot never wait for each other, and a slot is only ever held by one
// committed invocation. Slots are released lazily: an admission that finds no
// free slot rolls back, reclaims every slot whose holder is no longer in
// flight, by the same live-owner definition the count used, and tries once
// more. In-flight work is never evicted.

const (
	slotHolderWorkflow = "workflow"
	slotHolderInline   = "inline"
)

// errQuotaSlotsFull asks the admission's caller to refresh the workspace's
// slots and retry; after the retry it is reported as ErrQuotaExceeded.
var errQuotaSlotsFull = errors.New("capability quota slots are full")

// slotHolder names the invocation a slot is held for: workflow holders by
// invocation_id, inline holders by request_id.
type slotHolder struct {
	Kind, ApplicationID, ID string
}

// acquireCapabilityQuotaSlot takes a slot for holder, whose invocation row this
// transaction has already inserted, or returns errQuotaSlotsFull.
func acquireCapabilityQuotaSlot(ctx context.Context, tx pgx.Tx, workspaceID string, holder slotHolder) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace is required for capability quota")
	}
	var retiredHeld bool
	err := tx.QueryRow(ctx, `UPDATE weave_capability_quota_slots
	 SET holder_kind=$2,holder_application_id=$3,holder_id=$4,held_at=now()
	 WHERE workspace_id=$1 AND slot=(SELECT slot FROM weave_capability_quota_slots
	  WHERE workspace_id=$1 AND holder_kind IS NULL AND NOT retired
	  ORDER BY slot LIMIT 1 FOR UPDATE SKIP LOCKED)
	 RETURNING EXISTS(SELECT 1 FROM weave_capability_quota_slots
	  WHERE workspace_id=$1 AND retired AND holder_kind IS NOT NULL)`,
		workspaceID, holder.Kind, holder.ApplicationID, holder.ID).Scan(&retiredHeld)
	if errors.Is(err, pgx.ErrNoRows) {
		return errQuotaSlotsFull
	}
	if err != nil {
		return fmt.Errorf("take capability quota slot: %w", err)
	}
	if !retiredHeld {
		return nil
	}
	// A lowered quota still has work in flight above it, so a free slot below
	// the limit does not prove room. Until that work ends, admissions take the
	// workspace lock and count every held slot, this one included.
	quota, _, err := lockWorkspaceCapabilityQuota(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_capability_quota_slots
	 WHERE workspace_id=$1 AND holder_kind IS NOT NULL`, workspaceID).Scan(&held); err != nil {
		return fmt.Errorf("count held capability quota slots: %w", err)
	}
	if held > quota.MaxActiveInvocations {
		return errQuotaSlotsFull
	}
	return nil
}

// refreshQuotaSlots creates the workspace's slots on first use and frees the
// slots of ended work, in a transaction of its own.
func refreshQuotaSlots(ctx context.Context, pool *pgxpool.Pool, workspaceID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin capability quota slot refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ensureQuotaSlots(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := reclaimQuotaSlots(ctx, tx, workspaceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reclaimQuotaSlots frees every slot whose holder is no longer in flight and
// removes freed retired slots. The scan locks its candidates in slot order,
// waiting for slots another admission or reclaim holds, so a refusal that
// follows has seen every release in progress; the release then rechecks the
// candidates in a new statement, so a holder that committed after the scan
// began is seen and keeps its slot.
func reclaimQuotaSlots(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	rows, err := tx.Query(ctx, `SELECT s.slot FROM weave_capability_quota_slots s
	 WHERE s.workspace_id=$1 AND s.holder_kind IS NOT NULL AND NOT (`+slotHolderLiveSQL+`)
	 ORDER BY s.slot FOR UPDATE`, workspaceID)
	if err != nil {
		return fmt.Errorf("scan capability quota slots: %w", err)
	}
	slots, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return fmt.Errorf("scan capability quota slots: %w", err)
	}
	if len(slots) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_quota_slots s
		 SET holder_kind=NULL,holder_application_id=NULL,holder_id=NULL,held_at=NULL
		 WHERE s.workspace_id=$1 AND s.slot=ANY($2) AND NOT (`+slotHolderLiveSQL+`)`, workspaceID, slots); err != nil {
			return fmt.Errorf("release capability quota slots: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_quota_slots WHERE workspace_id=$1 AND slot IN (
	  SELECT slot FROM weave_capability_quota_slots WHERE workspace_id=$1 AND retired AND holder_kind IS NULL
	  FOR UPDATE SKIP LOCKED)`, workspaceID); err != nil {
		return fmt.Errorf("remove retired capability quota slots: %w", err)
	}
	return nil
}

// ensureQuotaSlots creates the workspace's slots on first use. Initialization
// runs once per workspace under the workspace quota lock and gives every
// invocation already in flight its own slot; holders beyond the quota get
// retired slots, which are counted until they end.
func ensureQuotaSlots(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var ready bool
	err := tx.QueryRow(ctx, `SELECT slots_ready FROM weave_capability_quotas WHERE workspace_id=$1`, workspaceID).Scan(&ready)
	if err == nil && ready {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read capability quota slots: %w", err)
	}
	quota, ready, err := lockWorkspaceCapabilityQuota(ctx, tx, workspaceID)
	if err != nil || ready {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quota_slots(workspace_id,slot)
	 SELECT $1,n FROM generate_series(0,$2::int-1) AS n ON CONFLICT DO NOTHING`, workspaceID, quota.MaxActiveInvocations); err != nil {
		return fmt.Errorf("create capability quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `WITH live AS (
	   SELECT 'inline' AS kind,i.application_id,i.request_id AS id FROM (`+liveInlineInvocationsSQL+`) i
	   UNION ALL
	   SELECT 'workflow',c.application_id,c.invocation_id FROM (`+liveWorkflowInvocationsSQL(`true`)+`) c
	    WHERE `+notKernelClosed+`),
	  numbered AS (SELECT kind,application_id,id,(row_number() OVER (ORDER BY kind,application_id,id))::int-1 AS n FROM live)
	 INSERT INTO weave_capability_quota_slots(workspace_id,slot,retired,holder_kind,holder_application_id,holder_id,held_at)
	 SELECT $1,n,n>=$2::int,kind,application_id,id,now() FROM numbered
	 ON CONFLICT (workspace_id,slot) DO UPDATE SET retired=EXCLUDED.retired,holder_kind=EXCLUDED.holder_kind,
	  holder_application_id=EXCLUDED.holder_application_id,holder_id=EXCLUDED.holder_id,held_at=EXCLUDED.held_at`,
		workspaceID, quota.MaxActiveInvocations); err != nil {
		return fmt.Errorf("assign in-flight capability invocations to quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_quotas SET slots_ready=true WHERE workspace_id=$1`, workspaceID); err != nil {
		return fmt.Errorf("mark capability quota slots ready: %w", err)
	}
	return nil
}

// lockWorkspaceCapabilityQuota serializes the workspace's slot initialization,
// quota changes, which create, retire and remove slots, and admissions while a
// lowered quota still has work in flight above it.
func lockWorkspaceCapabilityQuota(ctx context.Context, tx pgx.Tx, workspaceID string) (Quota, bool, error) {
	lock := invocationLockKey("capability-quota", workspaceID, "", "active")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return Quota{}, false, fmt.Errorf("lock workspace capability quota: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quotas(workspace_id) VALUES($1) ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return Quota{}, false, fmt.Errorf("ensure workspace capability quota: %w", err)
	}
	var quota Quota
	var ready bool
	err := tx.QueryRow(ctx, `SELECT max_steps_per_invocation,max_active_invocations,slots_ready
	 FROM weave_capability_quotas WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(
		&quota.MaxStepsPerInvocation, &quota.MaxActiveInvocations, &ready)
	if err != nil {
		return Quota{}, false, fmt.Errorf("read workspace capability quota: %w", err)
	}
	return quota, ready, nil
}

// resizeQuotaSlots makes slots 0..max-1 the admissible ones after a quota
// change on initialized slots: a raise adds slots and reopens retired ones
// below the new limit; a cut retires the slots above it, removing free ones
// at once and held ones when their holders end.
func resizeQuotaSlots(ctx context.Context, tx pgx.Tx, workspaceID string, max int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quota_slots(workspace_id,slot)
	 SELECT $1,n FROM generate_series(0,$2::int-1) AS n ON CONFLICT DO NOTHING`, workspaceID, max); err != nil {
		return fmt.Errorf("add capability quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_quota_slots SET retired=(slot>=$2::int)
	 WHERE workspace_id=$1 AND retired<>(slot>=$2::int)`, workspaceID, max); err != nil {
		return fmt.Errorf("retire capability quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_quota_slots
	 WHERE workspace_id=$1 AND retired AND holder_kind IS NULL`, workspaceID); err != nil {
		return fmt.Errorf("remove retired capability quota slots: %w", err)
	}
	return nil
}

// slotHolderLiveSQL reports whether the holder of slot row s is in flight. A
// workflow holder is in flight exactly when liveWorkflowInvocationsSQL selects
// it and its admission is not kernel-closed; an inline holder exactly when
// liveInlineInvocationsSQL selects it.
const slotHolderLiveSQL = `CASE s.holder_kind
	 WHEN 'workflow' THEN EXISTS (SELECT 1 FROM weave_capability_workflow_invocations i
	  WHERE i.workspace_id=s.workspace_id AND i.application_id=s.holder_application_id AND i.invocation_id=s.holder_id
	    AND NOT EXISTS (SELECT 1 FROM weave_kernel_publication_requests k
	      WHERE k.workspace_id=i.workspace_id AND k.request_id=i.admission_request_id AND k.operation='published_closed')
	    AND (EXISTS (SELECT 1 FROM weave_team_runs r WHERE r.workspace_id=i.workspace_id AND r.run_id=i.run_id
	          AND r.status IN ('queued','running','parked','cancel_requested'))
	      OR (NOT EXISTS (SELECT 1 FROM weave_team_runs r WHERE r.workspace_id=i.workspace_id AND r.run_id=i.run_id)
	        AND (EXISTS (SELECT 1 FROM weave_task_queue q WHERE q.workspace_id=i.workspace_id AND q.id=i.task_id
	              AND q.status IN ('queued','dispatched','running','cancel_requested'))
	          OR (NOT EXISTS (SELECT 1 FROM weave_task_queue q WHERE q.workspace_id=i.workspace_id AND q.id=i.task_id)
	            AND EXISTS (SELECT 1 FROM weave_workflow_admission_requests a
	              WHERE a.workspace_id=i.workspace_id AND a.request_id=i.admission_request_id AND a.receipt IS NULL))))))
	 ELSE EXISTS (SELECT 1 FROM weave_capability_invocations i
	  JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id AND q.capability_invocation_id=i.invocation_id
	  WHERE i.workspace_id=s.workspace_id AND i.application_id=s.holder_application_id AND i.request_id=s.holder_id
	    AND ((q.identity_kind='capability' AND (q.status IN ('queued','running','cancel_requested')
	          OR (q.status='failed' AND q.worker_id IS NOT NULL)))
	      OR (i.status='waiting' AND q.status='completed')))
	 END`

// liveInlineInvocationsSQL selects the inline invocations of workspace $1 in
// flight: their capability task is queued, running or cancel_requested, has
// failed while still owned by a worker, or has completed while the invocation
// waits. Only capability tasks carry a capability_invocation_id
// (weave_task_queue_identity_shape_check), so the identity_kind filter changes
// no result; it lets the first part read live capability tasks alone.
const liveInlineInvocationsSQL = `SELECT i.* FROM weave_task_queue q
	  JOIN weave_capability_invocations i ON i.workspace_id=q.workspace_id AND i.task_id=q.id
	    AND i.invocation_id=q.capability_invocation_id
	  WHERE q.workspace_id=$1 AND q.identity_kind='capability'
	    AND (q.status IN ('queued','running','cancel_requested') OR (q.status='failed' AND q.worker_id IS NOT NULL))
	 UNION ALL
	 SELECT i.* FROM weave_capability_invocations i
	  JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
	    AND q.capability_invocation_id=i.invocation_id
	  WHERE i.workspace_id=$1 AND i.status='waiting' AND q.status='completed'`

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
