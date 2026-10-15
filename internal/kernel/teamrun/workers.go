package teamrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type CancelGraceSweeper struct {
	Tasks        *taskqueue.Store
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
	runs, err := s.Runs.listCancelRequestsTx(ctx, tx, now, limit, s.Tasks == nil)
	if err != nil {
		return 0, err
	}
	settled := 0
	rootStopAcks := make([]cancelledRootStopAck, 0, len(runs))
	for _, run := range runs {
		if s.Tasks != nil {
			// Reapply the durable stop request after a crash between registration
			// and task cancellation, while retaining the parent lock.
			if _, err := s.Tasks.CancelRunTasksTx(ctx, tx, run.WorkspaceID, run.RunSnapshotID); err != nil {
				return 0, err
			}
			stopped, err := s.Tasks.RunExecutionsStoppedTx(ctx, tx, run.WorkspaceID, run.RunSnapshotID)
			if err != nil {
				return 0, err
			}
			if stopped {
				if _, err := s.Runs.ConfirmCancelTx(ctx, tx, ConfirmCancelRequest{
					WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedStatus: run.Status,
					ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
					ExpectedResumeGeneration: run.ResumeGeneration, IdempotencyKey: "teamrun-cancel-ack:" + run.RunID,
					Actor: "teamrun-cancel-grace-sweeper", Source: "teamrun_worker", OccurredAt: now,
				}); err != nil {
					return 0, err
				}
				settled++
				continue
			}
			proof, err := stoppedCancelledRootAttemptTx(ctx, tx, run)
			if err != nil {
				return 0, fmt.Errorf("inspect cancelled root attempt %q: %w", run.RunID, err)
			}
			if proof && run.SourceTaskID != "" {
				rootStopAcks = append(rootStopAcks, cancelledRootStopAck{
					WorkspaceID: run.WorkspaceID,
					TaskID:      run.SourceTaskID,
					SnapshotID:  run.RunSnapshotID,
				})
				continue
			}
		}
		if run.CancelGraceDeadlineAt == nil || now.Before(*run.CancelGraceDeadlineAt) {
			continue
		}
		settled++
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
	for _, ack := range rootStopAcks {
		task, err := s.Tasks.Get(ctx, ack.WorkspaceID, ack.TaskID)
		if err != nil {
			return settled, fmt.Errorf("read cancelled root task %q: %w", ack.TaskID, err)
		}
		if task == nil || task.Kind != "team_workflow" ||
			task.IdentityKind != taskqueue.IdentityTeamWorkflow ||
			task.RunSnapshotID != ack.SnapshotID || task.WorkerID == "" ||
			task.ClaimEpoch < 1 || task.Status != taskqueue.StatusCancelRequested {
			continue
		}
		if _, err := s.Tasks.AcknowledgeCancelledWorkflowStoppedAtEpoch(
			ctx, ack.WorkspaceID, task.ID, task.WorkerID, task.ClaimEpoch,
		); err != nil {
			return settled, fmt.Errorf("acknowledge terminal cancelled root task %q: %w", task.ID, err)
		}
	}
	return settled, nil
}

type cancelledRootStopAck struct {
	WorkspaceID string
	TaskID      string
	SnapshotID  string
}

// stoppedCancelledRootAttemptTx treats the existing root attempt terminal
// marker as a durable proof that the local workflow execution joined before a
// crash between attempt settlement and task acknowledgement. The next normal
// cancel sweep still owns TeamRun confirmation.
func stoppedCancelledRootAttemptTx(ctx context.Context, tx pgx.Tx, run TeamRun) (bool, error) {
	if run.Status != StatusCancelRequested || run.WorkspaceID == "" || run.RunID == "" ||
		run.RunSnapshotID == "" || run.ExecutionLeaseEpoch < 1 {
		return false, nil
	}
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil || !leasePresent {
		return false, err
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil || !markerPresent {
		return false, err
	}
	if err := loomruntime.ValidateRunAttemptLease(lease); err != nil {
		return false, fmt.Errorf("validate root attempt lease: %w", err)
	}
	if err := loomruntime.ValidateTerminalMarkerV1(marker); err != nil {
		return false, fmt.Errorf("validate root terminal marker: %w", err)
	}
	if lease.State != loomruntime.AttemptLeaseClosed ||
		lease.WorkspaceID != run.WorkspaceID || lease.RunID != run.RunID ||
		lease.AttemptGeneration != int64(run.ExecutionLeaseEpoch) ||
		lease.AttemptID != frozenAttemptID(run) || lease.GraphName != frozenGraphName(run) ||
		marker.WorkspaceID != run.WorkspaceID || marker.RunID != run.RunID ||
		marker.AttemptGeneration != lease.AttemptGeneration || marker.AttemptID != lease.AttemptID ||
		marker.Agent != lease.GraphName || marker.RunStartedAt != lease.RunStartedAt ||
		marker.AttributionScope != loomruntime.TerminalAttributionFixedWorkflow ||
		marker.TeamID == nil || *marker.TeamID != run.TeamID ||
		marker.WorkflowID == nil || *marker.WorkflowID != run.WorkflowID ||
		marker.WorkflowVersion == nil || int(*marker.WorkflowVersion) != run.WorkflowVersion ||
		marker.Phase != loomruntime.TerminalMarkerPhaseFinal ||
		marker.Status != loomruntime.TerminalMarkerStatusFailed ||
		marker.StopReason != teamRunCancelledTerminalStopReason ||
		marker.Source != loomruntime.TerminalMarkerSourceNormal ||
		marker.EvidenceKind != loomruntime.TerminalMarkerEvidenceRunResult ||
		marker.AuditState != loomruntime.TerminalMarkerAuditMaterialized ||
		marker.AuditSchemaVersion == nil || *marker.AuditSchemaVersion != 3 ||
		marker.RunSnapshotID == nil || *marker.RunSnapshotID != run.RunSnapshotID {
		return false, nil
	}
	var auditRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT value FROM loom_store
		WHERE namespace=$1 AND key=$2
	`, "audit:"+run.WorkspaceID, run.RunID).Scan(&auditRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read root terminal audit: %w", err)
	}
	inspection := loomruntime.InspectTerminalRecord(true, auditRaw)
	if inspection.Classification != loomruntime.TerminalRecordValid || inspection.Entry == nil || inspection.Err != nil {
		return false, fmt.Errorf("root cancellation audit is not a valid terminal record: %v", inspection.Err)
	}
	audit := inspection.Entry
	if audit.SchemaVersion != 3 || audit.Tenant != run.WorkspaceID || audit.RunID != run.RunID ||
		audit.AttributionScope != loomruntime.TerminalAttributionFixedWorkflow ||
		audit.TeamID == nil || *audit.TeamID != run.TeamID ||
		audit.WorkflowID == nil || *audit.WorkflowID != run.WorkflowID ||
		audit.WorkflowVersion == nil || *audit.WorkflowVersion != run.WorkflowVersion ||
		audit.RunSnapshotID == nil || *audit.RunSnapshotID != run.RunSnapshotID ||
		audit.Status != "failed" || audit.StopReason != teamRunCancelledTerminalStopReason {
		return false, nil
	}
	return true, nil
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
	Tasks        interface {
		CancelRunTasks(context.Context, string, string) (int, error)
	}
	Now func() time.Time
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
	// A stop request is idempotent at the public boundary. Once the run is
	// terminal there is no work left to cancel, and a retry while cancellation
	// is already registered should return the observed state rather than turn a
	// harmless page retry into a conflict.
	if run.Status.Terminal() {
		if err := tx.Commit(ctx); err != nil {
			return TeamRun{}, fmt.Errorf("commit terminal team run cancel request: %w", err)
		}
		if s.Tasks != nil {
			if _, err := s.Tasks.CancelRunTasks(ctx, run.WorkspaceID, run.RunSnapshotID); err != nil {
				return TeamRun{}, fmt.Errorf("cancel terminal team run child tasks: %w", err)
			}
		}
		return run, nil
	}
	if run.Status == StatusCancelRequested {
		if run.CancelIdempotencyKey != nil && *run.CancelIdempotencyKey == request.IdempotencyKey &&
			run.CancelReason != nil && *run.CancelReason == request.CancelReason {
			if err := tx.Commit(ctx); err != nil {
				return TeamRun{}, fmt.Errorf("commit repeated team run cancel request: %w", err)
			}
			if s.Tasks != nil {
				if _, err := s.Tasks.CancelRunTasks(ctx, run.WorkspaceID, run.RunSnapshotID); err != nil {
					return TeamRun{}, fmt.Errorf("cancel repeated team run child tasks: %w", err)
				}
			}
			return run, nil
		}
		return TeamRun{}, fmt.Errorf("%w: team run cancellation is already in progress with different facts", ErrTeamRunStateConflict)
	}
	var cancelled TeamRun
	if run.Status == StatusQueued {
		cancelled, err = s.Runs.CancelQueuedTx(ctx, tx, CancelQueuedRequest{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID,
			ExpectedStatus:              run.Status,
			ExpectedTeamRunGeneration:   run.Generation,
			ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
			ExpectedResumeGeneration:    run.ResumeGeneration,
			IdempotencyKey:              request.IdempotencyKey,
			Actor:                       request.CancelActor, Source: "teamrun_public_cancel", OccurredAt: now,
		})
	} else {
		cancelled, err = s.Runs.RequestCancelTx(ctx, tx, RequestCancelRequest{
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
			Source:                      "teamrun_public_cancel",
			OccurredAt:                  now,
		})
	}
	if err != nil {
		return TeamRun{}, err
	}
	transactional, atomicCancel := s.Tasks.(interface {
		CancelRunTasksTx(context.Context, pgx.Tx, string, string) (int, error)
	})
	if atomicCancel {
		if _, err := transactional.CancelRunTasksTx(ctx, tx, cancelled.WorkspaceID, cancelled.RunSnapshotID); err != nil {
			return TeamRun{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run cancel request: %w", err)
	}
	if s.Tasks != nil && !atomicCancel {
		if _, err := s.Tasks.CancelRunTasks(ctx, cancelled.WorkspaceID, cancelled.RunSnapshotID); err != nil {
			return TeamRun{}, fmt.Errorf("cancel team run child tasks: %w", err)
		}
	}
	return cancelled, nil
}

type Workers struct {
	Executor     *Executor
	CancelGrace  *CancelGraceSweeper
	TimerWake    *TimerWakeSweeper
	HumanTimeout *HumanTimeoutSweeper
	PollInterval time.Duration
	// ExecutorConcurrency bounds independent workflow/fanout claims. A single
	// executor loop makes a Parallel node observationally serial even when its
	// branches target different runtimes.
	ExecutorConcurrency int

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

	executorConcurrency := workers.ExecutorConcurrency
	if executorConcurrency <= 0 {
		executorConcurrency = 1
	}
	workerCount := executorConcurrency + 2
	if workers.HumanTimeout != nil {
		workerCount++
	}
	workers.wg.Add(workerCount)
	for index := 0; index < executorConcurrency; index++ {
		go func(index int) {
			defer workers.wg.Done()
			workers.executorLoop(ctx, index)
		}(index)
	}
	go func() {
		defer workers.wg.Done()
		workers.cancelGraceLoop(ctx)
	}()
	go func() {
		defer workers.wg.Done()
		workers.timerWakeLoop(ctx)
	}()
	if workers.HumanTimeout != nil {
		go func() {
			defer workers.wg.Done()
			workers.humanTimeoutLoop(ctx)
		}()
	}
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

func (workers *Workers) executorLoop(ctx context.Context, index int) {
	workerID := "teamrun:" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":" + strconv.Itoa(index)
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

func (workers *Workers) humanTimeoutLoop(ctx context.Context) {
	for {
		if _, err := workers.HumanTimeout.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("team run human timeout iteration failed", "error", err)
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
