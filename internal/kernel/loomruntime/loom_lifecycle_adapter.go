package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/loom"
)

type preparedCheckpointObserver struct {
	prepared PreparedRun
}

func (observer preparedCheckpointObserver) ObserveCheckpoint(
	ctx context.Context,
	event loom.CheckpointEvent,
) error {
	var stage RunLifecycleStage
	switch event.Stage {
	case loom.CheckpointLatestPutBefore:
		stage = RunLifecycleStageLatestCheckpointPutBefore
	case loom.CheckpointLatestPutAfter:
		stage = RunLifecycleStageLatestCheckpointPutAfter
	default:
		return fmt.Errorf("unsupported Loom checkpoint stage %q", event.Stage)
	}
	return observer.prepared.runLifecycleHook(ctx, stage)
}

type preparedRunTerminalizer struct {
	prepared      PreparedRun
	startedAt     time.Time
	attribution   TerminalAttribution
	coordinator   NormalTerminalCoordinator
	lease         *RunAttemptLease
	heartbeat     *attemptHeartbeatScope
	terminalCtx   context.Context
	expectedRun   string
	expectedGraph string
}

func (terminalizer *preparedRunTerminalizer) Terminalize(
	ctx context.Context,
	event loom.TerminalizationEvent,
) error {
	if terminalizer == nil {
		return fmt.Errorf("prepared run terminalizer is nil")
	}
	if event.RunID == "" ||
		event.RunID != terminalizer.expectedRun ||
		event.GraphName != terminalizer.expectedGraph {
		return &TerminalPersistenceError{Err: fmt.Errorf(
			"Loom terminalization identity does not match admitted run",
		)}
	}
	if heartbeatErr := terminalizer.heartbeat.Stop(); heartbeatErr != nil {
		return heartbeatErr
	}
	terminalCtx := terminalizer.terminalCtx
	if terminalCtx == nil {
		terminalCtx = ctx
	}
	result := resultFromLoomTerminalization(event)
	if terminalizer.prepared.terminalResultHook != nil {
		if terminalizer.lease == nil {
			return &TerminalPersistenceError{Err: errors.New("terminal result hook requires an admitted attempt lease")}
		}
		if err := terminalizer.prepared.terminalResultHook(terminalCtx, *terminalizer.lease, result); err != nil {
			return &TerminalPersistenceError{Err: fmt.Errorf("persist terminal result hook: %w", err)}
		}
	}
	coordinated, err := terminalizer.prepared.persistRootTerminal(
		terminalCtx,
		terminalizer.startedAt,
		time.Now(),
		result,
		nil,
		terminalizer.attribution,
		terminalizer.coordinator,
		terminalizer.lease,
	)
	if err != nil {
		return &TerminalPersistenceError{Err: err}
	}
	if !coordinated {
		if err := terminalizer.prepared.handoffYieldedAttempt(
			terminalCtx,
			terminalizer.lease,
			result,
			nil,
		); err != nil {
			return &TerminalPersistenceError{Err: err}
		}
	}
	return nil
}

func resultFromLoomTerminalization(
	event loom.TerminalizationEvent,
) Result {
	return FromRunResult(&loom.RunResult{
		State:      event.State,
		LastStep:   event.LastStep,
		Yielded:    event.Yielded,
		RunID:      event.RunID,
		StopReason: event.StopReason,
	})
}

type nestedRunTerminalizer struct {
	prepared PreparedRun
}

type nestedTerminalizationFailure struct {
	err error
}

func (failure *nestedTerminalizationFailure) Error() string {
	if failure == nil || failure.err == nil {
		return "nested terminalization failed"
	}
	return failure.err.Error()
}

func (failure *nestedTerminalizationFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.err
}

func (terminalizer nestedRunTerminalizer) Terminalize(
	ctx context.Context,
	event loom.TerminalizationEvent,
) error {
	attempt, ok := ctx.Value(
		nestedTerminalAttemptContextKey{},
	).(*nestedTerminalAttempt)
	if !ok || attempt == nil {
		return nil
	}
	err := terminalizer.prepared.observeNestedTerminal(
		ctx,
		&loom.RunResult{
			State:      event.State,
			LastStep:   event.LastStep,
			Yielded:    event.Yielded,
			RunID:      event.RunID,
			StopReason: event.StopReason,
		},
		nil,
		attempt.mode == nestedResume,
	)
	if err != nil {
		return &nestedTerminalizationFailure{err: err}
	}
	return nil
}

func nestedTerminalFailClosedObserver(
	_ context.Context,
	_ *loom.RunResult,
	runErr error,
	_ bool,
) error {
	var failure *nestedTerminalizationFailure
	if !errors.As(runErr, &failure) {
		return nil
	}
	return failure.err
}

type forkRunLifecycle struct {
	prepared        PreparedRun
	attribution     TerminalAttribution
	coordinator     NormalTerminalCoordinator
	runStartedAt    string
	startedAt       time.Time
	heartbeat       AttemptLeaseHeartbeat
	admissionAnchor time.Time
	lease           *RunAttemptLease
	heartbeatScope  *attemptHeartbeatScope
	allocated       bool
	runID           string
	terminalCtx     context.Context
}

func (lifecycle *forkRunLifecycle) RunAllocated(
	ctx context.Context,
	event loom.RunAllocationEvent,
) error {
	if lifecycle == nil {
		return fmt.Errorf("fork run lifecycle is nil")
	}
	attribution := lifecycle.attribution.inputCopy()
	if event.RunID == "" ||
		event.GraphName != lifecycle.prepared.graph.Name ||
		attribution.ParentRunID == nil ||
		attribution.ParentSeq == nil ||
		event.ParentRunID != *attribution.ParentRunID ||
		event.ParentSeq != *attribution.ParentSeq {
		return &ExpectedRunPersistenceError{Err: fmt.Errorf(
			"fork allocation identity does not match terminal attribution",
		)}
	}
	record := expectedRunRecordFromAttribution(
		event.RunID,
		lifecycle.prepared.agent,
		lifecycle.attribution,
		time.Now().UTC(),
	)
	lease, err := lifecycle.prepared.admitFreshExpectedRun(
		ctx,
		record,
		lifecycle.runStartedAt,
	)
	if err != nil {
		return err
	}
	lifecycle.lease = lease
	lifecycle.runID = event.RunID
	if lease != nil {
		lifecycle.startedAt, err = time.Parse(
			time.RFC3339Nano,
			lease.RunStartedAt,
		)
		if err != nil {
			return &ExpectedRunPersistenceError{Err: fmt.Errorf(
				"parse committed fork lease run_started_at: %w",
				err,
			)}
		}
		if err := lifecycle.prepared.runLifecycleHook(
			ctx,
			RunLifecycleStageAttemptAdmitted,
		); err != nil {
			return err
		}
		lifecycle.heartbeatScope, err = startAttemptHeartbeat(
			ctx,
			lifecycle.heartbeat,
			*lease,
			lifecycle.admissionAnchor,
			lifecycle.prepared.heartbeatConfig(),
		)
		if err != nil {
			return err
		}
	}
	lifecycle.allocated = true
	return nil
}

func (lifecycle *forkRunLifecycle) ExecutionContext(
	ctx context.Context,
	event loom.RunAllocationEvent,
) (context.Context, error) {
	if lifecycle == nil || !lifecycle.allocated {
		return nil, fmt.Errorf(
			"fork execution context requested before durable allocation",
		)
	}
	if event.RunID != lifecycle.runID ||
		event.GraphName != lifecycle.prepared.graph.Name {
		return nil, fmt.Errorf(
			"fork execution context identity does not match admitted run",
		)
	}
	if lifecycle.heartbeatScope == nil {
		return ctx, nil
	}
	return lifecycle.heartbeatScope.Context(), nil
}

func (lifecycle *forkRunLifecycle) Terminalize(
	ctx context.Context,
	event loom.TerminalizationEvent,
) error {
	if lifecycle == nil || !lifecycle.allocated {
		return &TerminalPersistenceError{Err: fmt.Errorf(
			"fork terminalization occurred before durable allocation",
		)}
	}
	return (&preparedRunTerminalizer{
		prepared:      lifecycle.prepared,
		startedAt:     lifecycle.startedAt,
		attribution:   lifecycle.attribution,
		coordinator:   lifecycle.coordinator,
		lease:         lifecycle.lease,
		heartbeat:     lifecycle.heartbeatScope,
		terminalCtx:   lifecycle.terminalCtx,
		expectedRun:   lifecycle.runID,
		expectedGraph: lifecycle.prepared.graph.Name,
	}).Terminalize(ctx, event)
}

func (lifecycle *forkRunLifecycle) stopHeartbeat() error {
	if lifecycle == nil {
		return nil
	}
	return lifecycle.heartbeatScope.Stop()
}
