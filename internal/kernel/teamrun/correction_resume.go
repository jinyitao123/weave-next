package teamrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func (e *Executor) processCorrectionResume(ctx context.Context, task *taskqueue.Task, workerID string) error {
	var payload CorrectionResumeTaskPayloadV1
	if err := decodeExact(task.Payload, &payload); err != nil || payload.SchemaVersion != 1 ||
		payload.Kind != "correction_resume" || payload.RunID == "" || payload.RunID != task.ContextKey ||
		payload.CorrectionID == "" || payload.IdempotencyKey == "" ||
		(payload.Disposition != "apply" && payload.Disposition != "discard") {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin correction continuation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := e.Runs.GetForUpdateTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	executorID := correctionResumeExecutorID(payload.RunID, payload.IdempotencyKey)
	if run.Status != StatusRunning || run.CurrentExecutorID == nil || *run.CurrentExecutorID != executorID ||
		run.WorkflowID != task.WorkflowID || run.WorkflowVersion != task.WorkflowVersion || run.RunSnapshotID != task.RunSnapshotID {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	transition, present, err := readTransitionByKey(ctx, tx, task.WorkspaceID, payload.RunID, payload.IdempotencyKey)
	if err != nil {
		return err
	}
	if !present || transition.FromStatus == nil || *transition.FromStatus != StatusParked || transition.ToStatus != StatusRunning {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	checkpoint, err := e.Checkpoints.GetTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return err
	}
	if payload.Disposition == "apply" {
		if e.Corrections == nil {
			return errors.New("correction store is unavailable")
		}
		if err := e.Corrections.MarkAppliedTx(ctx, tx, task.WorkspaceID, payload.RunID, payload.CorrectionID, executorID, e.now()); err != nil {
			return fmt.Errorf("mark correction applied: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit correction continuation read: %w", err)
	}
	result, runErr := e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		return e.Runtime.ResumeCheckpoint(execCtx, run, task, checkpoint)
	})
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	if runErr != nil {
		failed, failErr := e.failRunning(ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason)
		if failErr != nil {
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}
	return e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
}
