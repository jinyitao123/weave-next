package teamrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

var errRuntimeRetryOwnsExecution = errors.New("runtime retry owns workflow execution")

func (e *Executor) processRuntimeRetry(ctx context.Context, task *taskqueue.Task, workerID string) error {
	var payload RuntimeRetryTaskPayloadV1
	if err := decodeExact(task.Payload, &payload); err != nil || payload.SchemaVersion != 1 ||
		payload.Kind != "runtime_retry" || payload.RunID == "" || payload.RunID != task.ContextKey ||
		payload.NodeID == "" || payload.IdempotencyKey == "" {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin runtime retry continuation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reject := func() error {
		_ = tx.Rollback(ctx)
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	run, err := e.Runs.GetForUpdateTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	executorID := runtimeRetryExecutorID(payload.RunID, payload.NodeID, run.ResumeGeneration)
	if task.ID != runtimeRetryTaskID(task.WorkspaceID, payload.RunID, payload.NodeID, run.ResumeGeneration) ||
		payload.IdempotencyKey != runtimeRetryKey(payload.RunID, payload.NodeID, run.ResumeGeneration) ||
		run.WorkflowID != task.WorkflowID || run.WorkflowVersion != task.WorkflowVersion || run.RunSnapshotID != task.RunSnapshotID {
		return reject()
	}
	transition, present, err := readTransitionByKey(ctx, tx, task.WorkspaceID, payload.RunID, payload.IdempotencyKey)
	if err != nil {
		return err
	}
	if !present || transition.FromStatus == nil || *transition.FromStatus != StatusParked || transition.ToStatus != StatusRunning ||
		transition.ResumeGeneration != run.ResumeGeneration || transition.Source != "runtime_retry" {
		return reject()
	}
	// Terminalization and queue acknowledgement are separate commits. A worker
	// reclaiming after that crash window must not rerun an already finished stage.
	if run.Status.Terminal() {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return e.finishTerminalTask(ctx, task, workerID, run, nil)
	}
	if run.Status != StatusRunning || run.CurrentExecutorID == nil || *run.CurrentExecutorID != executorID ||
		transition.Generation != run.Generation || transition.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch {
		return reject()
	}
	checkpoint, err := e.Checkpoints.GetTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil || checkpoint.NodeID != payload.NodeID ||
		checkpoint.TeamRunGeneration != run.Generation || checkpoint.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch {
		return reject()
	}
	// One accepted continuation authorizes one process entry. If its queue
	// lease is reclaimed after a crash, wait for another user continuation.
	memberReclaimed := false
	if checkpoint.ActiveMember != nil {
		inserted, err := tx.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at)
			VALUES($1,$2,$3,statement_timestamp()) ON CONFLICT(namespace,key) DO NOTHING`,
			"member-retry-entry:"+run.WorkspaceID, task.ID, []byte(`{"schema_version":1}`))
		if err != nil {
			return err
		}
		memberReclaimed = inserted.RowsAffected() == 0
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if memberReclaimed {
		return e.parkInterruptedMember(ctx, run, task, workerID, executorID, checkpoint)
	}
	result, runErr := e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		return e.Runtime.ResumeCheckpoint(execCtx, run, task, checkpoint)
	})
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	if runErr != nil {
		failed, failErr := e.failRunning(ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason, result.MemberBreakdown)
		if failErr != nil {
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}
	return e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
}
