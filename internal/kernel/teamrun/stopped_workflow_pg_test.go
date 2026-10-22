package teamrun

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// stoppedSourceRun leaves the state #91 describes: a pooled executor force-
// drains a running workflow, so its source task fails awaiting outcome
// reconciliation with the drained worker's stop acknowledged, while the
// TeamRun stays running.
// With checkpointed, the run has reached a checkpoint before the drain, as a
// run between members has; deadline, when set, bounds the source task.
func stoppedSourceRun(t *testing.T, name string, checkpointed bool, deadline *time.Time) (*processNextHarness, string, TeamRun) {
	t.Helper()
	h := newProcessNextHarness(t)
	h.seedWorkflowSnapshot(t, name)
	taskID := "task-" + name
	if err := h.tasks.Enqueue(t.Context(), &taskqueue.Task{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"},
		OutcomeSensitive: true, DeadlineAt: deadline, ID: taskID, WorkspaceID: "workspace-1",
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: "workflow-1", WorkflowVersion: 1, RunSnapshotID: name,
		Source: "api", SourceRef: "build-" + name, Kind: "team_workflow", Payload: json.RawMessage(`{"input":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	blocked := &pooledBlockingRuntime{RuntimeRunner: h.runtime, started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
	h.executor.Runtime = blocked
	workers := &Workers{Pooled: true, Executor: h.executor, ExecutorConcurrency: 1, DrainTimeout: 100 * time.Millisecond, PollInterval: time.Hour,
		CancelGrace: &CancelGraceSweeper{Tasks: h.tasks, Transactions: h.pool, Runs: NewPGStore()}, TimerWake: &TimerWakeSweeper{Transactions: h.pool, Runs: NewPGStore(), Executor: h.executor}}
	workers.Start()
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("workflow never started")
	}
	workers.Stop()
	task, err := h.tasks.Get(t.Context(), "workspace-1", taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != taskqueue.StatusFailed || task.Error != taskqueue.ExecutionLeaseExpiredError || task.WorkerID != "" || task.StoppedEpoch != task.ClaimEpoch || task.StoppedWorkerID == "" {
		t.Fatalf("forced drain did not leave a stopped source task: %+v", task)
	}
	run := h.runForTask(t, taskID)
	if run.Status != StatusRunning || run.CurrentExecutorID == nil || *run.CurrentExecutorID != executorIdentity(taskID, task.StoppedWorkerID) {
		t.Fatalf("forced drain did not leave the run running under the drained executor: %+v", run)
	}
	if checkpointed {
		tx := h.mustBeginTx(t)
		cp := checkpointFromPark(run, RuntimePark{NodeID: "lead", CompletedOutputs: map[string]json.RawMessage{}, UsageComplete: true}, h.now)
		if _, err := h.executor.Checkpoints.PutTx(t.Context(), tx, cp); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	h.executor.Runtime = h.runtime
	h.runtime.executeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"ok":true}`)}
	h.runtime.resumeResult = h.runtime.executeResult
	return h, taskID, run
}

func (h *processNextHarness) runForTask(t *testing.T, taskID string) TeamRun {
	t.Helper()
	var runID string
	if err := h.pool.QueryRow(t.Context(), `SELECT run_id FROM weave_team_runs WHERE workspace_id='workspace-1' AND source_task_id=$1`, taskID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	runs := NewPGStore()
	runs.Transactions = h.pool
	run, err := runs.Get(t.Context(), "workspace-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (h *processNextHarness) stoppedSweeper() *StoppedWorkflowSweeper {
	return &StoppedWorkflowSweeper{Tasks: h.tasks, Transactions: h.pool, Runs: NewPGStore(), Executor: h.executor, Now: func() time.Time { return h.now }}
}

// TestStoppedWorkflowResumesCheckpointRealPG reopens the stopped source task,
// and the next claim takes over the running run and finishes it.
func TestStoppedWorkflowResumesCheckpointRealPG(t *testing.T) {
	h, taskID, _ := stoppedSourceRun(t, "stopped-resume", true, nil)
	before, err := h.tasks.Get(t.Context(), "workspace-1", taskID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := h.stoppedSweeper().Sweep(t.Context())
	if err != nil || recovered != 1 {
		t.Fatalf("sweep recovered %d: %v", recovered, err)
	}
	h.assertTask(t, taskID, taskqueue.StatusQueued, "", "")
	if again, err := h.stoppedSweeper().Sweep(t.Context()); err != nil || again != 0 {
		t.Fatalf("second sweep recovered %d: %v", again, err)
	}
	processed, err := h.executor.ProcessNext(t.Context(), "after-drain")
	if err != nil || !processed {
		t.Fatalf("reopened task was not processed: %v %v", processed, err)
	}
	after, err := h.tasks.Get(t.Context(), "workspace-1", taskID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != taskqueue.StatusCompleted || after.ClaimEpoch != before.ClaimEpoch+1 {
		t.Fatalf("resumed task = %s at epoch %d, want completed at %d", after.Status, after.ClaimEpoch, before.ClaimEpoch+1)
	}
	if run := h.runForTask(t, taskID); run.Status != StatusSucceeded {
		t.Fatalf("resumed run = %s, want succeeded", run.Status)
	}
	// The run continued from its checkpoint; it did not start over.
	if h.runtime.resumeCalls != 1 || h.runtime.executeCalls != 0 {
		t.Fatalf("after the drain: %d checkpoint resumes, %d fresh executions; want 1 and 0", h.runtime.resumeCalls, h.runtime.executeCalls)
	}
}

// TestStoppedWorkflowWithoutCheckpointAbandonsRealPG covers a run drained
// before its first checkpoint: the reopened task's takeover cannot resume
// anything and must not start the unknown attempt over, so the run ends
// abandoned as unrecoverable instead of staying running.
func TestStoppedWorkflowWithoutCheckpointAbandonsRealPG(t *testing.T) {
	h, taskID, _ := stoppedSourceRun(t, "stopped-fresh", false, nil)
	if recovered, err := h.stoppedSweeper().Sweep(t.Context()); err != nil || recovered != 1 {
		t.Fatalf("sweep recovered %d: %v", recovered, err)
	}
	_, _ = h.executor.ProcessNext(t.Context(), "after-drain")
	run := h.runForTask(t, taskID)
	if run.Status != StatusAbandoned || run.ErrorCode == nil || *run.ErrorCode != ErrorCodeExecutionUnrecoverable {
		t.Fatalf("run drained before a checkpoint = %+v, want abandoned as unrecoverable", run)
	}
	if calls := h.runtime.executeCalls + h.runtime.resumeCalls; calls != 0 {
		t.Fatalf("run drained before a checkpoint executed %d more times", calls)
	}
}

// TestStoppedWorkflowPastDeadlineFailsRealPG terminates a run whose stopped
// source task is past its deadline, without reviving the task.
func TestStoppedWorkflowPastDeadlineFailsRealPG(t *testing.T) {
	deadline := time.Date(2026, 8, 25, 10, 1, 0, 0, time.UTC) // the harness clock reads 10:00
	h, taskID, _ := stoppedSourceRun(t, "stopped-expired", true, &deadline)
	sweeper := h.stoppedSweeper()
	sweeper.Now = func() time.Time { return deadline.Add(time.Minute) }
	recovered, err := sweeper.Sweep(t.Context())
	if err != nil || recovered != 1 {
		t.Fatalf("sweep recovered %d: %v", recovered, err)
	}
	run := h.runForTask(t, taskID)
	if run.Status != StatusFailed || run.ErrorCode == nil || *run.ErrorCode != ErrorCodeExecutionUnrecoverable ||
		run.CauseSummary == nil || !strings.Contains(*run.CauseSummary, "deadline") {
		t.Fatalf("expired run = %+v, want failed with a deadline cause", run)
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", taskqueue.ExecutionLeaseExpiredError)
	if again, err := sweeper.Sweep(t.Context()); err != nil || again != 0 {
		t.Fatalf("second sweep recovered %d: %v", again, err)
	}
	if processed, err := h.executor.ProcessNext(t.Context(), "after-deadline"); err != nil || processed {
		t.Fatalf("expired task was claimable: %v %v", processed, err)
	}
	if calls := h.runtime.executeCalls + h.runtime.resumeCalls; calls != 0 {
		t.Fatalf("expired run executed %d times", calls)
	}
}

// TestStoppedWorkflowWaitsForStopProofRealPG leaves a task whose lease
// expired but whose worker never acknowledged stopping.
func TestStoppedWorkflowWaitsForStopProofRealPG(t *testing.T) {
	h, taskID, run := stoppedSourceRun(t, "stopped-unproven", true, nil)
	// Undo the acknowledgement: the worker still owns the expired execution.
	if _, err := h.pool.Exec(t.Context(), `UPDATE weave_task_queue SET worker_id=stopped_worker_id,stopped_epoch=claim_epoch-1 WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	recovered, err := h.stoppedSweeper().Sweep(t.Context())
	if err != nil || recovered != 0 {
		t.Fatalf("sweep without stop proof recovered %d: %v", recovered, err)
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", taskqueue.ExecutionLeaseExpiredError)
	if current := h.runForTask(t, taskID); current.Status != StatusRunning || current.Generation != run.Generation {
		t.Fatalf("run without stop proof changed: %+v", current)
	}
}

// TestStoppedWorkflowConcurrentSweepsReopenOnceRealPG runs sweepers at once:
// exactly one reopens the task and only one new attempt runs.
func TestStoppedWorkflowConcurrentSweepsReopenOnceRealPG(t *testing.T) {
	h, taskID, _ := stoppedSourceRun(t, "stopped-concurrent", true, nil)
	const sweepers = 6
	var wg sync.WaitGroup
	results := make([]int, sweepers)
	errs := make([]error, sweepers)
	for i := range sweepers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.stoppedSweeper().Sweep(context.Background())
		}()
	}
	wg.Wait()
	total := 0
	for i := range sweepers {
		if errs[i] != nil {
			t.Fatalf("sweeper %d: %v", i, errs[i])
		}
		total += results[i]
	}
	if total != 1 {
		t.Fatalf("concurrent sweepers reopened %d times, want 1", total)
	}
	if processed, err := h.executor.ProcessNext(t.Context(), "after-drain"); err != nil || !processed {
		t.Fatalf("reopened task was not processed: %v %v", processed, err)
	}
	if processed, err := h.executor.ProcessNext(t.Context(), "after-drain-2"); err != nil || processed {
		t.Fatalf("a second attempt was claimable: %v %v", processed, err)
	}
	if run := h.runForTask(t, taskID); run.Status != StatusSucceeded {
		t.Fatalf("run = %s, want succeeded", run.Status)
	}
}
