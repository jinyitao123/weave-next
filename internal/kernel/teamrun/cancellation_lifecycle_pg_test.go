package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestV1CancellationJoinsModelAndSettlesRootAttemptRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	release := make(chan struct{})
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	blocked := &cancellationBlockingLLM{
		started: started, cancelObserved: cancelObserved, release: release,
		errAfterRelease: errors.New("provider returned no receipt after cancellation"),
	}
	known := &cancellationFixedLLM{response: contract.ChatResponse{
		Content: "confirmed first model result",
		Usage:   contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: 0.25},
	}}
	runtime := newCancellationGraphRuntime(t, known, blocked)
	h.executor.Runtime = runtime
	h.executor.HeartbeatInterval = 5 * time.Millisecond
	taskID := h.enqueueWorkflowTask(t, "run-cancel-v1")
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	type processResult struct {
		processed bool
		err       error
	}
	done := make(chan processResult, 1)
	go func() {
		processed, err := h.executor.ProcessNext(context.Background(), "cancel-worker")
		done <- processResult{processed: processed, err: err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("V1 model was not started")
	}
	initial := h.readRun(t, "run-cancel-v1")
	if initial.Status != StatusRunning {
		t.Fatalf("run status before cancel = %q", initial.Status)
	}
	service := &CancelService{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now }}
	requested, err := service.RequestCancel(context.Background(), CancelRequest{
		WorkspaceID: initial.WorkspaceID, RunID: initial.RunID,
		CancelActor: "user-1", CancelReason: "stop this run",
		GraceDeadline: h.now.Add(time.Minute), IdempotencyKey: "cancel-v1-once",
	})
	if err != nil || requested.Status != StatusCancelRequested {
		t.Fatalf("request cancellation: status=%q err=%v", requested.Status, err)
	}
	select {
	case <-cancelObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("V1 model did not observe cancellation")
	}
	select {
	case result := <-done:
		t.Fatalf("workflow returned before blocked model release: %+v", result)
	default:
	}
	h.assertTask(t, taskID, taskqueue.StatusCancelRequested, "", "")
	var leaseState string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT state FROM weave_run_attempt_leases
		WHERE workspace_id='workspace-1' AND run_id='run-cancel-v1'
	`).Scan(&leaseState); err != nil || leaseState != string(loomruntime.AttemptLeaseActive) {
		t.Fatalf("root attempt before local execution exits = %q, err=%v", leaseState, err)
	}
	var markerCount int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM weave_run_terminal_markers
		WHERE workspace_id='workspace-1' AND run_id='run-cancel-v1'
	`).Scan(&markerCount); err != nil || markerCount != 0 {
		t.Fatalf("root marker before local execution exits = %d, err=%v", markerCount, err)
	}

	close(release)
	select {
	case result := <-done:
		if !result.processed || !errors.Is(result.err, errTaskLeaseLost) {
			t.Fatalf("process cancellation result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not finish after releasing the model")
	}
	h.assertTask(t, taskID, taskqueue.StatusCancelled, "", "")
	if got := h.readRun(t, "run-cancel-v1"); got.Status != StatusCancelRequested {
		t.Fatalf("TeamRun was confirmed before the existing sweeper: %q", got.Status)
	}
	if runtime.lastResult.Usage.InputTokens != 5 || runtime.lastResult.Usage.OutputTokens != 2 {
		t.Fatalf("confirmed earlier usage was lost: result=%+v serial=%+v knownCalls=%d", runtime.lastResult, runtime.lastSerial, known.calls)
	}
	sweep := &CancelGraceSweeper{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Second) }}
	if n, err := sweep.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("confirm stopped TeamRun: %d %v", n, err)
	}
	cancelCode := ErrorCodeCancelled
	h.assertRun(t, "run-cancel-v1", StatusCancelled, &cancelCode)
	h.assertTerminalMarker(t, "run-cancel-v1", "failed", teamRunCancelledTerminalStopReason)
	assertCancelledRootAttemptAndUsage(t, h, "run-cancel-v1", 5, 2, false)

	// A new executor instance can replay the exact durable terminal without
	// turning cancellation into ordinary TeamRun failure or losing usage facts.
	restarted := &Executor{
		Transactions: h.pool, Runs: NewPGStore(), Consumer: h.executor.Consumer,
		Now: func() time.Time { return h.now },
	}
	last := runtime.lastResult
	if err := restarted.commitFrozenNormalTerminal(
		context.Background(), initial, "failed", teamRunCancelledTerminalStopReason,
		last.Usage, last.UsageCoverage, last.UsageComplete, last.UsageIncompleteReason, last.MemberBreakdown,
	); err != nil {
		t.Fatalf("replay cancellation terminal after restart: %v", err)
	}
	assertCancelledRootAttemptAndUsage(t, h, "run-cancel-v1", 5, 2, false)
}

func TestCancelSweeperRecoversOnlyExactCancelledRootStopMarkerRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, running := h.seedRunningWorkflowTask(t, "run-cancel-restart")
	if _, err := h.pool.Exec(context.Background(), `
		UPDATE weave_task_queue SET status='running', worker_id='former-worker',
			started_at=$2, lease_expires_at=$3
		WHERE workspace_id='workspace-1' AND id=$1
	`, taskID, h.now, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	unrelated := &taskqueue.Task{
		ID: "other-workflow-task", WorkspaceID: running.WorkspaceID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: running.WorkflowID, WorkflowVersion: running.WorkflowVersion,
		RunSnapshotID: running.RunSnapshotID, Source: "api", Kind: "team_workflow",
		SourceRef: "unrelated", Payload: json.RawMessage(`{"input":true}`),
	}
	if err := h.tasks.Enqueue(context.Background(), unrelated); err != nil {
		t.Fatalf("enqueue unrelated workflow task: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `
		UPDATE weave_task_queue SET status='running', worker_id='other-worker',
			started_at=$2, lease_expires_at=$3
		WHERE workspace_id='workspace-1' AND id=$1
	`, unrelated.ID, h.now, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	service := &CancelService{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now }}
	if got, err := service.RequestCancel(context.Background(), CancelRequest{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID,
		CancelActor: "user-1", CancelReason: "restart window",
		GraceDeadline: h.now.Add(time.Minute), IdempotencyKey: "cancel-restart-once",
	}); err != nil || got.Status != StatusCancelRequested {
		t.Fatalf("request cancellation: status=%q err=%v", got.Status, err)
	}
	cancelRequested := h.readRun(t, running.RunID)
	usage := loomruntime.UsageTotals{InputTokens: 3, OutputTokens: 1}
	if err := h.executor.commitFrozenNormalTerminal(
		context.Background(), cancelRequested, "failed", teamRunCancelledTerminalStopReason, usage,
		nil, false, UsageIncompleteReasonAttemptLost,
	); err != nil {
		t.Fatalf("seed terminal-before-ack crash window: %v", err)
	}
	replayTx := h.mustBeginTx(t)
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(context.Background(), replayTx, running.WorkspaceID, running.RunID)
	if err != nil || !leasePresent {
		t.Fatalf("read cancellation lease for replay: present=%v err=%v", leasePresent, err)
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(context.Background(), replayTx, running.WorkspaceID, running.RunID)
	if err != nil || !markerPresent {
		t.Fatalf("read cancellation marker for replay: present=%v err=%v", markerPresent, err)
	}
	claimedRoot, err := h.tasks.Get(context.Background(), running.WorkspaceID, taskID)
	if err != nil || claimedRoot == nil {
		t.Fatalf("read root task for terminal replay: task=%+v err=%v", claimedRoot, err)
	}
	if _, err := h.executor.replayFrozenTerminalTx(context.Background(), replayTx, running, claimedRoot, marker, lease); !errors.Is(err, errCancelledTerminalCannotReplayAsFailure) {
		t.Fatalf("cancellation marker entered ordinary failure replay: %v", err)
	}
	_ = replayTx.Rollback(context.Background())
	if cancelRequested.Status != StatusCancelRequested {
		t.Fatalf("run status = %q", cancelRequested.Status)
	}
	tx := h.mustBeginTx(t)
	newGeneration := cancelRequested
	newGeneration.ExecutionLeaseEpoch++
	proof, err := stoppedCancelledRootAttemptTx(context.Background(), tx, newGeneration)
	if err != nil || proof {
		t.Fatalf("old marker proved a newer attempt: proof=%v err=%v", proof, err)
	}
	_ = tx.Rollback(context.Background())
	rootTask, err := h.tasks.Get(context.Background(), running.WorkspaceID, taskID)
	if err != nil || rootTask == nil || rootTask.ClaimEpoch < 1 {
		t.Fatalf("read root task claim: task=%+v err=%v", rootTask, err)
	}
	acked, err := h.tasks.AcknowledgeCancelledWorkflowStoppedAtEpoch(
		context.Background(), running.WorkspaceID, taskID, "former-worker", rootTask.ClaimEpoch+1,
	)
	if err != nil || acked {
		t.Fatalf("wrong claim epoch acknowledged root task: acked=%v err=%v", acked, err)
	}
	rootTask, err = h.tasks.Get(context.Background(), running.WorkspaceID, taskID)
	if err != nil || rootTask.Status != taskqueue.StatusCancelRequested || rootTask.WorkerID != "former-worker" {
		t.Fatalf("wrong epoch changed root task: task=%+v err=%v", rootTask, err)
	}

	sweep := &CancelGraceSweeper{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(2 * time.Second) }}
	if n, err := sweep.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("recover root stop marker: %d %v", n, err)
	}
	rootTask, err = h.tasks.Get(context.Background(), running.WorkspaceID, taskID)
	if err != nil || rootTask.Status != taskqueue.StatusCancelled || rootTask.WorkerID != "" {
		t.Fatalf("root task stop was not replayed: task=%+v err=%v", rootTask, err)
	}
	otherTask, err := h.tasks.Get(context.Background(), running.WorkspaceID, unrelated.ID)
	if err != nil || otherTask.Status != taskqueue.StatusCancelRequested || otherTask.WorkerID != "other-worker" {
		t.Fatalf("root marker acknowledged another workflow task: task=%+v err=%v", otherTask, err)
	}
	if got := h.readRun(t, running.RunID); got.Status != StatusCancelRequested {
		t.Fatalf("TeamRun confirmed while other task remained active: %q", got.Status)
	}
	if err := h.tasks.AcknowledgeExecutionStopped(context.Background(), unrelated.ID, "other-worker"); err != nil {
		t.Fatal(err)
	}
	if n, err := sweep.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("confirm after independent task stop: %d %v", n, err)
	}
	cancelCode := ErrorCodeCancelled
	h.assertRun(t, running.RunID, StatusCancelled, &cancelCode)
	assertCancelledRootAttemptAndUsage(t, h, running.RunID, 3, 1, false)
}

func TestV1WorkerShutdownAndQueueLeaseLossDoNotBecomeUserCancellationRealPG(t *testing.T) {
	for _, mode := range []string{"worker_shutdown", "queue_lease_lost"} {
		t.Run(mode, func(t *testing.T) {
			h := newProcessNextHarness(t)
			release := make(chan struct{})
			started := make(chan struct{})
			cancelObserved := make(chan struct{})
			blocked := &cancellationBlockingLLM{
				started: started, cancelObserved: cancelObserved, release: release,
				errAfterRelease: errors.New("provider returned no receipt after interruption"),
			}
			known := &cancellationFixedLLM{response: contract.ChatResponse{
				Content: "confirmed first model result",
				Usage:   contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: 0.25},
			}}
			runtime := newCancellationGraphRuntime(t, known, blocked)
			h.executor.Runtime = runtime
			h.executor.HeartbeatInterval = 5 * time.Millisecond
			runID := "run-stop-" + mode
			var taskID string
			if mode == "queue_lease_lost" {
				taskID = h.enqueueOutcomeSensitiveWorkflowTask(t, runID)
			} else {
				taskID = h.enqueueWorkflowTask(t, runID)
			}
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()

			workerCtx, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			type processResult struct {
				processed bool
				err       error
			}
			done := make(chan processResult, 1)
			go func() {
				processed, err := h.executor.ProcessNext(workerCtx, mode+"-worker")
				done <- processResult{processed: processed, err: err}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("V1 model was not started")
			}
			run := h.readRun(t, runID)
			if mode == "worker_shutdown" {
				shutdown()
			} else {
				if _, err := h.pool.Exec(context.Background(), `
					UPDATE weave_task_queue SET status='failed', error=$2
					WHERE workspace_id='workspace-1' AND id=$1
				`, taskID, taskqueue.ExecutionLeaseExpiredError); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-cancelObserved:
			case <-time.After(3 * time.Second):
				t.Fatal("V1 model did not observe execution stop")
			}
			select {
			case result := <-done:
				t.Fatalf("execution returned before the model was released: %+v", result)
			default:
			}
			if current := h.readRun(t, run.RunID); current.Status != StatusRunning {
				t.Fatalf("non-user stop changed TeamRun status to %q", current.Status)
			}
			var leaseState string
			if err := h.pool.QueryRow(context.Background(), `
				SELECT state FROM weave_run_attempt_leases WHERE workspace_id='workspace-1' AND run_id=$1
			`, run.RunID).Scan(&leaseState); err != nil || leaseState != string(loomruntime.AttemptLeaseActive) {
				t.Fatalf("root attempt before release = %q, err=%v", leaseState, err)
			}

			close(release)
			select {
			case result := <-done:
				if !result.processed {
					t.Fatalf("processed = false, err=%v", result.err)
				}
				if mode == "worker_shutdown" && !errors.Is(result.err, context.Canceled) {
					t.Fatalf("shutdown error = %v", result.err)
				}
				if mode == "queue_lease_lost" && !errors.Is(result.err, errTaskLeaseLost) {
					t.Fatalf("lease-loss error = %v", result.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("workflow did not finish after releasing the model")
			}
			if current := h.readRun(t, run.RunID); current.Status != StatusRunning {
				t.Fatalf("non-user stop terminalized TeamRun as %q", current.Status)
			}
			if runtime.lastResult.Usage.InputTokens != 5 || runtime.lastResult.Usage.OutputTokens != 2 {
				t.Fatalf("returned RuntimeResult lost prior usage: %+v", runtime.lastResult)
			}
			if err := h.pool.QueryRow(context.Background(), `
				SELECT state FROM weave_run_attempt_leases WHERE workspace_id='workspace-1' AND run_id=$1
			`, run.RunID).Scan(&leaseState); err != nil || leaseState != string(loomruntime.AttemptLeaseActive) {
				t.Fatalf("root attempt was falsely settled without user cancel: %q, err=%v", leaseState, err)
			}
			var markerCount int
			if err := h.pool.QueryRow(context.Background(), `
				SELECT count(*) FROM weave_run_terminal_markers WHERE workspace_id='workspace-1' AND run_id=$1
			`, run.RunID).Scan(&markerCount); err != nil || markerCount != 0 {
				t.Fatalf("non-user stop wrote a cancellation marker: count=%d err=%v", markerCount, err)
			}
		})
	}
}

func TestOrdinaryRuntimeErrorLosesRaceToDurableCancelRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	started := make(chan struct{})
	release := make(chan struct{})
	runtime := &cancellationErrorRuntime{
		started: started, release: release,
		result: RuntimeResult{
			Status: RuntimeFailed, Usage: UsageTotals{InputTokens: 4, OutputTokens: 1},
			UsageComplete: false, UsageIncompleteReason: UsageIncompleteReasonAttemptLost,
		},
		err: executionError(ErrorCodeRuntimeIncompatible, errors.New("published credential was revoked")),
	}
	h.executor.Runtime = runtime
	// The durable cancel must win even when a runtime reports an ordinary
	// authorization error before the next queue heartbeat observes it.
	h.executor.HeartbeatInterval = time.Hour
	taskID := h.enqueueWorkflowTask(t, "run-error-cancel-race")
	type processResult struct {
		processed bool
		err       error
	}
	done := make(chan processResult, 1)
	go func() {
		processed, err := h.executor.ProcessNext(context.Background(), "error-race-worker")
		done <- processResult{processed: processed, err: err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime execution did not start")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	run := h.readRun(t, "run-error-cancel-race")
	service := &CancelService{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now }}
	if got, err := service.RequestCancel(context.Background(), CancelRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		CancelActor: "user-1", CancelReason: "cancel while authorization changes",
		GraceDeadline: h.now.Add(time.Minute), IdempotencyKey: "error-race-cancel-once",
	}); err != nil || got.Status != StatusCancelRequested {
		t.Fatalf("request cancellation: status=%q err=%v", got.Status, err)
	}
	select {
	case result := <-done:
		t.Fatalf("runtime returned before release: %+v", result)
	default:
	}
	close(release)
	select {
	case result := <-done:
		if !result.processed || result.err == nil || !strings.Contains(result.err.Error(), "published credential was revoked") {
			t.Fatalf("ordinary runtime error result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime error did not settle after release")
	}
	h.assertTask(t, taskID, taskqueue.StatusCancelled, "", "")
	sweep := &CancelGraceSweeper{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Second) }}
	if n, err := sweep.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("confirm cancelled run: %d %v", n, err)
	}
	cancelCode := ErrorCodeCancelled
	h.assertRun(t, run.RunID, StatusCancelled, &cancelCode)
	h.assertTerminalMarker(t, run.RunID, "failed", teamRunCancelledTerminalStopReason)
	assertCancelledRootAttemptAndUsage(t, h, run.RunID, 4, 1, false)
}

func (h *processNextHarness) enqueueOutcomeSensitiveWorkflowTask(t *testing.T, runID string) string {
	t.Helper()
	h.seedWorkflowSnapshot(t, runID)
	taskID := "task-" + runID
	err := h.tasks.Enqueue(context.Background(), &taskqueue.Task{
		Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"},
		ID:      taskID, WorkspaceID: "workspace-1", IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: "workflow-1", WorkflowVersion: 1,
		RunSnapshotID: runID, Source: "api", SourceRef: "build-" + runID,
		Kind: "team_workflow", Payload: json.RawMessage(`{"input":true}`), OutcomeSensitive: true,
	})
	if err != nil {
		t.Fatalf("enqueue outcome-sensitive workflow task: %v", err)
	}
	return taskID
}

func newCancellationGraphRuntime(t *testing.T, known, blocked contract.LLM) *cancellationGraphRuntime {
	t.Helper()
	knownGraph := compileCancellationTestAgent(t, "known", known)
	blockedGraph := compileCancellationTestAgent(t, "blocked", blocked)
	knownNode := machine.Node{ID: "known-node", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "known", AgentVersion: 1}}
	blockedNode := machine.Node{ID: "blocked-node", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "blocked", AgentVersion: 1}}
	deliver := machine.Node{ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: blockedNode.ID}}}
	return &cancellationGraphRuntime{
		graph: machine.GraphDefinition{
			EntryNodeID: knownNode.ID,
			Nodes:       []machine.Node{knownNode, blockedNode, deliver},
			Edges: []machine.Edge{
				{ID: "known-next", FromNodeID: knownNode.ID, ToNodeID: blockedNode.ID, Route: machine.RouteSuccess},
				{ID: "blocked-next", FromNodeID: blockedNode.ID, ToNodeID: deliver.ID, Route: machine.RouteSuccess},
			},
		},
		artifact: &workflow.RuntimeArtifact{Entries: []workflow.RuntimeGraphEntry{knownGraph, blockedGraph}},
	}
}

func compileCancellationTestAgent(t *testing.T, id string, llm contract.LLM) workflow.RuntimeGraphEntry {
	t.Helper()
	usageLLM := loomruntime.NewUsageBoundaryLLM(llm, loomruntime.NewRandomUsageAttemptID)
	graph, err := compiler.CompileAgent("workspace-1", &registry.AgentRecord{
		WorkspaceID: "workspace-1", ID: id, Name: id, Version: 1, Model: "controlled-test-model",
		Compaction: &registry.CompactionConfig{Enabled: false},
	}, usageLLM, &compiler.CompileGuardDispatcher{}, compiler.CompileOpts{
		BeforeStepHooks: []loom.StepHook{loomruntime.BindUsageBeforeStep},
		ExecutionLLMWrapper: func(inner contract.LLM) contract.LLM {
			return loomruntime.NewLogicalUsageLLM(inner)
		},
	})
	if err != nil {
		t.Fatalf("compile controlled agent %q: %v", id, err)
	}
	return workflow.RuntimeGraphEntry{AgentID: id, AgentVersion: 1, Graph: graph}
}

type cancellationGraphRuntime struct {
	graph      machine.GraphDefinition
	artifact   *workflow.RuntimeArtifact
	mu         sync.Mutex
	lastResult RuntimeResult
	lastSerial serialMachineResult
}

func (r *cancellationGraphRuntime) Execute(ctx context.Context, run TeamRun, _ *taskqueue.Task) (RuntimeResult, error) {
	serial := runSerialMachine(ctx, r.graph, frozen.ArtifactPayloadV1{}, r.artifact, "test input", serialMachineStart{
		Run: run, Usage: loomruntime.NewUsageAccumulator(), UsageComplete: true,
	})
	result, err := runtimeResultFromSerial(serial, frozen.ArtifactPayloadV1{}, nil)
	r.mu.Lock()
	r.lastResult = result
	r.lastSerial = serial
	r.mu.Unlock()
	return result, err
}

func (r *cancellationGraphRuntime) ResumeCheckpoint(ctx context.Context, run TeamRun, _ *taskqueue.Task, _ WorkflowCheckpointV1) (RuntimeResult, error) {
	return r.Execute(ctx, run, nil)
}

func (*cancellationGraphRuntime) TimerResumeTarget(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (string, bool, error) {
	return "", false, nil
}

type cancellationErrorRuntime struct {
	started chan struct{}
	release <-chan struct{}
	result  RuntimeResult
	err     error
	once    sync.Once
}

func (r *cancellationErrorRuntime) Execute(context.Context, TeamRun, *taskqueue.Task) (RuntimeResult, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.result, r.err
}

func (r *cancellationErrorRuntime) ResumeCheckpoint(ctx context.Context, run TeamRun, task *taskqueue.Task, _ WorkflowCheckpointV1) (RuntimeResult, error) {
	return r.Execute(ctx, run, task)
}

func (*cancellationErrorRuntime) TimerResumeTarget(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (string, bool, error) {
	return "", false, nil
}

type cancellationFixedLLM struct {
	response contract.ChatResponse
	calls    int
}

func (l *cancellationFixedLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	l.calls++
	response := l.response
	return &response, nil
}

func (*cancellationFixedLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("stream is not used by this fixture")
}

type cancellationBlockingLLM struct {
	started         chan struct{}
	cancelObserved  chan struct{}
	release         <-chan struct{}
	errAfterRelease error
	once            sync.Once
	cancelOnce      sync.Once
}

func (l *cancellationBlockingLLM) Chat(ctx context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
	l.once.Do(func() { close(l.started) })
	<-ctx.Done()
	l.cancelOnce.Do(func() { close(l.cancelObserved) })
	<-l.release // Model transport intentionally ignores cancellation until released by the test.
	return nil, l.errAfterRelease
}

func (*cancellationBlockingLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("stream is not used by this fixture")
}

func assertCancelledRootAttemptAndUsage(t *testing.T, h *processNextHarness, runID string, input, output int, wantComplete bool) {
	t.Helper()
	var leaseState string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT state FROM weave_run_attempt_leases
		WHERE workspace_id='workspace-1' AND run_id=$1
	`, runID).Scan(&leaseState); err != nil || leaseState != string(loomruntime.AttemptLeaseClosed) {
		t.Fatalf("root attempt state = %q, err=%v", leaseState, err)
	}
	var raw []byte
	if err := h.pool.QueryRow(context.Background(), `
		SELECT value FROM loom_store WHERE namespace='audit:workspace-1' AND key=$1
	`, runID).Scan(&raw); err != nil {
		t.Fatalf("read root terminal audit: %v", err)
	}
	inspection := loomruntime.InspectTerminalRecord(true, raw)
	if inspection.Classification != loomruntime.TerminalRecordValid || inspection.Entry == nil || inspection.Err != nil {
		t.Fatalf("root terminal audit classification=%s err=%v", inspection.Classification, inspection.Err)
	}
	entry := inspection.Entry
	if entry.Status != "failed" || entry.StopReason != teamRunCancelledTerminalStopReason ||
		entry.TokensIn != input || entry.TokensOut != output {
		t.Fatalf("root cancellation audit lost outcome/usage: %+v", entry)
	}
	complete := entry.UsageComplete == nil || *entry.UsageComplete
	if complete != wantComplete {
		t.Fatalf("root cancellation usage_complete=%v, want %v (reason=%q)", complete, wantComplete, entry.UsageIncompleteReason)
	}
	if !wantComplete && entry.UsageIncompleteReason != UsageIncompleteReasonAttemptLost {
		t.Fatalf("root cancellation usage gap reason=%q", entry.UsageIncompleteReason)
	}
}
