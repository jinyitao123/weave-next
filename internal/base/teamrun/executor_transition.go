package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

type ExecutionError struct {
	Code  ErrorCode
	Cause error
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func executionError(code ErrorCode, cause error) error {
	return &ExecutionError{Code: code, Cause: cause}
}

type Executor struct {
	Tasks             ExecutorTaskStore
	Consumer          *Consumer
	Transactions      TransactionBeginner
	Runs              ExecutorRunStore
	Checkpoints       ExecutorCheckpointStore
	Runtime           RuntimeRunner
	Fanout            ExecutorFanout
	Corrections       *CorrectionStore
	RuntimeRecords    loomruntime.TerminalRecordStore
	Now               func() time.Time
	ResumeTokenHash   func() ([]byte, error)
	HeartbeatInterval time.Duration
	// ClaimWorkspaceID and ClaimRunSnapshotID optionally restrict ProcessNext
	// to one admitted run. Ordinary daemon workers leave them empty and keep
	// claiming the shared queue; synchronous candidate drivers set both so a
	// healthy build cannot be hijacked by another conversation's task.
	ClaimWorkspaceID   string
	ClaimRunSnapshotID string
}

func executorIdentity(taskID, workerID string) string {
	return "teamrun-executor:" + taskID + ":" + workerID
}

func executorClaimKey(taskID string) string {
	return "teamrun-claim:" + taskID
}

func executorTerminalKey(taskID string, status Status) string {
	return "teamrun-terminal:" + string(status) + ":" + taskID
}

func executorParkKey(taskID string, resumeGeneration ResumeGeneration) string {
	return fmt.Sprintf("teamrun-park:%s:%d", taskID, resumeGeneration)
}

func executorReclaimKey(taskID string, epoch ExecutionLeaseEpoch) string {
	return fmt.Sprintf("teamrun-reclaim:%s:%d", taskID, epoch)
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Executor) claimRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
) (TeamRun, error) {
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	claimed, err := e.Runs.ClaimRunningTx(ctx, tx, ClaimRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusQueued,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorClaimKey(task.ID),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("claim running team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run claim: %w", err)
	}
	if _, err := e.admitFrozenAttempt(ctx, claimed); err != nil {
		return TeamRun{}, err
	}
	return claimed, nil
}

var errTaskLeaseLost = errors.New("team workflow task lease lost")

func (e *Executor) executeWithHeartbeat(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	execute func(context.Context) (RuntimeResult, error),
) (RuntimeResult, error) {
	interval := e.HeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result RuntimeResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := execute(execCtx)
		outcomes <- outcome{result: result, err: err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return RuntimeResult{}, ctx.Err()
		case <-ticker.C:
			if err := e.Tasks.Heartbeat(ctx, task.ID, workerID); err != nil {
				cancel()
				return RuntimeResult{}, fmt.Errorf("%w: %v", errTaskLeaseLost, err)
			}
		case outcome := <-outcomes:
			return outcome.result, outcome.err
		}
	}
}

func (e *Executor) reclaimRunning(
	ctx context.Context,
	observed TeamRun,
	task *taskqueue.Task,
	executorID string,
) (TeamRun, *WorkflowCheckpointV1, bool, error) {
	now := e.now()
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("begin team run reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := e.Runs.GetForUpdateTx(ctx, tx, observed.WorkspaceID, observed.RunID)
	if err != nil {
		return TeamRun{}, nil, false, err
	}
	if run.Status != StatusRunning {
		return TeamRun{}, nil, false, ErrTeamRunStateConflict
	}
	stateStore := loomruntime.NewPGTerminalStateStore()
	if err := stateStore.LockTerminalRun(ctx, tx, run.WorkspaceID, run.RunID); err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("lock frozen terminal run: %w", err)
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("read frozen terminal marker: %w", err)
	}
	currentLease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("read frozen attempt lease: %w", err)
	}
	if markerPresent && marker.Phase == loomruntime.TerminalMarkerPhaseFinal {
		if !leasePresent {
			return TeamRun{}, nil, false, errors.New("frozen final terminal lease is missing")
		}
		terminal, err := e.replayFrozenTerminalTx(
			ctx, tx, run, task, marker, currentLease,
		)
		if err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("replay frozen terminal: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("commit frozen terminal replay: %w", err)
		}
		return terminal, nil, true, nil
	}

	checkpoint, recoverableErr := e.Checkpoints.GetTx(
		ctx, tx, run.WorkspaceID, run.RunID,
	)
	checkpointPresent := recoverableErr == nil
	if !leasePresent && errors.Is(recoverableErr, ErrWorkflowCheckpointMissing) {
		recoverableErr = nil
	}
	if recoverableErr == nil && checkpointPresent {
		recoverableErr = ValidateCheckpointRun(checkpoint, run)
	}
	if recoverableErr == nil && checkpointPresent {
		typedSnapshot, snapshotErr := e.Consumer.Snapshots.GetByRunID(
			ctx, run.WorkspaceID, run.RunSnapshotID,
		)
		if snapshotErr != nil {
			recoverableErr = fmt.Errorf("read reclaim snapshot: %w", snapshotErr)
		} else {
			recoverableErr = ValidateCheckpointSnapshot(checkpoint, typedSnapshot)
		}
	}
	if recoverableErr != nil {
		abandoned, abandonErr := e.Runs.AbandonUnrecoverableTx(
			ctx, tx, AbandonUnrecoverableRequest{
				WorkspaceID:                 run.WorkspaceID,
				RunID:                       run.RunID,
				ExpectedStatus:              StatusRunning,
				ExpectedTeamRunGeneration:   run.Generation,
				ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
				ExpectedResumeGeneration:    run.ResumeGeneration,
				Cause:                       recoverableErr,
				IdempotencyKey:              executorReclaimKey(task.ID, run.ExecutionLeaseEpoch),
				Actor:                       executorID,
				Source:                      consumerSource,
				OccurredAt:                  now,
			},
		)
		if abandonErr != nil {
			return TeamRun{}, nil, false, fmt.Errorf("abandon unrecoverable team run: %w", abandonErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf("commit team run abandon: %w", err)
		}
		return abandoned, nil, true, nil
	}

	reclaimed, err := e.Runs.ReclaimRunningTx(ctx, tx, ReclaimRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorReclaimKey(task.ID, run.ExecutionLeaseEpoch),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  now,
	})
	if err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("reclaim team run: %w", err)
	}
	if leasePresent {
		if _, err := advanceFrozenAttemptTx(
			ctx, tx, currentLease, reclaimed,
		); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf(
				"advance reclaimed frozen attempt: %w", err,
			)
		}
	} else {
		records, err := e.runtimeRecordStore()
		if err != nil {
			return TeamRun{}, nil, false, err
		}
		if _, err := loomruntime.AdmitFrozenAttemptTx(
			ctx, records, tx, frozenAttemptAdmission(reclaimed),
		); err != nil {
			return TeamRun{}, nil, false, fmt.Errorf(
				"admit reclaimed frozen attempt: %w", err,
			)
		}
	}
	if checkpointPresent {
		checkpoint.TeamRunGeneration = reclaimed.Generation
		checkpoint.ExecutionLeaseEpoch = reclaimed.ExecutionLeaseEpoch
		checkpoint.WrittenAt = now
		if _, err := e.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
			return TeamRun{}, nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, nil, false, fmt.Errorf("commit team run reclaim: %w", err)
	}
	if !checkpointPresent {
		return reclaimed, nil, false, nil
	}
	return reclaimed, &checkpoint, false, nil
}

func executionErrorCode(err error) ErrorCode {
	var classified *ExecutionError
	if errors.As(err, &classified) && ValidateErrorCode(classified.Code) {
		return classified.Code
	}
	return ErrorCodeRuntimeIncompatible
}

func (e *Executor) succeedRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
	usage loomruntime.UsageTotals,
	usageCoverage *loomruntime.UsageCoverage,
	usageComplete bool,
	usageIncompleteReason string,
) (TeamRun, error) {
	if err := e.commitFrozenNormalTerminal(
		ctx, run, "success", "completed", usage, usageCoverage,
		usageComplete, usageIncompleteReason,
	); err != nil {
		return TeamRun{}, err
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run success: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	succeeded, err := e.Runs.SucceedTx(ctx, tx, SucceedRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		IdempotencyKey:              executorTerminalKey(task.ID, StatusSucceeded),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("succeed team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run success: %w", err)
	}
	return succeeded, nil
}

func (e *Executor) failRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
	runErr error,
	usage loomruntime.UsageTotals,
	usageCoverage *loomruntime.UsageCoverage,
	usageComplete bool,
	usageIncompleteReason string,
) (TeamRun, error) {
	code := ErrorCodeExecutionUnrecoverable
	var classified *ExecutionError
	if errors.As(runErr, &classified) && ValidateErrorCode(classified.Code) &&
		classified.Code != ErrorCodeCancelled {
		code = classified.Code
	}
	if err := e.commitFrozenNormalTerminal(
		ctx, run, "failed", string(code), usage, usageCoverage,
		usageComplete, usageIncompleteReason,
	); err != nil {
		return TeamRun{}, err
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	failed, err := e.Runs.FailTx(ctx, tx, FailRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		ErrorCode:                   code,
		Cause:                       runErr,
		IdempotencyKey:              executorTerminalKey(task.ID, StatusFailed),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  e.now(),
	})
	if err != nil {
		return TeamRun{}, fmt.Errorf("fail team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run failure: %w", err)
	}
	return failed, nil
}

func (e *Executor) finishTerminalTask(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	run TeamRun,
	output json.RawMessage,
) error {
	switch run.Status {
	case StatusSucceeded:
		if len(output) == 0 {
			output = json.RawMessage(`{"status":"succeeded"}`)
		}
		return e.Tasks.CompleteClaimed(ctx, task.ID, workerID, output, run.RunID)
	case StatusFailed, StatusCancelled, StatusAbandoned:
		code := ErrorCodeStateConflict
		if run.ErrorCode != nil {
			code = *run.ErrorCode
		}
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(code))
	default:
		return fmt.Errorf("team run %q is not terminal", run.RunID)
	}
}
