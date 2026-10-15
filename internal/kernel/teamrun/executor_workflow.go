package teamrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

var errCancellationTerminalNotSettled = errors.New("team run cancellation terminal not settled")

const cancellationTerminalPersistenceTimeout = 5 * time.Second

func (e *Executor) processConsumedWorkflowRun(ctx context.Context, task *taskqueue.Task, workerID string, run TeamRun) error {
	if run.Status.Terminal() {
		return e.finishTerminalTask(ctx, task, workerID, run, nil)
	}
	if run.Status == StatusParked {
		return e.finishParkedTask(ctx, task, workerID, run)
	}

	executorID := executorIdentity(task.ID, workerID)
	wasRunning := run.Status == StatusRunning
	preparingRun := run
	run, checkpoint, terminal, err := e.prepareWorkflowRunExecution(ctx, run, task, workerID, executorID)
	if errors.Is(err, errRuntimeRetryOwnsExecution) {
		return e.finishParkedTask(ctx, task, workerID, run)
	}
	if err != nil {
		if preparingRun.Status == StatusRunning {
			_, settleErr := e.settleStoppedCancellation(ctx, preparingRun, task, RuntimeResult{
				UsageComplete: false, UsageIncompleteReason: UsageIncompleteReasonAttemptLost,
			}, err)
			if errors.Is(settleErr, errCancellationTerminalNotSettled) {
				return settleErr
			}
		}
		return err
	}
	if terminal {
		return e.finishTerminalTask(ctx, task, workerID, run, nil)
	}
	if wasRunning && checkpoint != nil && checkpoint.ActiveMember != nil {
		return e.parkInterruptedMember(ctx, run, task, workerID, executorID, *checkpoint)
	}

	result, runErr := e.executePreparedWorkflowRun(ctx, run, task, workerID, checkpoint)
	if runErr != nil {
		settled, settleErr := e.settleStoppedCancellation(ctx, run, task, result, runErr)
		if settled || errors.Is(settleErr, errCancellationTerminalNotSettled) ||
			errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
			errors.Is(runErr, errTaskLeaseLost) {
			return settleErr
		}
		failed, failErr := e.failRunning(
			ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason, result.MemberBreakdown,
		)
		if failErr != nil {
			settled, settleErr = e.settleStoppedCancellation(ctx, run, task, result, runErr)
			if settled || errors.Is(settleErr, errCancellationTerminalNotSettled) {
				return settleErr
			}
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}

	finishErr := e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
	if finishErr != nil {
		settled, settleErr := e.settleStoppedCancellation(ctx, run, task, result, finishErr)
		if settled || errors.Is(settleErr, errCancellationTerminalNotSettled) {
			return settleErr
		}
	}
	return finishErr
}

func (e *Executor) settleStoppedCancellation(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	result RuntimeResult,
	executionErr error,
) (bool, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancellationTerminalPersistenceTimeout)
	defer cancel()

	current, err := e.readRunForCancellationSettlement(settleCtx, run)
	if err != nil {
		return false, fmt.Errorf("%w: read current TeamRun state: %v", errCancellationTerminalNotSettled, err)
	}
	if current.Status != StatusCancelRequested && current.Status != StatusAbandoned {
		if task != nil && e.Tasks != nil {
			currentTask, taskErr := e.Tasks.Get(settleCtx, task.WorkspaceID, task.ID)
			if taskErr != nil {
				return false, fmt.Errorf("%w: read current task state: %v", errCancellationTerminalNotSettled, taskErr)
			}
			if currentTask != nil && currentTask.Status == taskqueue.StatusCancelRequested {
				return false, fmt.Errorf("%w: task cancellation has no matching TeamRun cancellation", errCancellationTerminalNotSettled)
			}
		}
		// A canceled process context or a lost queue lease without a durable
		// TeamRun cancellation request is a recovery event, not user cancellation.
		// Leave the attempt active for the existing reclaim/fencing path.
		return false, executionErr
	}
	if current.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch {
		if current.Status != StatusAbandoned || current.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch+1 {
			return false, executionErr
		}
	}
	terminalRun := current
	if current.Status == StatusAbandoned {
		// The grace sweeper may have fenced the TeamRun before the local runtime
		// returned. The exact old attempt can still be closed using the original
		// frozen epoch; the lease owner CAS prevents a late writer from settling a
		// newer attempt.
		terminalRun = run
	}
	usageComplete := result.UsageComplete
	usageIncompleteReason := result.UsageIncompleteReason
	if !usageComplete && usageIncompleteReason == "" {
		usageIncompleteReason = UsageIncompleteReasonAttemptLost
	}
	if usageIncompleteReason != "" {
		usageComplete = false
	}
	if err := e.commitFrozenNormalTerminal(
		settleCtx,
		terminalRun,
		"failed",
		teamRunCancelledTerminalStopReason,
		result.Usage,
		result.UsageCoverage,
		usageComplete,
		usageIncompleteReason,
		result.MemberBreakdown,
	); err != nil {
		return false, fmt.Errorf("%w: persist stopped cancelled attempt: %v", errCancellationTerminalNotSettled, err)
	}
	return true, executionErr
}

func (e *Executor) readRunForCancellationSettlement(ctx context.Context, run TeamRun) (TeamRun, error) {
	if e == nil || e.Transactions == nil || e.Runs == nil {
		return TeamRun{}, errors.New("team run settlement dependencies are unavailable")
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin cancellation state read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := e.Runs.GetForUpdateTx(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil {
		return TeamRun{}, fmt.Errorf("lock current TeamRun: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit current TeamRun read: %w", err)
	}
	return current, nil
}

func (e *Executor) prepareWorkflowRunExecution(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	workerID string,
	executorID string,
) (TeamRun, *WorkflowCheckpointV1, bool, error) {
	switch run.Status {
	case StatusQueued:
		claimed, err := e.claimRunning(ctx, run, task, executorID)
		if err != nil {
			return TeamRun{}, nil, false, err
		}
		return claimed, nil, false, nil
	case StatusRunning:
		return e.reclaimRunning(ctx, run, task, executorID)
	default:
		err := e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeStateConflict))
		return TeamRun{}, nil, false, err
	}
}

func (e *Executor) executePreparedWorkflowRun(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	workerID string,
	checkpoint *WorkflowCheckpointV1,
) (RuntimeResult, error) {
	if checkpoint == nil {
		return e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
			return e.Runtime.Execute(execCtx, run, task)
		})
	}
	return e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		return e.Runtime.ResumeCheckpoint(execCtx, run, task, *checkpoint)
	})
}
