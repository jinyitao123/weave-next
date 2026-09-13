package capabilities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type ReconciliationDecision string

const (
	ReconcileRetrySafe ReconciliationDecision = "retry_safe"
	ReconcileCompleted ReconciliationDecision = "completed"
	ReconcileFailed    ReconciliationDecision = "failed"
	ReconcileCancelled ReconciliationDecision = "cancelled"
)

// Reconciliation is a business disposition. The physical stop receipt is read
// from the platform queue and must match its last claim epoch.
type Reconciliation struct {
	Decision ReconciliationDecision
	Result   json.RawMessage
	Error    string
}

func (s *PGStore) ReconcileTask(ctx context.Context, workspaceID, invocationID string, observation Reconciliation) (Invocation, error) {
	if workspaceID == "" || invocationID == "" {
		return Invocation{}, errors.New("capability reconciliation identity is required")
	}
	if observation.Decision == ReconcileCompleted && !json.Valid(observation.Result) {
		return Invocation{}, errors.New("capability reconciliation result is not JSON")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var taskID, appID, current string
	if err := tx.QueryRow(ctx, `SELECT task_id,application_id,status FROM weave_capability_invocations
	 WHERE workspace_id=$1 AND invocation_id=$2 FOR UPDATE`, workspaceID, invocationID).Scan(&taskID, &appID, &current); err != nil {
		return Invocation{}, ErrClaimLost
	}
	physical, err := s.queue.GetTx(ctx, tx, workspaceID, taskID)
	if err != nil || physical.StoppedEpoch != physical.ClaimEpoch || physical.StoppedWorkerID == "" || physical.WorkerID != "" {
		return Invocation{}, ErrClaimLost
	}
	decision := observation.Decision
	if current == "cancel_requested" {
		decision = ReconcileCancelled
	}
	queueResult := taskqueue.Reconciliation{}
	status, resultState := "failed", "unavailable"
	var result any
	var errorText any
	switch decision {
	case ReconcileRetrySafe:
		status, queueResult.Status = "queued", taskqueue.StatusQueued
	case ReconcileCompleted:
		status, resultState, result = "completed", "available", string(observation.Result)
		queueResult.Status, queueResult.Result = taskqueue.StatusCompleted, observation.Result
	case ReconcileFailed:
		if observation.Error == "" {
			return Invocation{}, errors.New("failed capability reconciliation requires an error")
		}
		errorText = observation.Error
		queueResult.Status, queueResult.Error = taskqueue.StatusFailed, observation.Error
	case ReconcileCancelled:
		status, queueResult.Status = "cancelled", taskqueue.StatusCancelled
	default:
		return Invocation{}, errors.New("unsupported capability reconciliation decision")
	}
	if _, err := s.queue.ReconcileTaskTx(ctx, tx, workspaceID, taskID, physical.ClaimEpoch, queueResult); err != nil {
		return Invocation{}, ErrClaimLost
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$3,result_state=$4,result=$5::jsonb,error=$6,
	 completed_at=CASE WHEN $3 IN ('completed','failed','cancelled') THEN now() ELSE NULL END
	 WHERE workspace_id=$1 AND invocation_id=$2`, workspaceID, invocationID, status, resultState, result, errorText); err != nil {
		return Invocation{}, err
	}
	detail, _ := json.Marshal(map[string]any{"decision": decision, "claim_epoch": physical.ClaimEpoch, "stopped_worker_id": physical.StoppedWorkerID})
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_invocation_events(workspace_id,invocation_id,event_type,detail)
	 VALUES($1,$2,'execution_reconciled',$3::jsonb)`, workspaceID, invocationID, string(detail)); err != nil {
		return Invocation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, err
	}
	return s.GetInvocation(ctx, workspaceID, appID, invocationID)
}
