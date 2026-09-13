package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type ReconciliationDecision string

const (
	ReconcileRetrySafe ReconciliationDecision = "retry_safe"
	ReconcileCompleted ReconciliationDecision = "completed"
	ReconcileFailed    ReconciliationDecision = "failed"
	ReconcileCancelled ReconciliationDecision = "cancelled"
)

// Reconciliation is a trusted observation from the physical runtime. ReceiptID
// identifies the runtime stop or terminal receipt used to settle uncertainty.
type Reconciliation struct {
	Decision  ReconciliationDecision
	ReceiptID string
	Result    json.RawMessage
	Error     string
}

func (s *PGStore) ReconcileTask(ctx context.Context, workspaceID, invocationID string, observation Reconciliation) (Invocation, error) {
	if workspaceID == "" || invocationID == "" || observation.ReceiptID == "" {
		return Invocation{}, errors.New("capability reconciliation identity and receipt are required")
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
	if current != "reconciling" && current != "cancel_requested" {
		return Invocation{}, ErrClaimLost
	}
	decision := observation.Decision
	if current == "cancel_requested" {
		decision = ReconcileCancelled
	}
	status, resultState := "failed", "unavailable"
	var result any
	var errorText any = nil
	switch decision {
	case ReconcileRetrySafe:
		status = "queued"
	case ReconcileCompleted:
		status, resultState, result = "completed", "available", string(observation.Result)
	case ReconcileFailed:
		if observation.Error == "" {
			return Invocation{}, errors.New("failed capability reconciliation requires an error")
		}
		errorText = observation.Error
	case ReconcileCancelled:
		status = "cancelled"
	default:
		return Invocation{}, errors.New("unsupported capability reconciliation decision")
	}
	if decision == ReconcileRetrySafe {
		tag, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks
		 SET status='queued',claim_token=NULL,deadline_at=NULL,claim_started_at=NULL
		 WHERE task_id=$1 AND status='reconciling'`, taskID)
		if err != nil {
			return Invocation{}, err
		}
		if tag.RowsAffected() != 1 {
			return Invocation{}, ErrClaimLost
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks
		 SET status=$2,claim_token=NULL,deadline_at=NULL,claim_started_at=NULL
		 WHERE task_id=$1 AND status='reconciling'`, taskID, status)
		if err != nil {
			return Invocation{}, err
		}
		if tag.RowsAffected() != 1 {
			return Invocation{}, ErrClaimLost
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations
	 SET status=$3,result_state=$4,result=$5::jsonb,error=$6,
	 completed_at=CASE WHEN $3 IN ('completed','failed','cancelled') THEN now() ELSE NULL END
	 WHERE workspace_id=$1 AND invocation_id=$2`, workspaceID, invocationID, status, resultState, result, errorText); err != nil {
		return Invocation{}, err
	}
	detail, _ := json.Marshal(map[string]string{"decision": string(decision), "receipt_id": observation.ReceiptID})
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_invocation_events(workspace_id,invocation_id,event_type,detail)
	 VALUES($1,$2,'execution_reconciled',$3::jsonb)`, workspaceID, invocationID, string(detail)); err != nil {
		return Invocation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, fmt.Errorf("commit capability reconciliation: %w", err)
	}
	return s.GetInvocation(ctx, workspaceID, appID, invocationID)
}
