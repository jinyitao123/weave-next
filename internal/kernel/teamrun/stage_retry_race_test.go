package teamrun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type retryTransactionHook struct {
	TransactionBeginner
	before func()
}

func (h retryTransactionHook) Begin(ctx context.Context) (pgx.Tx, error) {
	h.before()
	return h.TransactionBeginner.Begin(ctx)
}

type retryBranchHook struct {
	failedTaskRequeuer
	before func(context.Context) error
	fail   bool
}

func (h retryBranchHook) RequeueFailedTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, id, snapshotID, groupID string) (*taskqueue.Task, error) {
	if h.before != nil {
		if err := h.before(ctx); err != nil {
			return nil, err
		}
	}
	task, err := h.failedTaskRequeuer.RequeueFailedTaskTx(ctx, tx, workspaceID, id, snapshotID, groupID)
	if err == nil && h.fail {
		return nil, errors.New("requeue result unavailable")
	}
	return task, err
}

func retryStopRequest(h *processNextHarness, run TeamRun) CancelRequest {
	return CancelRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, CancelActor: "user", CancelReason: "stop",
		GraceDeadline: h.now.Add(2 * time.Minute), IdempotencyKey: "stop"}
}

func TestStageRetryFanoutStopWinsAfterInitialRead(t *testing.T) {
	h, run, taskID := seedFanoutStageRetry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := &CancelService{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Minute) }}
	var stopErr error
	s := &StageRetryService{Runs: &PGStore{Transactions: h.pool}, Tasks: h.tasks,
		Transactions: retryTransactionHook{TransactionBeginner: h.pool, before: func() {
			_, stopErr = stop.RequestCancel(ctx, retryStopRequest(h, run))
		}}}
	_, err := s.Retry(ctx, StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics"})
	if stopErr != nil || !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("retry raced past stop: stop=%v retry=%v", stopErr, err)
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", "request timed out")
	if h.readRun(t, run.RunID).Status != StatusCancelRequested {
		t.Fatal("parent stop was lost")
	}
}

func TestStageRetryFanoutStopCancelsBranchRequeuedConcurrently(t *testing.T) {
	h, run, taskID := seedFanoutStageRetry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	branchReady, stopStarted := make(chan struct{}), make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		select {
		case <-branchReady:
		case <-ctx.Done():
			stopped <- ctx.Err()
			return
		}
		stop := &CancelService{Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Minute) },
			Transactions: retryTransactionHook{TransactionBeginner: h.pool, before: func() { close(stopStarted) }}}
		_, err := stop.RequestCancel(ctx, retryStopRequest(h, run))
		stopped <- err
	}()
	s := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool},
		Tasks: retryBranchHook{failedTaskRequeuer: h.tasks, before: func(ctx context.Context) error {
			close(branchReady)
			select {
			case <-stopStarted:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}}
	if _, err := s.Retry(ctx, StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics"}); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	h.assertTask(t, taskID, taskqueue.StatusCancelled, "", "")
	if h.readRun(t, run.RunID).Status != StatusCancelRequested {
		t.Fatal("parent stop was lost")
	}
}

func TestStageRetryFanoutRequeueRollsBackWithOneConnection(t *testing.T) {
	h, run, taskID := seedFanoutStageRetry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := h.pool.Config()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tasks := taskqueue.New(pool, fixedTaskClock{now: h.now}, time.Minute)
	s := &StageRetryService{Transactions: pool, Runs: &PGStore{Transactions: pool},
		Tasks: retryBranchHook{failedTaskRequeuer: tasks, fail: true}}
	if _, err := s.Retry(ctx, StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics"}); err == nil || ctx.Err() != nil {
		t.Fatalf("requeue did not fail atomically on one connection: err=%v context=%v", err, ctx.Err())
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", "request timed out")
}
