package teamorch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	ExecutionQueued    = "queued"
	ExecutionRunning   = "running"
	ExecutionSucceeded = "succeeded"
	ExecutionFailed    = "failed"

	retryableExecutionBackoff = 30 * time.Second
)

var (
	ErrExecutionNotRunnable = errors.New("team build run is not executable")
	ErrExecutionLeaseLost   = errors.New("team build execution lease lost")
)

type ExecutionJob struct {
	WorkspaceID string
	BuildRunID  string
	Status      string
	WorkerID    string
	LeaseEpoch  int64
	LeaseUntil  *time.Time
	LastError   string
	RequestedAt time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	UpdatedAt   time.Time
}

type ExecutionSubmission struct {
	WorkspaceID string `json:"workspace_id"`
	BuildRunID  string `json:"build_run_id"`
	Status      string `json:"status"`
}

type ExecutionQueue struct {
	pool  *pgxpool.Pool
	runs  *teambuild.Store
	clock teambuild.Clock
}

func NewExecutionQueue(
	pool *pgxpool.Pool, runs *teambuild.Store, clock teambuild.Clock,
) *ExecutionQueue {
	if clock == nil {
		clock = teambuild.RealClock{}
	}
	return &ExecutionQueue{pool: pool, runs: runs, clock: clock}
}

func (q *ExecutionQueue) Submit(
	ctx context.Context, workspaceID, buildRunID string,
) (ExecutionSubmission, error) {
	if q == nil || q.pool == nil || q.runs == nil {
		return ExecutionSubmission{}, errors.New("team build execution queue unavailable")
	}
	run, err := q.runs.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return ExecutionSubmission{}, err
	}
	switch run.Status {
	case teambuild.StatusAuthorized, teambuild.StatusRoundRunning, teambuild.StatusPublishing:
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled:
		return ExecutionSubmission{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: run.Status}, nil
	default:
		return ExecutionSubmission{}, fmt.Errorf("%w: status %s", ErrExecutionNotRunnable, run.Status)
	}

	now := q.clock.Now().UTC()
	var status string
	err = q.pool.QueryRow(ctx, `
		INSERT INTO weave_team_build_execution_jobs (
			workspace_id, build_run_id, status, requested_at, updated_at
		) VALUES ($1,$2,'queued',$3,$3)
		ON CONFLICT (workspace_id, build_run_id) DO UPDATE SET
			status = CASE
				WHEN weave_team_build_execution_jobs.status = 'failed' THEN 'queued'
				ELSE weave_team_build_execution_jobs.status
			END,
			requested_at = CASE
				WHEN weave_team_build_execution_jobs.status = 'failed' THEN EXCLUDED.requested_at
				ELSE weave_team_build_execution_jobs.requested_at
			END,
			started_at = CASE
				WHEN weave_team_build_execution_jobs.status = 'failed' THEN NULL
				ELSE weave_team_build_execution_jobs.started_at
			END,
			completed_at = CASE
				WHEN weave_team_build_execution_jobs.status = 'failed' THEN NULL
				ELSE weave_team_build_execution_jobs.completed_at
			END,
			last_error = CASE
				WHEN weave_team_build_execution_jobs.status = 'failed' THEN ''
				ELSE weave_team_build_execution_jobs.last_error
			END,
			updated_at = EXCLUDED.updated_at
		RETURNING status
	`, workspaceID, buildRunID, now).Scan(&status)
	if err != nil {
		return ExecutionSubmission{}, fmt.Errorf("submit team build execution: %w", err)
	}
	return ExecutionSubmission{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: status}, nil
}

func (q *ExecutionQueue) Claim(
	ctx context.Context, workerID string, leaseDuration time.Duration,
) (*ExecutionJob, error) {
	if q == nil || q.pool == nil || workerID == "" || leaseDuration <= 0 {
		return nil, errors.New("invalid team build execution claim")
	}
	now := q.clock.Now().UTC()
	leaseUntil := now.Add(leaseDuration)
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin team build execution claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workspaceID, buildRunID string
	err = tx.QueryRow(ctx, `
		SELECT job.workspace_id, job.build_run_id
		FROM weave_team_build_execution_jobs AS job
		JOIN weave_team_build_runs AS run
		  ON run.workspace_id=job.workspace_id AND run.build_run_id=job.build_run_id
		WHERE run.status IN ('authorized','round_running','publishing')
		  AND job.requested_at <= $1
		  AND (
			job.status = 'queued'
			OR (job.status = 'running' AND job.lease_until <= $1)
			OR job.status = 'succeeded'
		  )
		ORDER BY job.requested_at, job.workspace_id, job.build_run_id
		FOR UPDATE OF job SKIP LOCKED
		LIMIT 1
	`, now).Scan(&workspaceID, &buildRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select team build execution claim: %w", err)
	}
	job, err := scanExecutionJob(tx.QueryRow(ctx, `
		UPDATE weave_team_build_execution_jobs
		SET status='running', worker_id=$3, lease_epoch=lease_epoch+1,
			lease_until=$4, started_at=COALESCE(started_at,$5),
			completed_at=NULL, updated_at=$5
		WHERE workspace_id=$1 AND build_run_id=$2
		RETURNING workspace_id, build_run_id, status, worker_id,
			lease_epoch, lease_until, last_error, requested_at,
			started_at, completed_at, updated_at
	`, workspaceID, buildRunID, workerID, leaseUntil, now))
	if err != nil {
		return nil, fmt.Errorf("claim team build execution: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit team build execution claim: %w", err)
	}
	return &job, nil
}

func (q *ExecutionQueue) Renew(
	ctx context.Context, job ExecutionJob, leaseDuration time.Duration,
) error {
	now := q.clock.Now().UTC()
	result, err := q.pool.Exec(ctx, `
		UPDATE weave_team_build_execution_jobs
		SET lease_until=$5, updated_at=$4
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='running'
		  AND worker_id=$3 AND lease_epoch=$6
		  AND lease_until>$4
		  AND EXISTS (
			SELECT 1 FROM weave_team_build_runs AS run
			WHERE run.workspace_id=$1 AND run.build_run_id=$2
			  AND run.status IN ('authorized','round_running','publishing')
		  )
	`, job.WorkspaceID, job.BuildRunID, job.WorkerID, now, now.Add(leaseDuration), job.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("renew team build execution lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrExecutionLeaseLost
	}
	return nil
}

func (q *ExecutionQueue) finish(
	ctx context.Context, job ExecutionJob, status, lastError string,
) error {
	now := q.clock.Now().UTC()
	result, err := q.pool.Exec(ctx, `
		UPDATE weave_team_build_execution_jobs
		SET status=$5, worker_id=NULL, lease_until=NULL,
			completed_at=$4, updated_at=$4, last_error=$6
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='running'
		  AND worker_id=$3 AND lease_epoch=$7
	`, job.WorkspaceID, job.BuildRunID, job.WorkerID, now, status, lastError, job.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("finish team build execution: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrExecutionLeaseLost
	}
	return nil
}

func (q *ExecutionQueue) release(ctx context.Context, job ExecutionJob) error {
	return q.releaseAfter(ctx, job, 0)
}

func (q *ExecutionQueue) releaseAfter(ctx context.Context, job ExecutionJob, delay time.Duration) error {
	now := q.clock.Now().UTC()
	requestedAt := now
	if delay > 0 {
		requestedAt = now.Add(delay)
	}
	result, err := q.pool.Exec(ctx, `
		UPDATE weave_team_build_execution_jobs
		SET status='queued', worker_id=NULL, lease_until=NULL,
			completed_at=NULL, requested_at=$6, updated_at=$4
		WHERE workspace_id=$1 AND build_run_id=$2 AND status='running'
		  AND worker_id=$3 AND lease_epoch=$5
	`, job.WorkspaceID, job.BuildRunID, job.WorkerID, now, job.LeaseEpoch, requestedAt)
	if err != nil {
		return fmt.Errorf("release team build execution: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrExecutionLeaseLost
	}
	return nil
}

type executionJobRow interface{ Scan(...any) error }

func scanExecutionJob(row executionJobRow) (ExecutionJob, error) {
	var job ExecutionJob
	err := row.Scan(
		&job.WorkspaceID, &job.BuildRunID, &job.Status, &job.WorkerID,
		&job.LeaseEpoch, &job.LeaseUntil, &job.LastError, &job.RequestedAt,
		&job.StartedAt, &job.CompletedAt, &job.UpdatedAt,
	)
	return job, err
}

// AsyncService separates durable submission from execution. The HTTP request
// only calls Submit; a background worker owns the long-lived execution
// context. Lease time detects a missing executor and never limits task length.
type AsyncService struct {
	queue        *ExecutionQueue
	executor     *Service
	lease        time.Duration
	pollInterval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewAsyncService(queue *ExecutionQueue, executor *Service) *AsyncService {
	return &AsyncService{
		queue: queue, executor: executor,
		lease: 30 * time.Second, pollInterval: time.Second,
	}
}

func (s *AsyncService) Submit(
	ctx context.Context, workspaceID, buildRunID string,
) (ExecutionSubmission, error) {
	return s.queue.Submit(ctx, workspaceID, buildRunID)
}

func (s *AsyncService) Start() {
	if s == nil || s.queue == nil || s.executor == nil {
		return
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx, "team-build:"+uuid.NewString())
	}()
}

func (s *AsyncService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
}

func (s *AsyncService) loop(ctx context.Context, workerID string) {
	for {
		job, err := s.queue.Claim(ctx, workerID, s.lease)
		if err != nil && ctx.Err() == nil {
			slog.Error("team build execution claim failed", "error", err)
		}
		if job != nil {
			s.execute(ctx, *job)
			continue
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *AsyncService) execute(workerCtx context.Context, job ExecutionJob) {
	ctx, cancel := context.WithCancel(workerCtx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(s.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.queue.Renew(ctx, job, s.lease); err != nil {
					slog.Error("team build execution heartbeat failed", "build_run_id", job.BuildRunID, "error", err)
					cancel()
					return
				}
			}
		}
	}()
	result, execErr := s.executor.Execute(ctx, job.WorkspaceID, job.BuildRunID)
	cancel()
	<-heartbeatDone
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finishCancel()
	if workerCtx.Err() != nil || errors.Is(execErr, context.Canceled) ||
		errors.Is(execErr, context.DeadlineExceeded) {
		if err := s.queue.release(finishCtx, job); err != nil && !errors.Is(err, ErrExecutionLeaseLost) {
			slog.Error("release interrupted team build execution failed", "build_run_id", job.BuildRunID, "error", err)
		}
		return
	}
	if execErr != nil {
		_, err := s.blockRunAfterExecutionFailure(finishCtx, job, execErr)
		if err != nil {
			slog.Error("block failed team build run failed", "build_run_id", job.BuildRunID, "error", err)
		}
		if err := s.queue.finish(finishCtx, job, ExecutionFailed, execErr.Error()); err != nil && !errors.Is(err, ErrExecutionLeaseLost) {
			slog.Error("persist failed team build execution failed", "build_run_id", job.BuildRunID, "error", err)
		}
		return
	}
	if executionResultStillActive(result) {
		backoff := time.Duration(0)
		if strings.TrimSpace(result.StopReason) != "" {
			backoff = retryableExecutionBackoff
		}
		if err := s.queue.releaseAfter(finishCtx, job, backoff); err != nil && !errors.Is(err, ErrExecutionLeaseLost) {
			slog.Error("requeue still-active team build execution failed", "build_run_id", job.BuildRunID, "status", result.Status, "error", err)
		}
		return
	}
	if err := s.queue.finish(finishCtx, job, ExecutionSucceeded, ""); err != nil && !errors.Is(err, ErrExecutionLeaseLost) {
		slog.Error("persist completed team build execution failed", "build_run_id", job.BuildRunID, "error", err)
	}
}

func executionResultStillActive(result Result) bool {
	switch result.Status {
	case teambuild.StatusAuthorized, teambuild.StatusRoundRunning, teambuild.StatusPublishing:
		return true
	default:
		return false
	}
}

func (s *AsyncService) blockRunAfterExecutionFailure(ctx context.Context, job ExecutionJob, execErr error) (*teambuild.TeamBuildRun, error) {
	if s == nil || s.queue == nil || s.queue.runs == nil || execErr == nil {
		return nil, nil
	}
	run, err := s.queue.runs.GetBuildRun(ctx, job.WorkspaceID, job.BuildRunID)
	if err != nil {
		return nil, err
	}
	switch run.Status {
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled:
		return nil, nil
	case teambuild.StatusAuthorized, teambuild.StatusRoundRunning, teambuild.StatusPublishing:
	default:
		return nil, nil
	}
	reason := "execution_failed:" + classifyExecutionError(execErr)
	if detail := strings.TrimSpace(execErr.Error()); detail != "" {
		reason += ": " + detail
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	blocked, err := s.queue.runs.TransitionStatus(
		ctx,
		job.WorkspaceID,
		job.BuildRunID,
		run.Status,
		teambuild.StatusBlocked,
		"team-build-worker",
		reason,
	)
	if err != nil {
		return nil, err
	}
	return &blocked, nil
}

func classifyExecutionError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, teambuild.ErrCompilerRevisionRequired):
		return "blueprint_required"
	default:
		return "infra"
	}
}
