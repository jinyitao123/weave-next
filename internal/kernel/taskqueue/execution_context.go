package taskqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// ClaimedTaskReader re-reads authoritative task ownership at the execution
// boundary. A caller-provided Task alone is not sufficient authority.
type ClaimedTaskReader interface {
	Get(context.Context, string, string) (*Task, error)
}

func BindTaskExecution(ctx context.Context, task *Task, reader ClaimedTaskReader) (context.Context, error) {
	if task == nil || reader == nil || task.ID == "" || task.WorkerID == "" || task.ClaimEpoch < 1 || task.Status != StatusRunning {
		return nil, execution.ErrCurrentTaskMismatch
	}
	bound, err := BindTaskSubject(ctx, task)
	if err != nil {
		return nil, err
	}
	current, err := reader.Get(bound, task.WorkspaceID, task.ID)
	if err != nil {
		return nil, fmt.Errorf("read current execution claim: %w", err)
	}
	if current == nil || current.ID != task.ID || current.WorkspaceID != task.WorkspaceID || current.Subject != task.Subject ||
		current.Status != StatusRunning || current.WorkerID != task.WorkerID || current.ClaimEpoch != task.ClaimEpoch {
		return nil, execution.ErrCurrentTaskMismatch
	}
	return execution.WithCurrentTask(bound, execution.CurrentTask{ID: current.ID, WorkspaceID: current.WorkspaceID,
		Subject: current.Subject, WorkerID: current.WorkerID, ClaimEpoch: current.ClaimEpoch})
}

// ValidateCurrentTaskTx fences a write against the trusted executing claim.
// The caller owns the transaction. This only locks and validates the task; it
// never renews a lease, changes state, or creates execution authority.
func (s *Store) ValidateCurrentTaskTx(ctx context.Context, tx pgx.Tx) error {
	if s == nil || tx == nil {
		return execution.ErrCurrentTaskMismatch
	}
	_, err := s.currentTaskDeadlineTx(ctx, tx)
	return err
}

func (s *Store) currentTaskDeadlineTx(ctx context.Context, tx pgx.Tx) (*time.Time, error) {
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok {
		return nil, execution.ErrCurrentTaskMismatch
	}
	subject, err := execution.RequireSubject(ctx, current.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if current.Subject != subject || current.ID == "" || current.WorkerID == "" || current.ClaimEpoch < 1 {
		return nil, execution.ErrCurrentTaskMismatch
	}
	var status, worker string
	var epoch int64
	var lease, deadline *time.Time
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT status,COALESCE(worker_id,''),claim_epoch,lease_expires_at,deadline_at,actor_subject
 FROM weave_task_queue WHERE workspace_id=$1 AND id=$2 FOR SHARE`, current.WorkspaceID, current.ID).
		Scan(&status, &worker, &epoch, &lease, &deadline, &raw)
	if err != nil {
		return nil, fmt.Errorf("read current execution claim: %w", err)
	}
	var parentSubject execution.Subject
	if err = json.Unmarshal(raw, &parentSubject); err != nil {
		return nil, err
	}
	// A lock wait may span a lease or deadline, so validate against fresh time.
	now := s.clock.Now()
	if status != StatusRunning || worker != current.WorkerID || epoch != current.ClaimEpoch || parentSubject != subject ||
		lease == nil || !lease.After(now) || (deadline != nil && !deadline.After(now)) {
		return nil, execution.ErrCurrentTaskMismatch
	}
	return deadline, nil
}

// validateControlParent serializes child admission with parent cancellation and
// claim replacement. The SQL parent fence still covers all explicit parents;
// this check additionally rejects a stale executing owner's context.
func (s *Store) validateControlParent(ctx context.Context, tx pgx.Tx, task *Task) error {
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok {
		return nil
	}
	if current.WorkspaceID != task.WorkspaceID {
		return execution.ErrCurrentTaskMismatch
	}
	if task.ParentTaskID == "" {
		if task.Kind == "engine_exec" {
			return fmt.Errorf("engine child omitted its current control parent")
		}
		return nil
	}
	if task.ParentTaskID != current.ID {
		return execution.ErrCurrentTaskMismatch
	}
	deadline, err := s.currentTaskDeadlineTx(ctx, tx)
	if err != nil {
		return err
	}
	if deadline != nil && (task.DeadlineAt == nil || deadline.Before(*task.DeadlineAt)) {
		copy := *deadline
		task.DeadlineAt = &copy
	}
	return nil
}

// AcknowledgeUnstartedClaim is used only before a handler has been invoked.
// It releases a stopped claim with a complete ownership CAS, so cancellation
// between Claim and Bind cannot strand an owner or clear a replacement claim.
func (s *Store) AcknowledgeUnstartedClaim(ctx context.Context, task *Task) error {
	if task == nil || task.Subject.Validate() != nil || task.Subject.WorkspaceID != task.WorkspaceID ||
		task.ID == "" || task.WorkerID == "" || task.ClaimEpoch < 1 {
		return execution.ErrCurrentTaskMismatch
	}
	subject, _ := json.Marshal(task.Subject)
	_, err := s.pool.Exec(ctx, `UPDATE weave_task_queue
 SET status=CASE WHEN status='cancel_requested' THEN 'cancelled' ELSE status END,
     stopped_epoch=claim_epoch,stopped_worker_id=worker_id,worker_id=NULL,lease_expires_at=NULL,
     completed_at=$6,updated_at=$6
 WHERE workspace_id=$1 AND id=$2 AND worker_id=$3 AND claim_epoch=$4 AND actor_subject=$5::jsonb
   AND (status='cancel_requested' OR (status='failed' AND error IN ($7,$8)))`,
		task.WorkspaceID, task.ID, task.WorkerID, task.ClaimEpoch, string(subject), s.clock.Now(),
		RuntimeLeaseExpiredError, ExecutionLeaseExpiredError)
	return err
}
