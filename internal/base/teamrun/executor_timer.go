package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type timerResumePlan struct {
	run        TeamRun
	task       *taskqueue.Task
	checkpoint WorkflowCheckpointV1
	executorID string
}

type timerWaitDetail struct {
	WakeAt time.Time `json:"wake_at"`
	NodeID string    `json:"node_id"`
}

func (e *Executor) prepareTimerWakeTx(
	ctx context.Context,
	tx pgx.Tx,
	locked TeamRun,
	now time.Time,
) (*timerResumePlan, error) {
	if locked.Status != StatusParked || locked.WaitKind == nil ||
		*locked.WaitKind != WaitTimer || locked.CheckpointRef == nil ||
		*locked.CheckpointRef != CheckpointRef(locked.WorkspaceID, locked.RunID) {
		return nil, e.failTimerWaitTx(
			ctx, tx, locked, now, ErrorCodeIdentityMismatch,
			errors.New("timer wait envelope differs from checkpoint convention"),
		)
	}
	var detail timerWaitDetail
	if err := decodeExact(locked.WaitDetail, &detail); err != nil ||
		detail.WakeAt.IsZero() || detail.NodeID == "" || detail.WakeAt.After(now) {
		if err == nil {
			err = errors.New("timer wait detail is invalid or not expired")
		}
		return nil, e.failTimerWaitTx(
			ctx, tx, locked, now, ErrorCodeResumeInvalid, err,
		)
	}
	executorID := fmt.Sprintf(
		"teamrun-timer:%s:%d", locked.RunID, locked.ResumeGeneration,
	)
	resumeTx, err := tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin timer resume savepoint: %w", err)
	}
	resumed, err := e.Runs.ResumeRunningTx(ctx, resumeTx, ResumeRequest{
		WorkspaceID:                 locked.WorkspaceID,
		RunID:                       locked.RunID,
		ExpectedStatus:              StatusParked,
		ExpectedTeamRunGeneration:   locked.Generation,
		ExpectedExecutionLeaseEpoch: locked.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    locked.ResumeGeneration,
		ExpectedWaitKind:            WaitTimer,
		ExpectedResumeTokenHash:     locked.ResumeTokenHash,
		ExecutorID:                  executorID,
		IdempotencyKey: fmt.Sprintf(
			"teamrun-timer-resume:%s:%d", locked.RunID, locked.ResumeGeneration,
		),
		Actor:      "teamrun-timer-wake-sweeper",
		Source:     "teamrun_worker",
		OccurredAt: now,
	})
	if err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, err
	}
	checkpoint, err := e.Checkpoints.GetTx(
		ctx, resumeTx, locked.WorkspaceID, locked.RunID,
	)
	if err == nil {
		err = ValidateCheckpointRun(checkpoint, resumed)
	}
	if err == nil && checkpoint.NodeID != detail.NodeID {
		err = fmt.Errorf("%w: timer node differs from checkpoint", ErrTeamRunIdentityMismatch)
	}
	if err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, e.failTimerWaitTx(ctx, tx, locked, now, checkpointErrorCode(err), err)
	}
	runSnapshot, err := e.Consumer.Snapshots.GetByRunID(
		ctx, locked.WorkspaceID, locked.RunSnapshotID,
	)
	if err == nil {
		err = ValidateCheckpointSnapshot(checkpoint, runSnapshot)
	}
	if err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, e.failTimerWaitTx(ctx, tx, locked, now, checkpointErrorCode(err), err)
	}
	task, err := e.Tasks.Get(ctx, locked.WorkspaceID, locked.SourceTaskID)
	if err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, e.failTimerWaitTx(
			ctx, tx, locked, now, ErrorCodeSnapshotUnavailable, err,
		)
	}
	target, present, err := e.Runtime.TimerResumeTarget(ctx, locked, task, checkpoint)
	if err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, e.failTimerWaitTx(ctx, tx, locked, now, executionErrorCode(err), err)
	}
	if !present {
		_ = resumeTx.Rollback(ctx)
		return nil, e.failTimerWaitTx(
			ctx, tx, locked, now, ErrorCodeExecutionUnrecoverable,
			fmt.Errorf("timer wait node %q has no timeout edge", checkpoint.NodeID),
		)
	}
	if checkpoint.CompletedOutputs == nil {
		checkpoint.CompletedOutputs = make(map[string]json.RawMessage)
	}
	timeoutResult, _ := json.Marshal(map[string]any{
		"status": "timeout", "node_id": checkpoint.NodeID,
	})
	checkpoint.CompletedOutputs[checkpoint.NodeID] = timeoutResult
	checkpoint.NodeID = target
	checkpoint.TeamRunGeneration = resumed.Generation
	checkpoint.ExecutionLeaseEpoch = resumed.ExecutionLeaseEpoch
	checkpoint.WrittenAt = now
	if _, err := e.Checkpoints.PutTx(ctx, resumeTx, checkpoint); err != nil {
		_ = resumeTx.Rollback(ctx)
		return nil, err
	}
	if err := resumeTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit timer resume savepoint: %w", err)
	}
	return &timerResumePlan{
		run: resumed, task: task, checkpoint: checkpoint, executorID: executorID,
	}, nil
}

func checkpointErrorCode(err error) ErrorCode {
	if errors.Is(err, ErrTeamRunIdentityMismatch) {
		return ErrorCodeIdentityMismatch
	}
	return ErrorCodeSnapshotUnavailable
}

func (e *Executor) failTimerWaitTx(
	ctx context.Context,
	tx pgx.Tx,
	run TeamRun,
	now time.Time,
	code ErrorCode,
	cause error,
) error {
	_, err := e.Runs.FailWaitTimeoutTx(ctx, tx, FailWaitTimeoutRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusParked,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExpectedWaitKind:            WaitTimer,
		ErrorCode:                   code,
		Cause:                       cause,
		IdempotencyKey: fmt.Sprintf(
			"teamrun-timer-fail:%s:%d", run.RunID, run.ResumeGeneration,
		),
		Actor:      "teamrun-timer-wake-sweeper",
		Source:     "teamrun_worker",
		OccurredAt: now,
	})
	return err
}

func (e *Executor) executeTimerResume(
	ctx context.Context,
	plan timerResumePlan,
) error {
	result, runErr := e.Runtime.ResumeCheckpoint(
		ctx, plan.run, plan.task, plan.checkpoint,
	)
	if runErr != nil {
		_, err := e.failRunning(
			ctx, plan.run, plan.task, plan.executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason,
		)
		return err
	}
	return e.finishRuntimeResult(
		ctx, plan.run, plan.task, "", plan.executorID, result, false,
	)
}
