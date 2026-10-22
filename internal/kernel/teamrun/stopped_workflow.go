package teamrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// StoppedWorkflowSweeper recovers TeamRuns left running after their source
// task's execution provably stopped: the task's execution lease expired, so it
// failed awaiting outcome reconciliation, and then the worker that held it
// acknowledged the stop, as a forced drain does. Nothing else advances such a
// run. Before the task's deadline the sweeper reopens the same source task, so
// the next claim takes the run over through the ordinary path and resumes its
// checkpoint, keeping finished members and action receipts. Past the deadline
// it fails the run with that cause instead of reviving it. A task whose worker
// has not acknowledged stopping is left alone: lease expiry does not prove the
// execution ended.
type StoppedWorkflowSweeper struct {
	Tasks        *taskqueue.Store
	Transactions TransactionBeginner
	Runs         *PGStore
	Executor     *Executor
	BatchSize    int
	Now          func() time.Time
}

// errStoppedAfterDeadline is the cause recorded on a run whose stopped source
// task could not be resumed before its deadline.
var errStoppedAfterDeadline = errors.New("the source task's execution stopped and its deadline passed before it could resume")

func (s *StoppedWorkflowSweeper) Sweep(ctx context.Context) (int, error) {
	if s == nil || s.Tasks == nil || s.Transactions == nil || s.Runs == nil || s.Executor == nil {
		return 0, errors.New("team run stopped workflow sweeper dependencies are unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	limit := s.BatchSize
	if limit < 1 {
		limit = 32
	}
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin team run stopped workflow sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs, err := s.Runs.ListStoppedSourceRunsTx(ctx, tx, limit)
	if err != nil {
		return 0, err
	}
	type expiredRun struct {
		run  TeamRun
		task *taskqueue.Task
	}
	expired := make([]expiredRun, 0)
	recovered := 0
	for _, run := range runs {
		task, err := s.Tasks.Get(ctx, run.WorkspaceID, run.SourceTaskID)
		if err != nil {
			return 0, fmt.Errorf("read stopped source task of run %q: %w", run.RunID, err)
		}
		if task.DeadlineAt != nil && !task.DeadlineAt.After(now) {
			expired = append(expired, expiredRun{run: run, task: task})
			continue
		}
		// The run row stays locked until commit, so no other sweeper or
		// executor moves it while its task returns to the queue.
		reopened, err := s.Tasks.ReopenStoppedWorkflowTaskTx(ctx, tx, task.WorkspaceID, task.ID, task.ClaimEpoch, task.StoppedWorkerID)
		if err != nil {
			return 0, fmt.Errorf("reopen stopped source task of run %q: %w", run.RunID, err)
		}
		if reopened {
			recovered++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit team run stopped workflow sweep: %w", err)
	}
	for _, item := range expired {
		// FailTx fences on the run's generation, lease epoch and executor, so
		// of concurrent sweepers only one terminates the run.
		executorID := executorIdentity(item.task.ID, item.task.StoppedWorkerID)
		_, err := s.Executor.failRunning(ctx, item.run, item.task, executorID,
			executionError(ErrorCodeExecutionUnrecoverable, errStoppedAfterDeadline),
			loomruntime.UsageTotals{}, nil, false, UsageIncompleteReasonAttemptLost)
		switch {
		case err == nil:
			recovered++
		case errors.Is(err, ErrTeamRunStateConflict), errors.Is(err, ErrTeamRunCancelled),
			errors.Is(err, ErrTeamRunExecutionUnrecoverable):
			// Another sweeper or a cancellation settled the run first.
		default:
			return recovered, fmt.Errorf("fail run %q stopped past its deadline: %w", item.run.RunID, err)
		}
	}
	return recovered, nil
}

// ListStoppedSourceRunsTx locks a bounded batch of running TeamRuns whose
// source workflow task failed awaiting outcome reconciliation and whose stop
// was acknowledged by the very worker the run still names as its executor.
// It starts from running runs, so its cost follows live work.
func (*PGStore) ListStoppedSourceRunsTx(ctx context.Context, tx pgx.Tx, limit int) ([]TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, fmt.Errorf("limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+teamRunColumns+`
		FROM weave_team_runs
		WHERE status='running' AND (workspace_id,run_id) IN (
			SELECT r.workspace_id,r.run_id FROM weave_team_runs r
			JOIN weave_task_queue q ON q.workspace_id=r.workspace_id AND q.id=r.source_task_id
			WHERE r.status='running' AND q.kind='team_workflow' AND q.identity_kind=$1
				AND q.status=$2 AND q.error=$3 AND q.worker_id IS NULL
				AND q.stopped_worker_id IS NOT NULL AND q.stopped_epoch=q.claim_epoch
				AND r.current_executor_id=$4||q.id||':'||q.stopped_worker_id)
		ORDER BY workspace_id,run_id
		FOR UPDATE SKIP LOCKED
		LIMIT $5`,
		taskqueue.IdentityTeamWorkflow, taskqueue.StatusFailed, taskqueue.ExecutionLeaseExpiredError,
		executorIdentityPrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("list team runs with stopped source tasks: %w", err)
	}
	defer rows.Close()
	runs := make([]TeamRun, 0)
	for rows.Next() {
		run, scanErr := scanTeamRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan team run with stopped source task: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list team runs with stopped source tasks rows: %w", err)
	}
	return runs, nil
}
