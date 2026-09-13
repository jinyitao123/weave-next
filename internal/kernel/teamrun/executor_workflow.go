package teamrun

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func (e *Executor) processConsumedWorkflowRun(ctx context.Context, task *taskqueue.Task, workerID string, run TeamRun) error {
	if run.Status.Terminal() {
		return e.finishTerminalTask(ctx, task, workerID, run, nil)
	}
	if run.Status == StatusParked {
		return e.finishParkedTask(ctx, task, workerID, run)
	}

	executorID := executorIdentity(task.ID, workerID)
	wasRunning := run.Status == StatusRunning
	run, checkpoint, terminal, err := e.prepareWorkflowRunExecution(ctx, run, task, workerID, executorID)
	if errors.Is(err, errRuntimeRetryOwnsExecution) {
		return e.finishParkedTask(ctx, task, workerID, run)
	}
	if err != nil {
		return err
	}
	if terminal {
		return e.finishTerminalTask(ctx, task, workerID, run, nil)
	}
	if wasRunning && checkpoint != nil && checkpoint.ActiveMember != nil {
		return e.parkInterruptedMember(ctx, run, task, workerID, executorID, *checkpoint)
	}

	result, runErr := e.executePreparedWorkflowRun(ctx, run, task, workerID, checkpoint)
	if runErr != nil && (errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	if runErr != nil {
		failed, failErr := e.failRunning(
			ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason, result.MemberBreakdown,
		)
		if failErr != nil {
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}

	return e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
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
