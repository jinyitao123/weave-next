package teamrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

type CancelGraceSweeper struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	BatchSize    int
	Now          func() time.Time
}

type TimerWakeSweeper struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Executor     *Executor
	BatchSize    int
	Now          func() time.Time
}

func (s *TimerWakeSweeper) Sweep(ctx context.Context) (int, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil || s.Executor == nil {
		return 0, errors.New("team run timer wake sweeper dependencies are unavailable")
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
		return 0, fmt.Errorf("begin team run timer wake sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs, err := s.Runs.ListTimerWakeExpiredTx(ctx, tx, now, limit)
	if err != nil {
		return 0, err
	}
	计划 := make([]timerResumePlan, 0, len(runs))
	for _, run := range runs {
		plan, err := s.Executor.prepareTimerWakeTx(ctx, tx, run, now)
		if err != nil {
			return 0, fmt.Errorf("wake expired timer run %q: %w", run.RunID, err)
		}
		if plan != nil {
			计划 = append(计划, *plan)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit team run timer wake sweep: %w", err)
	}
	for _, plan := range 计划 {
		if err := s.Executor.executeTimerResume(ctx, plan); err != nil {
			return 0, fmt.Errorf("execute resumed timer run %q: %w", plan.run.RunID, err)
		}
	}
	return len(runs), nil
}

func (s *CancelGraceSweeper) Sweep(ctx context.Context) (int, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil {
		return 0, errors.New("team run cancel grace sweeper dependencies are unavailable")
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
		return 0, fmt.Errorf("begin team run cancel grace sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs, err := s.Runs.ListCancelGraceExpiredTx(ctx, tx, now, limit)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		deadline := ""
		if run.CancelGraceDeadlineAt != nil {
			deadline = run.CancelGraceDeadlineAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := s.Runs.AbandonCancelGraceTx(
			ctx,
			tx,
			AbandonCancelGraceRequest{
				WorkspaceID:                 run.WorkspaceID,
				RunID:                       run.RunID,
				ExpectedStatus:              StatusCancelRequested,
				ExpectedTeamRunGeneration:   run.Generation,
				ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
				ExpectedResumeGeneration:    run.ResumeGeneration,
				IdempotencyKey:              "teamrun-cancel-grace:" + run.RunID + ":" + deadline,
				Actor:                       "teamrun-cancel-grace-sweeper",
				Source:                      "teamrun_worker",
				OccurredAt:                  now,
			},
		); err != nil {
			return 0, fmt.Errorf("abandon expired team run %q: %w", run.RunID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit team run cancel grace sweep: %w", err)
	}
	return len(runs), nil
}

type CancelRequest struct {
	WorkspaceID    string
	RunID          string
	CancelActor    string
	CancelReason   string
	GraceDeadline  time.Time
	IdempotencyKey string
}

type CancelService struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Now          func() time.Time
}

// RequestCancel is the narrow internal registration boundary for running or
// parked TeamRuns. It does not expose admission or session lease behavior.
func (s *CancelService) RequestCancel(
	ctx context.Context,
	request CancelRequest,
) (TeamRun, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil {
		return TeamRun{}, errors.New("team run cancel service dependencies are unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run cancel request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := s.Runs.GetForUpdateTx(
		ctx, tx, request.WorkspaceID, request.RunID,
	)
	if err != nil {
		return TeamRun{}, err
	}
	cancelled, err := s.Runs.RequestCancelTx(ctx, tx, RequestCancelRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              run.Status,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		CancelActor:                 request.CancelActor,
		CancelReason:                request.CancelReason,
		GraceDeadlineAt:             request.GraceDeadline,
		IdempotencyKey:              request.IdempotencyKey,
		Actor:                       request.CancelActor,
		Source:                      "teamrun_internal_cancel",
		OccurredAt:                  now,
	})
	if err != nil {
		return TeamRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run cancel request: %w", err)
	}
	return cancelled, nil
}

type Workers struct {
	Executor     *Executor
	CancelGrace  *CancelGraceSweeper
	TimerWake    *TimerWakeSweeper
	PollInterval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (workers *Workers) Start() {
	if workers == nil || workers.Executor == nil || workers.CancelGrace == nil ||
		workers.TimerWake == nil {
		return
	}
	workers.mu.Lock()
	if workers.cancel != nil {
		workers.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	workers.cancel = cancel
	workers.mu.Unlock()

	workers.wg.Add(3)
	go func() {
		defer workers.wg.Done()
		workers.executorLoop(ctx)
	}()
	go func() {
		defer workers.wg.Done()
		workers.cancelGraceLoop(ctx)
	}()
	go func() {
		defer workers.wg.Done()
		workers.timerWakeLoop(ctx)
	}()
	slog.Info("team run background workers started")
}

func (workers *Workers) Stop() {
	if workers == nil {
		return
	}
	workers.mu.Lock()
	cancel := workers.cancel
	workers.cancel = nil
	workers.mu.Unlock()
	if cancel != nil {
		cancel()
		workers.wg.Wait()
		slog.Info("team run background workers stopped")
	}
}

func (workers *Workers) executorLoop(ctx context.Context) {
	workerID := "teamrun:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	for {
		processed, err := workers.Executor.ProcessNext(ctx, workerID)
		if err != nil && ctx.Err() == nil {
			slog.Error("team run executor iteration failed", "error", err)
		}
		if processed {
			continue
		}
		if !workers.wait(ctx) {
			return
		}
	}
}

func (workers *Workers) cancelGraceLoop(ctx context.Context) {
	for {
		if _, err := workers.CancelGrace.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("team run cancel grace iteration failed", "error", err)
		}
		if !workers.wait(ctx) {
			return
		}
	}
}

func (workers *Workers) timerWakeLoop(ctx context.Context) {
	for {
		if _, err := workers.TimerWake.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("team run timer wake iteration failed", "error", err)
		}
		if !workers.wait(ctx) {
			return
		}
	}
}

func (workers *Workers) wait(ctx context.Context) bool {
	interval := workers.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
