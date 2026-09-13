package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// ErrRuntimeClaimUnavailable covers missing and closed runtime claim gates.
var ErrRuntimeClaimUnavailable = errors.New("runtime unavailable for claim")

// Clock supplies timestamps for queue state transitions.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// Store persists tasks and their leases.
type Store struct {
	pool     *pgxpool.Pool
	clock    Clock
	leaseTTL time.Duration
}

// New creates a task queue store.
func New(pool *pgxpool.Pool, clock Clock, leaseTTL time.Duration) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	if leaseTTL <= 0 {
		leaseTTL = time.Minute
	}
	return &Store{pool: pool, clock: clock, leaseTTL: leaseTTL}
}

func (s *Store) leaseTimes() (time.Time, time.Time) {
	now := s.clock.Now()
	return now, now.Add(s.leaseTTL)
}

// RecordDispatch inserts a completed single-agent dispatch for tracing.
func (s *Store) RecordDispatch(
	ctx context.Context,
	workspaceID, fromAgent, toAgent, message, result string,
	ok bool,
) error {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return err
	}
	actorSubject, _ := json.Marshal(subject)
	payload, err := json.Marshal(map[string]string{
		"from":    fromAgent,
		"to":      toAgent,
		"message": message,
	})
	if err != nil {
		return fmt.Errorf("marshal dispatch payload: %w", err)
	}
	resultJSON, err := json.Marshal(map[string]string{"output": result})
	if err != nil {
		return fmt.Errorf("marshal dispatch result: %w", err)
	}
	status := StatusFailed
	if ok {
		status = StatusCompleted
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record dispatch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_task_queue (
			id, workspace_id, agent, identity_kind, identity_schema_version,
			source, status, payload, result,
			created_at, started_at, completed_at, updated_at, actor_subject
		) VALUES (
			$1, $2, NULL, $3, 2,
			'dispatch', $4, $5, $6, $7, $7, $7, $7, $8::jsonb
		)
	`, "task-"+uuid.NewString(), workspaceID, IdentityAudit, status, payload, resultJSON, now, string(actorSubject)); err != nil {
		return fmt.Errorf("insert dispatch task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dispatch task: %w", err)
	}
	return nil
}

// Enqueue inserts a queued task in a workspace established by the product.
// Execution admission never creates or mutates the product directory.
func (s *Store) Enqueue(ctx context.Context, task *Task) error {
	if task == nil {
		return fmt.Errorf("task is required")
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin enqueue task: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.enqueueTx(ctx, tx, task, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit enqueue task: %w", err)
	}
	if task.Source == "" {
		task.Source = "chat"
	}
	if task.Kind == "" {
		task.Kind = "chat"
	}
	task.Status = StatusQueued
	task.AvailableAt = now
	task.CreatedAt = now
	task.UpdatedAt = now
	return nil
}

// EnqueueTx inserts a queued task into a caller-owned transaction.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, task *Task) error {
	if tx == nil {
		return fmt.Errorf("transaction is required")
	}
	if task == nil {
		return fmt.Errorf("task is required")
	}
	return s.enqueueTx(ctx, tx, task, s.clock.Now())
}

func (s *Store) enqueueTx(
	ctx context.Context,
	tx pgx.Tx,
	task *Task,
	now time.Time,
) error {
	if err := s.validateControlParent(ctx, tx, task); err != nil {
		return err
	}
	subject, err := s.admissionSubject(ctx, tx, task)
	if err != nil {
		return err
	}
	task.Subject = subject
	encodedSubject, _ := json.Marshal(subject)
	source := task.Source
	if source == "" {
		source = "chat"
	}
	kind := task.Kind
	if kind == "" {
		kind = "chat"
	}
	// Serialize remote work admission with parent stop. A late worker must not
	// enqueue a fresh process after cancellation has scanned the task tree.
	if kind == "engine_exec" && task.RunSnapshotID != "" {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM weave_team_runs
			WHERE workspace_id=$1 AND run_snapshot_id=$2 FOR UPDATE`, task.WorkspaceID, task.RunSnapshotID).Scan(&status)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check engine task parent: %w", err)
		}
		if err == nil && status != "queued" && status != "running" && status != "parked" {
			return errors.New("engine task parent has stopped")
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_task_queue (
				id, workspace_id, project_id, agent, agent_id, agent_version,
			identity_kind, identity_schema_version, execution_scope,
			workflow_id, workflow_version, run_snapshot_id,
				source, kind, runtime_id, runtime_assignment, status, priority,
			context_key, trace_id, parent_task_id, task_group_id,
			subtask_deadline_at, payload, created_at, updated_at, build_run_id, available_at, actor_subject, deadline_at, outcome_sensitive, capability_invocation_id, source_ref
		) VALUES (
				$1, $2, COALESCE(
					NULLIF($3, ''),
					(
						SELECT project_id FROM weave_task_group
						WHERE workspace_id=$2 AND id=NULLIF($22, '')
					),
					(
						SELECT project_id FROM weave_team_run_snapshots
						WHERE workspace_id=$2 AND run_id=NULLIF($12, '')
					)
				), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, 0),
				$7, $8, NULLIF($9, ''),
				NULLIF($10, ''), NULLIF($11, 0), NULLIF($12, ''),
				$13, $14, NULLIF($15, ''), $16::jsonb, $17, $18,
				$19, $20, $21, NULLIF($22, ''),
				$23, $24, $25, $25, NULLIF($26, ''), $25, $27::jsonb, $28, $29, NULLIF($30,''), $31
			)
		`, task.ID, task.WorkspaceID, task.ProjectID, task.Agent, task.AgentID, task.AgentVersion,
		task.IdentityKind, task.IdentitySchemaVersion, task.ExecutionScope,
		task.WorkflowID, task.WorkflowVersion, task.RunSnapshotID,
		source, kind, task.RuntimeID, task.RuntimeAssignment, StatusQueued, task.Priority,
		nullIfEmpty(task.ContextKey), nullIfEmpty(task.TraceID), nullIfEmpty(task.ParentTaskID),
		task.TaskGroupID, task.SubtaskDeadlineAt, task.Payload, now, task.BuildRunID, string(encodedSubject), task.DeadlineAt, task.OutcomeSensitive || kind == "engine_exec" || kind == "team_build", task.CapabilityInvocationID, task.SourceRef); err != nil {
		return fmt.Errorf("enqueue task: %w", err)
	}
	return nil
}

// Claim atomically takes the highest-priority queued task matching filter.
func (s *Store) Claim(ctx context.Context, workerID string, filter ClaimFilter) (*Task, error) {
	identityKind := filter.IdentityKind
	if identityKind == "" {
		identityKind = IdentityAgent
	}
	switch identityKind {
	case IdentityAgent, IdentityTeamWorkflow, IdentityTeamBuild, IdentityCapability:
	default:
		return nil, fmt.Errorf("unsupported claim identity kind %q", identityKind)
	}
	if filter.Kind == "engine_exec" {
		if filter.WorkspaceID == "" {
			return nil, fmt.Errorf("engine claim workspace is required")
		}
		if filter.RuntimeID == "" {
			return nil, fmt.Errorf("engine claim runtime is required")
		}
		return s.claimEngineTask(ctx, workerID, filter, identityKind)
	}

	now, leaseExpiresAt := s.leaseTimes()
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_task_queue
		SET status=$1, worker_id=$2, started_at=$3, lease_expires_at=$4, updated_at=$3, claim_epoch=claim_epoch+1, unreported_attempts=unreported_attempts+1
		WHERE id = (
			SELECT id FROM weave_task_queue
				WHERE status='queued' AND available_at <= $3
					AND kind <> 'engine_exec'
				AND kind=$5
				AND identity_kind=$6
					AND ($7='' OR workspace_id=$7)
					AND ($8='' OR run_snapshot_id=$8)
					AND (parent_task_id IS NULL OR EXISTS (
						SELECT 1 FROM weave_task_queue parent
						WHERE parent.workspace_id=weave_task_queue.workspace_id
						  AND parent.id=weave_task_queue.parent_task_id
						  AND parent.actor_subject=weave_task_queue.actor_subject
						  AND parent.status IN ('queued','dispatched','running')
					))
			ORDER BY priority DESC, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+taskColumns,
		StatusRunning, workerID, now, leaseExpiresAt,
		filter.Kind, identityKind, filter.WorkspaceID, filter.RunSnapshotID)
	task, err := scanTask(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("claim task: %w", err)
	}
	return task, nil
}

func (s *Store) claimEngineTask(
	ctx context.Context,
	workerID string,
	filter ClaimFilter,
	identityKind IdentityKind,
) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin engine claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runtimeID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2
		  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
		FOR UPDATE
	`, filter.WorkspaceID, filter.RuntimeID).Scan(&runtimeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: runtime %q",
			ErrRuntimeClaimUnavailable,
			filter.RuntimeID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("lock runtime claim gate: %w", err)
	}

	now, leaseExpiresAt := s.leaseTimes()
	row := tx.QueryRow(ctx, `
		UPDATE weave_task_queue
		SET status=$1, worker_id=$2, started_at=$3, lease_expires_at=$4, updated_at=$3, claim_epoch=claim_epoch+1, unreported_attempts=unreported_attempts+1
		WHERE id = (
			SELECT id FROM weave_task_queue
				WHERE status='queued' AND available_at <= $3
				AND kind='engine_exec'
				AND workspace_id=$5
					AND runtime_id=$6
					AND identity_kind=$7
					AND (parent_task_id IS NULL OR EXISTS (
						SELECT 1 FROM weave_task_queue parent
						WHERE parent.workspace_id=weave_task_queue.workspace_id
						  AND parent.id=weave_task_queue.parent_task_id
						  AND parent.actor_subject=weave_task_queue.actor_subject
						  AND parent.status IN ('queued','dispatched','running')
					))
			ORDER BY priority DESC, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+taskColumns,
		StatusRunning, workerID, now, leaseExpiresAt,
		filter.WorkspaceID, filter.RuntimeID, identityKind)
	task, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim engine task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit engine claim: %w", err)
	}
	return task, nil
}

// AwaitTerminal polls a workspace task until it reaches a terminal status or times out.
func (s *Store) AwaitTerminal(
	ctx context.Context,
	workspaceID, taskID string,
	timeout time.Duration,
) (*Task, error) {
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	pollTicker := time.NewTicker(2 * time.Second)
	defer pollTicker.Stop()

	for {
		task, err := s.Get(ctx, workspaceID, taskID)
		if err != nil {
			return nil, err
		}
		if task.IsTerminal() {
			return task, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("await task %q: %w", taskID, ctx.Err())
		case <-timeoutTimer.C:
			return nil, fmt.Errorf("await task %q: %w", taskID, context.DeadlineExceeded)
		case <-pollTicker.C:
		}
	}
}

// Heartbeat renews a lease held by workerID.
func (s *Store) Heartbeat(ctx context.Context, id, workerID string) error {
	now, leaseExpiresAt := s.leaseTimes()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET lease_expires_at=$1, updated_at=$2
		WHERE id=$3 AND worker_id=$4 AND status=$5 AND lease_expires_at >= $2
	`, leaseExpiresAt, now, id, workerID, StatusRunning)
	if err != nil {
		return fmt.Errorf("heartbeat task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q lease is no longer held by worker %q", id, workerID)
	}
	return nil
}

// Complete records a successful running task.
func (s *Store) Complete(ctx context.Context, id string, result json.RawMessage, runID string) error {
	now := s.clock.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$1, result=$2, run_id=$3, stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL, lease_expires_at=NULL,
			completed_at=$4, updated_at=$4
		WHERE id=$5 AND status=$6
	`, StatusCompleted, result, runID, now, id, StatusRunning)
	if err != nil {
		return fmt.Errorf("complete task: %w", err)
	}
	return nil
}

func (s *Store) completeClaimed(ctx context.Context, id, workerID string, result json.RawMessage, runID string) error {
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$1, result=$2, run_id=$3, stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL, lease_expires_at=NULL,
			completed_at=$4, updated_at=$4
		WHERE id=$5 AND worker_id=$6 AND status=$7 AND lease_expires_at >= $4
	`, StatusCompleted, result, runID, now, id, workerID, StatusRunning)
	if err != nil {
		return fmt.Errorf("complete claimed task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is no longer claimed by worker %q", id, workerID)
	}
	return nil
}

// CompleteClaimed records a successful task only while workerID still holds its lease.
func (s *Store) CompleteClaimed(ctx context.Context, id, workerID string, result json.RawMessage, runID string) error {
	return s.completeClaimed(ctx, id, workerID, result, runID)
}

// Fail records an unsuccessful running task.
func (s *Store) Fail(ctx context.Context, id, errMsg string) error {
	now := s.clock.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$1, error=$2, stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL, lease_expires_at=NULL,
			completed_at=$3, updated_at=$3
		WHERE id=$4 AND status=$5
	`, StatusFailed, errMsg, now, id, StatusRunning)
	if err != nil {
		return fmt.Errorf("fail task: %w", err)
	}
	return nil
}

func (s *Store) failClaimed(ctx context.Context, id, workerID, errMsg string) error {
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$1, error=$2, stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL, lease_expires_at=NULL,
			completed_at=$3, updated_at=$3
		WHERE id=$4 AND worker_id=$5 AND status=$6 AND lease_expires_at >= $3
	`, StatusFailed, errMsg, now, id, workerID, StatusRunning)
	if err != nil {
		return fmt.Errorf("fail claimed task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is no longer claimed by worker %q", id, workerID)
	}
	return nil
}

// FailClaimed records a failed task only while workerID still holds its lease.
func (s *Store) FailClaimed(ctx context.Context, id, workerID, errMsg string) error {
	return s.failClaimed(ctx, id, workerID, errMsg)
}

func (s *Store) requeueClaimed(ctx context.Context, id, workerID string, delay time.Duration) error {
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$1, error=NULL, result=NULL, run_id=NULL, stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL,
			lease_expires_at=NULL, started_at=NULL, completed_at=NULL, updated_at=$2, available_at=$6
		WHERE id=$3 AND worker_id=$4 AND status=$5 AND lease_expires_at >= $2
	`, StatusQueued, now, id, workerID, StatusRunning, now.Add(delay))
	if err != nil {
		return fmt.Errorf("requeue claimed task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is no longer claimed by worker %q", id, workerID)
	}
	return nil
}

// Cancel records cancellation intent; active work remains owned until it stops.
func (s *Store) Cancel(ctx context.Context, workspaceID, id string) error {
	return s.cancelTask(ctx, s.pool, workspaceID, id)
}

// CancelTx lets a product command commit its business cancellation with dispatch fencing.
func (s *Store) CancelTx(ctx context.Context, tx pgx.Tx, workspaceID, id string) error {
	return s.cancelTask(ctx, tx, workspaceID, id)
}

func (s *Store) cancelTask(ctx context.Context, query taskQuerier, workspaceID, id string) error {
	if _, err := s.get(ctx, query, workspaceID, id); err != nil {
		return err
	}
	var found string
	err := query.QueryRow(ctx, `UPDATE weave_task_queue
 SET status=CASE WHEN worker_id IS NOT NULL THEN $7 ELSE $3 END,
     completed_at=CASE WHEN worker_id IS NOT NULL THEN NULL ELSE $4::timestamptz END,
     updated_at=$4
 WHERE workspace_id=$1 AND id=$2 AND (status IN ($3,$5,$6,$7) OR (status='failed' AND worker_id IS NOT NULL))
 RETURNING id`, workspaceID, id, StatusCancelled, s.clock.Now(), StatusQueued, StatusRunning, StatusCancelRequested).Scan(&found)
	if err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	return nil
}

// CancelRunTasks closes every non-terminal workflow task belonging to one
// frozen TeamRun. It is the compensation boundary used when the parent
// candidate driver stops waiting: queued roots and fanout legs must not remain
// recoverable after their TeamRun has been cancelled.
func (s *Store) CancelRunTasks(ctx context.Context, workspaceID, runSnapshotID string) (int, error) {
	return s.cancelRunTasks(ctx, s.pool, workspaceID, runSnapshotID)
}

func (s *Store) CancelRunTasksTx(ctx context.Context, tx pgx.Tx, workspaceID, runSnapshotID string) (int, error) {
	return s.cancelRunTasks(ctx, tx, workspaceID, runSnapshotID)
}

func (s *Store) cancelRunTasks(ctx context.Context, query taskQuerier, workspaceID, runSnapshotID string) (int, error) {
	if workspaceID == "" || runSnapshotID == "" {
		return 0, errors.New("workspace_id and run_snapshot_id are required")
	}
	var count int
	err := query.QueryRow(ctx, `WITH cancelled AS (UPDATE weave_task_queue
		SET status=CASE WHEN worker_id IS NOT NULL THEN $8 ELSE $3 END,
			completed_at=CASE WHEN worker_id IS NULL THEN $4::timestamptz ELSE NULL END, updated_at=$4
		WHERE workspace_id=$1 AND run_snapshot_id=$2
		  AND status IN ($5,$6,$7) AND ($9::jsonb IS NULL OR actor_subject=$9::jsonb) RETURNING id)
		SELECT count(*) FROM cancelled
	`, workspaceID, runSnapshotID, StatusCancelled, s.clock.Now(),
		StatusQueued, StatusDispatched, StatusRunning, StatusCancelRequested, subjectFilter(ctx)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("cancel run tasks: %w", err)
	}
	return count, nil
}

// RuntimeLeaseExpiredError identifies a failed remote execution awaiting its
// original worker's exit acknowledgement; expiry alone does not prove exit.
const RuntimeLeaseExpiredError = "runtime offline: execution lease expired"

const ExecutionLeaseExpiredError = "execution lease expired; outcome reconciliation required"

// AcknowledgeExecutionStopped is called only after the worker has joined its
// execution. It preserves an expired execution's failure and clears ownership.
func (s *Store) AcknowledgeExecutionStopped(ctx context.Context, id, workerID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE weave_task_queue
 SET status=CASE WHEN status=$3 THEN $4 ELSE $6 END,
     error=CASE WHEN status=$8 THEN CASE WHEN kind='engine_exec' THEN $7 ELSE $9 END ELSE error END,
     stopped_epoch=claim_epoch, stopped_worker_id=worker_id, worker_id=NULL, lease_expires_at=NULL, completed_at=$5, updated_at=$5
 WHERE id=$1 AND worker_id=$2 AND (status=$3 OR (outcome_sensitive AND
     (status=$8 OR (status=$6 AND error IN ($7,$9)))))`,
		id, workerID, StatusCancelRequested, StatusCancelled, s.clock.Now(), StatusFailed, RuntimeLeaseExpiredError, StatusRunning, ExecutionLeaseExpiredError)
	return err
}

// RunExecutionsStoppedTx requires per-task terminal acknowledgements, including
// remote executions which failed after losing contact with their runtime.
func (*Store) RunExecutionsStoppedTx(ctx context.Context, tx pgx.Tx, workspaceID, runSnapshotID string) (bool, error) {
	var stopped bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM weave_task_queue
		WHERE workspace_id=$1 AND run_snapshot_id=$2
		AND (status IN ('queued','dispatched','running','cancel_requested') OR worker_id IS NOT NULL))`,
		workspaceID, runSnapshotID).Scan(&stopped)
	return stopped, err
}

// RuntimeFailuresStopped reports whether every failed remote attempt has an
// exit acknowledgement. Read projections and retry admission share this rule.
func (s *Store) RuntimeFailuresStopped(ctx context.Context, workspaceID, runSnapshotID string) (bool, error) {
	return runtimeFailuresStopped(ctx, s.pool, workspaceID, runSnapshotID)
}

func RuntimeFailuresStoppedTx(ctx context.Context, tx pgx.Tx, workspaceID, runSnapshotID string) (bool, error) {
	return runtimeFailuresStopped(ctx, tx, workspaceID, runSnapshotID)
}

func runtimeFailuresStopped(ctx context.Context, query taskQuerier, workspaceID, runSnapshotID string) (bool, error) {
	var stopped bool
	err := query.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM weave_task_queue
		WHERE workspace_id=$1 AND run_snapshot_id=$2 AND kind='engine_exec'
		AND status='failed' AND worker_id IS NOT NULL)`, workspaceID, runSnapshotID).Scan(&stopped)
	return stopped, err
}

// RequestCancelTask atomically records a durable cancellation request for a
// running task without clearing its worker lease or marking it terminal.
func (s *Store) RequestCancelTask(ctx context.Context, workspaceID, id string) (bool, error) {
	if _, err := s.Get(ctx, workspaceID, id); err != nil {
		return false, err
	}
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$3, updated_at=$4
		WHERE workspace_id=$1 AND id=$2 AND status=$5
	`, workspaceID, id, StatusCancelRequested, now, StatusRunning)
	if err != nil {
		return false, fmt.Errorf("request task cancellation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Supersede replaces non-terminal tasks with the same context in one workspace.
func (s *Store) Supersede(ctx context.Context, workspaceID, contextKey, exceptID string) error {
	now := s.clock.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$4, worker_id=NULL, lease_expires_at=NULL, completed_at=$5, updated_at=$5
		WHERE workspace_id=$1 AND context_key=$2 AND id<>$3
			AND status IN ($6, $7, $8)
	`, workspaceID, contextKey, exceptID, StatusSuperseded, now,
		StatusQueued, StatusDispatched, StatusRunning)
	if err != nil {
		return fmt.Errorf("supersede tasks: %w", err)
	}
	return nil
}

// RecoverStale retains the owner of executions whose outcome is unknown.
// Team construction and remote execution require a stop receipt before resubmission.
func (s *Store) RecoverStale(ctx context.Context) (int, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `
		UPDATE weave_task_queue
		SET status=CASE WHEN outcome_sensitive THEN $5 ELSE $1 END,
			worker_id=CASE WHEN outcome_sensitive THEN worker_id ELSE NULL END,
			lease_expires_at=CASE WHEN outcome_sensitive THEN lease_expires_at ELSE NULL END,
			error=CASE WHEN outcome_sensitive THEN CASE WHEN kind='engine_exec' THEN $6 ELSE $7 END ELSE error END,
			completed_at=CASE WHEN outcome_sensitive THEN $2::timestamptz ELSE NULL END, updated_at=$2
		WHERE status IN ($3, $4) AND lease_expires_at < $2
		RETURNING id
	`, StatusQueued, now, StatusRunning, StatusDispatched, StatusFailed, RuntimeLeaseExpiredError, ExecutionLeaseExpiredError)
	if err != nil {
		return 0, fmt.Errorf("recover stale tasks: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("count recovered tasks: %w", err)
	}
	return count, nil
}

// ListByGroup returns all tasks of one workspace task group ordered by creation.
func (s *Store) ListByGroup(ctx context.Context, workspaceID, groupID string) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM weave_task_queue
		WHERE workspace_id=$1 AND task_group_id=$2 AND ($3::jsonb IS NULL OR actor_subject=$3::jsonb)
		ORDER BY created_at, id
	`, workspaceID, groupID, subjectFilter(ctx))
	if err != nil {
		return nil, fmt.Errorf("list task group legs: %w", err)
	}
	defer rows.Close()
	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task group leg: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task group legs: %w", err)
	}
	return tasks, nil
}

// ListEngineExecObservations returns completed runtime task results for one
// exact run snapshot and agent identity. TeamRun uses it to reconcile durable
// CLI tool observations after a member settles.
func (s *Store) ListEngineExecObservations(
	ctx context.Context,
	workspaceID, runSnapshotID, agentID string,
) ([]EngineExecObservation, error) {
	if workspaceID == "" || runSnapshotID == "" || agentID == "" {
		return nil, fmt.Errorf("workspace, run snapshot, and agent are required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, result
		FROM weave_task_queue
		WHERE workspace_id=$1
		  AND run_snapshot_id=$2
		  AND agent_id=$3
		  AND kind='engine_exec'
		  AND status=$4
		  AND result IS NOT NULL AND ($5::jsonb IS NULL OR actor_subject=$5::jsonb)
		ORDER BY COALESCE(completed_at, updated_at), created_at, id
	`, workspaceID, runSnapshotID, agentID, StatusCompleted, subjectFilter(ctx))
	if err != nil {
		return nil, fmt.Errorf("list engine exec observations: %w", err)
	}
	defer rows.Close()
	observations := make([]EngineExecObservation, 0)
	for rows.Next() {
		var observation EngineExecObservation
		if err := rows.Scan(&observation.TaskID, &observation.Result); err != nil {
			return nil, fmt.Errorf("scan engine exec observation: %w", err)
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list engine exec observation rows: %w", err)
	}
	return observations, nil
}

// CancelGroupLegs marks every queued, dispatched, or running leg of one group
// as cancelled and returns how many legs changed. In-flight workers lose their
// lease on the next heartbeat; their late completions are dropped because the
// leg is no longer running.
func (s *Store) CancelGroupLegs(ctx context.Context, workspaceID, groupID string) (int, error) {
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status=$3, worker_id=NULL, lease_expires_at=NULL, completed_at=$4, updated_at=$4
		WHERE workspace_id=$1 AND task_group_id=$2 AND status IN ($5, $6, $7)
	`, workspaceID, groupID, StatusCancelled, now,
		StatusQueued, StatusDispatched, StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("cancel task group legs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RequeueFailedLeg moves the newest failed leg of one worker in one group back
// to queued so a worker claims it again. Result, error, run, and lease state
// are reset; the original payload and creation time are kept.
func (s *Store) RequeueFailedLeg(ctx context.Context, workspaceID, groupID, agent string) (*Task, error) {
	now := s.clock.Now()
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_task_queue
		SET status=$4, result=NULL, error=NULL, run_id=NULL, worker_id=NULL,
			lease_expires_at=NULL, started_at=NULL, completed_at=NULL, updated_at=$5
		WHERE id = (
			SELECT id FROM weave_task_queue
			WHERE workspace_id=$1 AND task_group_id=$2 AND agent=$3 AND status=$6 AND worker_id IS NULL AND ($7::jsonb IS NULL OR actor_subject=$7::jsonb)
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		)
		RETURNING `+taskColumns,
		workspaceID, groupID, agent, StatusQueued, now, StatusFailed, subjectFilter(ctx))
	task, err := scanTask(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("no failed leg for worker %q in task group %q", agent, groupID)
		}
		return nil, fmt.Errorf("requeue failed leg: %w", err)
	}
	return task, nil
}

// RequeueFailedTask retries one exact failed task while retaining its frozen
// payload and identity. The expected snapshot and context prevent a caller
// from reviving unrelated or superseded work.
func (s *Store) RequeueFailedTask(ctx context.Context, workspaceID, id, runSnapshotID, contextKey string) (*Task, error) {
	return s.requeueFailedTask(ctx, s.pool, workspaceID, id, runSnapshotID, contextKey)
}

// RequeueFailedTaskTx keeps the branch requeue in the transaction fencing its parent run.
func (s *Store) RequeueFailedTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, id, runSnapshotID, contextKey string) (*Task, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	return s.requeueFailedTask(ctx, tx, workspaceID, id, runSnapshotID, contextKey)
}

type taskQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) requeueFailedTask(ctx context.Context, query taskQuerier, workspaceID, id, runSnapshotID, contextKey string) (*Task, error) {
	if _, err := s.get(ctx, query, workspaceID, id); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	row := query.QueryRow(ctx, `
		UPDATE weave_task_queue
		SET status=$6,result=NULL,error=NULL,run_id=NULL,worker_id=NULL,
            runtime_assignment=jsonb_set(COALESCE(runtime_assignment,'{}'::jsonb),'{retry_generation}',to_jsonb(COALESCE((runtime_assignment->>'retry_generation')::int,0)+1),true),
            lease_expires_at=NULL,started_at=NULL,completed_at=NULL,updated_at=$7
		WHERE workspace_id=$1 AND id=$2 AND run_snapshot_id=$3 AND context_key=$4 AND status=$5 AND worker_id IS NULL
		RETURNING `+taskColumns,
		workspaceID, id, runSnapshotID, contextKey, StatusFailed, StatusQueued, now)
	task, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("failed task %q is not retryable", id)
	}
	if err != nil {
		return nil, fmt.Errorf("requeue failed task: %w", err)
	}
	return task, nil
}

// Get reads one task from a workspace.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (*Task, error) {
	return s.get(ctx, s.pool, workspaceID, id)
}

// GetTx reads a task using the caller's transaction and connection.
func (s *Store) GetTx(ctx context.Context, tx pgx.Tx, workspaceID, id string) (*Task, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	return s.get(ctx, tx, workspaceID, id)
}

func (s *Store) get(ctx context.Context, query taskQuerier, workspaceID, id string) (*Task, error) {
	row := query.QueryRow(ctx, `
		SELECT `+taskColumns+`
		FROM weave_task_queue
		WHERE workspace_id=$1 AND id=$2 AND ($3::jsonb IS NULL OR actor_subject=$3::jsonb)
	`, workspaceID, id, subjectFilter(ctx))
	task, err := scanTask(row)
	if err != nil {
		return nil, fmt.Errorf("task %q not found: %w", id, err)
	}
	return task, nil
}

// List returns paginated tasks from a workspace.
func (s *Store) List(ctx context.Context, workspaceID string, limit, offset int) ([]Task, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM weave_task_queue WHERE workspace_id=$1 AND ($2::jsonb IS NULL OR actor_subject=$2::jsonb)
	`, workspaceID, subjectFilter(ctx)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tasks: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM weave_task_queue
		WHERE workspace_id=$1 AND ($4::jsonb IS NULL OR actor_subject=$4::jsonb)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, workspaceID, limit, offset, subjectFilter(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, total, nil
}

// ListFiltered returns paginated workspace tasks matching any supplied status.
func (s *Store) ListFiltered(
	ctx context.Context,
	workspaceID string,
	statuses []string,
	limit, offset int,
) ([]Task, int, error) {
	if len(statuses) == 0 {
		return s.List(ctx, workspaceID, limit, offset)
	}

	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_task_queue
		WHERE workspace_id=$1 AND status=ANY($2::text[]) AND ($3::jsonb IS NULL OR actor_subject=$3::jsonb)
	`, workspaceID, statuses, subjectFilter(ctx)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count filtered tasks: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM weave_task_queue
		WHERE workspace_id=$1 AND status=ANY($2::text[]) AND ($5::jsonb IS NULL OR actor_subject=$5::jsonb)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4
	`, workspaceID, statuses, limit, offset, subjectFilter(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("list filtered tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan filtered task: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list filtered tasks: %w", err)
	}
	return tasks, total, nil
}

// ListFilteredByProject returns paginated tasks with immutable Project and status filters.
func (s *Store) ListFilteredByProject(
	ctx context.Context,
	workspaceID, projectID string,
	statuses []string,
	limit, offset int,
) ([]Task, int, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return s.ListFiltered(ctx, workspaceID, statuses, limit, offset)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_task_queue
		WHERE workspace_id=$1 AND project_id=$2
		  AND ($3::text[] IS NULL OR cardinality($3::text[])=0 OR status=ANY($3::text[]))
	AND ($4::jsonb IS NULL OR actor_subject=$4::jsonb)
	`, workspaceID, projectID, statuses, subjectFilter(ctx)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count project tasks: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM weave_task_queue
		WHERE workspace_id=$1 AND project_id=$2
		  AND ($3::text[] IS NULL OR cardinality($3::text[])=0 OR status=ANY($3::text[]))
AND ($6::jsonb IS NULL OR actor_subject=$6::jsonb)
		ORDER BY created_at DESC
		LIMIT $4 OFFSET $5
	`, workspaceID, projectID, statuses, limit, offset, subjectFilter(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("list project tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan project task: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list project tasks: %w", err)
	}
	return tasks, total, nil
}

const taskColumns = `
		id, workspace_id, COALESCE(project_id, ''), COALESCE(agent, ''), COALESCE(agent_id, ''), COALESCE(agent_version, 0),
	identity_kind, identity_schema_version, COALESCE(execution_scope, ''),
	COALESCE(workflow_id, ''), COALESCE(workflow_version, 0),
	COALESCE(run_snapshot_id, ''),
	source, kind, COALESCE(runtime_id, ''), runtime_assignment, status, priority,
	COALESCE(context_key, ''), COALESCE(trace_id, ''), COALESCE(parent_task_id, ''),
	COALESCE(task_group_id, ''), subtask_deadline_at,
	payload, result, COALESCE(error, ''), COALESCE(run_id, ''), COALESCE(worker_id, ''),
	lease_expires_at, created_at, started_at, completed_at, updated_at, claim_epoch, COALESCE(build_run_id, ''), available_at, actor_subject, deadline_at, outcome_sensitive, physical_usage, unreported_attempts, stopped_epoch, COALESCE(stopped_worker_id,''), COALESCE(capability_invocation_id,''), source_ref`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	var task Task
	var actorSubject, physicalUsage []byte
	if err := row.Scan(
		&task.ID, &task.WorkspaceID, &task.ProjectID, &task.Agent, &task.AgentID, &task.AgentVersion,
		&task.IdentityKind, &task.IdentitySchemaVersion, &task.ExecutionScope,
		&task.WorkflowID, &task.WorkflowVersion, &task.RunSnapshotID,
		&task.Source, &task.Kind, &task.RuntimeID, &task.RuntimeAssignment, &task.Status, &task.Priority,
		&task.ContextKey, &task.TraceID, &task.ParentTaskID,
		&task.TaskGroupID, &task.SubtaskDeadlineAt,
		&task.Payload, &task.Result, &task.Error, &task.RunID, &task.WorkerID,
		&task.LeaseExpiresAt, &task.CreatedAt, &task.StartedAt, &task.CompletedAt, &task.UpdatedAt, &task.ClaimEpoch, &task.BuildRunID, &task.AvailableAt, &actorSubject, &task.DeadlineAt, &task.OutcomeSensitive, &physicalUsage, &task.UnreportedAttempts, &task.StoppedEpoch, &task.StoppedWorkerID, &task.CapabilityInvocationID, &task.SourceRef,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(physicalUsage, &task.PhysicalUsage); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(actorSubject, &task.Subject); err != nil {
		return nil, err
	}
	if task.Subject.Validate() != nil || task.Subject.WorkspaceID != task.WorkspaceID {
		return nil, execution.ErrSubjectMismatch
	}
	return &task, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
