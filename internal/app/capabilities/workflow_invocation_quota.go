package capabilities

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A workspace admits at most max_active_invocations capability invocations in
// flight, and an application with an admission limit at most its own
// max_active_invocations of them. Each in-flight invocation holds one row of
// weave_capability_quota_slots in the workspace's scope (scope_application_id
// '') and, when its application is limited, one in the application's scope.
// An admission takes a free slot with FOR UPDATE SKIP LOCKED inside its own
// transaction, so admissions that find a free slot never wait for each other,
// and a slot is only ever held by one committed invocation. Slots are released
// lazily: an admission that finds no free slot rolls back, reclaims every slot
// whose holder is no longer in flight, by the same live-owner definition the
// count used, and tries once more. In-flight work is never evicted.

const (
	slotHolderWorkflow = "workflow"
	slotHolderInline   = "inline"
)

// errQuotaSlotsFull and errApplicationQuotaSlotsFull ask the admission's
// caller to refresh the slots and retry; after the retry they are reported as
// ErrQuotaExceeded and ErrApplicationQuotaExceeded.
var (
	errQuotaSlotsFull            = errors.New("capability quota slots are full")
	errApplicationQuotaSlotsFull = errors.New("capability application quota slots are full")
)

// slotHolder names the invocation a slot is held for: workflow holders by
// invocation_id, inline holders by request_id.
type slotHolder struct {
	Kind, ApplicationID, ID string
}

// quotaScope is a set of slots: the workspace's, or, with ApplicationID, one
// application's.
type quotaScope struct {
	WorkspaceID, ApplicationID string
}

func (q quotaScope) full() error {
	if q.ApplicationID != "" {
		return errApplicationQuotaSlotsFull
	}
	return errQuotaSlotsFull
}

// acquireCapabilityQuotaSlot takes the slots holder needs, whose invocation row
// this transaction has already inserted: its application's first, when that
// application is limited, then the workspace's. It returns the scope's full
// error when one has no free slot.
func acquireCapabilityQuotaSlot(ctx context.Context, tx pgx.Tx, workspaceID string, holder slotHolder) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace is required for capability quota")
	}
	limited, err := applicationAdmissionLimited(ctx, tx, workspaceID, holder.ApplicationID)
	if err != nil {
		return err
	}
	if limited {
		if err := acquireQuotaSlot(ctx, tx, quotaScope{workspaceID, holder.ApplicationID}, holder); err != nil {
			return err
		}
	}
	return acquireQuotaSlot(ctx, tx, quotaScope{WorkspaceID: workspaceID}, holder)
}

func applicationAdmissionLimited(ctx context.Context, tx pgx.Tx, workspaceID, applicationID string) (bool, error) {
	if applicationID == "" {
		return false, nil
	}
	var limited bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_application_quotas
	 WHERE workspace_id=$1 AND application_id=$2)`, workspaceID, applicationID).Scan(&limited); err != nil {
		return false, fmt.Errorf("read application admission limit: %w", err)
	}
	return limited, nil
}

func acquireQuotaSlot(ctx context.Context, tx pgx.Tx, scope quotaScope, holder slotHolder) error {
	var retiredHeld bool
	err := tx.QueryRow(ctx, `UPDATE weave_capability_quota_slots
	 SET holder_kind=$3,holder_application_id=$4,holder_id=$5,held_at=now()
	 WHERE workspace_id=$1 AND scope_application_id=$2 AND slot=(SELECT slot FROM weave_capability_quota_slots
	  WHERE workspace_id=$1 AND scope_application_id=$2 AND holder_kind IS NULL AND NOT retired
	  ORDER BY slot LIMIT 1 FOR UPDATE SKIP LOCKED)
	 RETURNING EXISTS(SELECT 1 FROM weave_capability_quota_slots
	  WHERE workspace_id=$1 AND scope_application_id=$2 AND retired AND holder_kind IS NOT NULL)`,
		scope.WorkspaceID, scope.ApplicationID, holder.Kind, holder.ApplicationID, holder.ID).Scan(&retiredHeld)
	if errors.Is(err, pgx.ErrNoRows) {
		return scope.full()
	}
	if err != nil {
		return fmt.Errorf("take capability quota slot: %w", err)
	}
	if !retiredHeld {
		return nil
	}
	// A lowered limit still has work in flight above it, so a free slot below
	// the limit does not prove room. Until that work ends, admissions take the
	// scope's lock and count every held slot, this one included.
	limit, _, exists, err := lockQuotaScope(ctx, tx, scope)
	if err != nil || !exists {
		return err
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_capability_quota_slots
	 WHERE workspace_id=$1 AND scope_application_id=$2 AND holder_kind IS NOT NULL`, scope.WorkspaceID, scope.ApplicationID).Scan(&held); err != nil {
		return fmt.Errorf("count held capability quota slots: %w", err)
	}
	if held > limit {
		return scope.full()
	}
	return nil
}

// refreshQuotaSlots creates the slots of the workspace and, when it is
// limited, of the application on first use, and frees the slots of ended
// work, each scope in a transaction of its own.
func refreshQuotaSlots(ctx context.Context, pool *pgxpool.Pool, workspaceID, applicationID string) error {
	scopes := []quotaScope{{WorkspaceID: workspaceID}}
	if applicationID != "" {
		scopes = append(scopes, quotaScope{workspaceID, applicationID})
	}
	for _, scope := range scopes {
		if err := refreshQuotaScope(ctx, pool, scope); err != nil {
			return err
		}
	}
	return nil
}

func refreshQuotaScope(ctx context.Context, pool *pgxpool.Pool, scope quotaScope) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin capability quota slot refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ensureQuotaSlots(ctx, tx, scope); err != nil {
		return err
	}
	if err := reclaimQuotaSlots(ctx, tx, scope); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reclaimQuotaSlots frees every slot of scope whose holder is no longer in
// flight and removes freed retired slots. Reclaims of a scope take turns, so a
// refusal that follows has seen every release in progress. The scan locks its
// candidates in slot order, waiting for slots an admission holds; the release
// then rechecks the candidates in a new statement, so a holder that committed
// after the scan began is seen and keeps its slot.
func reclaimQuotaSlots(ctx context.Context, tx pgx.Tx, scope quotaScope) error {
	// Reclaims of one scope run one after another. A reclaim that waited for
	// another's release and then found those slots already free would still
	// keep their row locks until it committed, and admissions retrying with
	// SKIP LOCKED would then see no free slot and be refused. Taken in turn, a
	// reclaim starts after the previous one committed and never locks a slot
	// it does not release. Only admissions that found the scope full get here.
	lock := invocationLockKey("capability-quota-reclaim", scope.WorkspaceID, scope.ApplicationID, "")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return fmt.Errorf("serialize capability quota slot reclaim: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT s.slot FROM weave_capability_quota_slots s
	 WHERE s.workspace_id=$1 AND s.scope_application_id=$2 AND s.holder_kind IS NOT NULL AND NOT (`+slotHolderLiveSQL+`)
	 ORDER BY s.slot FOR UPDATE`, scope.WorkspaceID, scope.ApplicationID)
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
		 WHERE s.workspace_id=$1 AND s.scope_application_id=$2 AND s.slot=ANY($3) AND NOT (`+slotHolderLiveSQL+`)`,
			scope.WorkspaceID, scope.ApplicationID, slots); err != nil {
			return fmt.Errorf("release capability quota slots: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_quota_slots WHERE workspace_id=$1 AND scope_application_id=$2 AND slot IN (
	  SELECT slot FROM weave_capability_quota_slots WHERE workspace_id=$1 AND scope_application_id=$2 AND retired AND holder_kind IS NULL
	  FOR UPDATE SKIP LOCKED)`, scope.WorkspaceID, scope.ApplicationID); err != nil {
		return fmt.Errorf("remove retired capability quota slots: %w", err)
	}
	return nil
}

// ensureQuotaSlots creates the scope's slots on first use. Initialization runs
// once per scope under the scope's lock and gives every invocation already in
// flight in it its own slot; holders beyond the limit get retired slots, which
// are counted until they end. An application without a limit has no slots.
func ensureQuotaSlots(ctx context.Context, tx pgx.Tx, scope quotaScope) error {
	var ready bool
	readyQuery := `SELECT slots_ready FROM weave_capability_quotas WHERE workspace_id=$1 AND $2::text=''`
	if scope.ApplicationID != "" {
		readyQuery = `SELECT slots_ready FROM weave_capability_application_quotas WHERE workspace_id=$1 AND application_id=$2`
	}
	err := tx.QueryRow(ctx, readyQuery, scope.WorkspaceID, scope.ApplicationID).Scan(&ready)
	if err == nil && ready {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read capability quota slots: %w", err)
	}
	limit, ready, exists, err := lockQuotaScope(ctx, tx, scope)
	if err != nil || ready || !exists {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quota_slots(workspace_id,scope_application_id,slot)
	 SELECT $1,$2,n FROM generate_series(0,$3::int-1) AS n ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.ApplicationID, limit); err != nil {
		return fmt.Errorf("create capability quota slots: %w", err)
	}
	// The scope's live work: the workspace's, or the application's alone.
	filter := `($2::text='' OR i.application_id=$2)`
	if _, err := tx.Exec(ctx, `WITH live AS (
	   SELECT 'inline' AS kind,i.application_id,i.request_id AS id FROM (`+liveInlineInvocationsSQL+`) i WHERE `+filter+`
	   UNION ALL
	   SELECT 'workflow',c.application_id,c.invocation_id FROM (`+liveWorkflowInvocationsSQL(filter)+`) c
	    WHERE `+notKernelClosed+`),
	  numbered AS (SELECT kind,application_id,id,(row_number() OVER (ORDER BY kind,application_id,id))::int-1 AS n FROM live)
	 INSERT INTO weave_capability_quota_slots(workspace_id,scope_application_id,slot,retired,holder_kind,holder_application_id,holder_id,held_at)
	 SELECT $1,$2,n,n>=$3::int,kind,application_id,id,now() FROM numbered
	 ON CONFLICT (workspace_id,scope_application_id,slot) DO UPDATE SET retired=EXCLUDED.retired,holder_kind=EXCLUDED.holder_kind,
	  holder_application_id=EXCLUDED.holder_application_id,holder_id=EXCLUDED.holder_id,held_at=EXCLUDED.held_at`,
		scope.WorkspaceID, scope.ApplicationID, limit); err != nil {
		return fmt.Errorf("assign in-flight capability invocations to quota slots: %w", err)
	}
	markReady := `UPDATE weave_capability_quotas SET slots_ready=true WHERE workspace_id=$1 AND $2::text=''`
	if scope.ApplicationID != "" {
		markReady = `UPDATE weave_capability_application_quotas SET slots_ready=true WHERE workspace_id=$1 AND application_id=$2`
	}
	if _, err := tx.Exec(ctx, markReady, scope.WorkspaceID, scope.ApplicationID); err != nil {
		return fmt.Errorf("mark capability quota slots ready: %w", err)
	}
	return nil
}

// lockQuotaScope serializes the scope's slot initialization, limit changes,
// which create, retire and remove slots, and admissions while a lowered limit
// still has work in flight above it. exists is false for an application whose
// limit was removed.
func lockQuotaScope(ctx context.Context, tx pgx.Tx, scope quotaScope) (limit int, ready, exists bool, err error) {
	if scope.ApplicationID == "" {
		quota, ready, err := lockWorkspaceCapabilityQuota(ctx, tx, scope.WorkspaceID)
		return quota.MaxActiveInvocations, ready, err == nil, err
	}
	lock := invocationLockKey("capability-quota", scope.WorkspaceID, scope.ApplicationID, "active")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return 0, false, false, fmt.Errorf("lock application capability quota: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT max_active_invocations,slots_ready FROM weave_capability_application_quotas
	 WHERE workspace_id=$1 AND application_id=$2 FOR UPDATE`, scope.WorkspaceID, scope.ApplicationID).Scan(&limit, &ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("read application capability quota: %w", err)
	}
	return limit, ready, true, nil
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

// resizeQuotaSlots makes slots 0..max-1 of scope the admissible ones after a
// limit change on initialized slots: a raise adds slots and reopens retired
// ones below the new limit; a cut retires the slots above it, removing free
// ones at once and held ones when their holders end.
func resizeQuotaSlots(ctx context.Context, tx pgx.Tx, scope quotaScope, max int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_quota_slots(workspace_id,scope_application_id,slot)
	 SELECT $1,$2,n FROM generate_series(0,$3::int-1) AS n ON CONFLICT DO NOTHING`, scope.WorkspaceID, scope.ApplicationID, max); err != nil {
		return fmt.Errorf("add capability quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_quota_slots SET retired=(slot>=$3::int)
	 WHERE workspace_id=$1 AND scope_application_id=$2 AND retired<>(slot>=$3::int)`, scope.WorkspaceID, scope.ApplicationID, max); err != nil {
		return fmt.Errorf("retire capability quota slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_quota_slots
	 WHERE workspace_id=$1 AND scope_application_id=$2 AND retired AND holder_kind IS NULL`, scope.WorkspaceID, scope.ApplicationID); err != nil {
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
