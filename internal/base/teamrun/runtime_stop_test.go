package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

func seedRemoteStopTask(t *testing.T, h *processNextHarness, run TeamRun) *taskqueue.Task {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
 INSERT INTO weave_agents (id,workspace_id,name,role,spec) VALUES ('stop-worker','workspace-1','worker','worker','{}') ON CONFLICT DO NOTHING;
 INSERT INTO weave_agent_versions (agent_id,workspace_id,version,spec) VALUES ('stop-worker','workspace-1',1,'{}') ON CONFLICT DO NOTHING;`); err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{ID: "remote-stop-task", WorkspaceID: run.WorkspaceID, Agent: "worker", AgentID: "stop-worker", AgentVersion: 1,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamWorkerLeaf,
		RunSnapshotID: run.RunSnapshotID, Kind: "engine_exec", Source: "dispatch", Payload: json.RawMessage(`{"task":"work"}`)}
	if err := h.tasks.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE weave_task_queue SET status='running', worker_id='runtime-worker', started_at=$2, lease_expires_at=$3 WHERE id=$1`, task.ID, h.now, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRuntimeStopRequiresEachExecutionAcknowledgement(t *testing.T) {
	for _, acknowledge := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "unconfirmed"}[acknowledge], func(t *testing.T) {
			h := newProcessNextHarness(t)
			taskID, run := h.seedRunningWorkflowTask(t, "run-stop-ack")
			remote := seedRemoteStopTask(t, h, run)
			ctx := context.Background()
			if _, err := h.pool.Exec(ctx, `UPDATE weave_task_queue SET status='running',worker_id='workflow-worker',lease_expires_at=$2 WHERE id=$1`, taskID, h.now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			service := &CancelService{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return h.now }}
			request := retryStopRequest(h, run)
			request.GraceDeadline = h.now.Add(30 * time.Second)
			if _, err := service.RequestCancel(ctx, request); err != nil {
				t.Fatal(err)
			}
			stored, err := h.tasks.Get(ctx, run.WorkspaceID, remote.ID)
			if err != nil || stored.Status != taskqueue.StatusCancelRequested || stored.WorkerID != "runtime-worker" {
				t.Fatalf("lost execution ownership: %+v %v", stored, err)
			}
			now := h.now.Add(time.Second)
			sweep := &CancelGraceSweeper{Transactions: h.pool, Runs: NewPGStore(), Tasks: h.tasks, Now: func() time.Time { return now }}
			if n, err := sweep.Sweep(ctx); err != nil || n != 0 {
				t.Fatalf("premature cancellation: %d %v", n, err)
			}
			// A different worker and a locally joined workflow cannot prove remote exit.
			if err := h.tasks.AcknowledgeExecutionStopped(ctx, remote.ID, "another-worker"); err != nil {
				t.Fatal(err)
			}
			if err := h.tasks.AcknowledgeExecutionStopped(ctx, taskID, "workflow-worker"); err != nil {
				t.Fatal(err)
			}
			if n, err := sweep.Sweep(ctx); err != nil || n != 0 {
				t.Fatalf("remote execution was not acknowledged: %d %v", n, err)
			}
			if acknowledge {
				for range 2 {
					if err := h.tasks.AcknowledgeExecutionStopped(ctx, remote.ID, "runtime-worker"); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				now = h.now.Add(time.Minute)
			}
			if n, err := sweep.Sweep(ctx); err != nil || n != 1 {
				t.Fatalf("settle cancellation: %d %v", n, err)
			}
			want := StatusCancelled
			if !acknowledge {
				want = StatusAbandoned
			}
			if got := h.readRun(t, run.RunID).Status; got != want {
				t.Fatalf("status=%s want=%s", got, want)
			}
			late := *remote
			late.ID = "late-remote"
			if err := h.tasks.Enqueue(ctx, &late); err == nil {
				t.Fatal("late execution admitted after stop")
			}
		})
	}
}

func TestRuntimeLeaseExpiryRequiresStopBeforeExplicitRetry(t *testing.T) {
	h, parked, retry, request := seedRuntimeRetry(t)
	remote := seedRemoteStopTask(t, h, parked)
	ctx := context.Background()
	expired := taskqueue.New(h.pool, fixedTaskClock{now: h.now.Add(2 * time.Minute)}, time.Minute)
	if err := expired.Heartbeat(ctx, remote.ID, "runtime-worker"); err == nil {
		t.Fatal("expired lease was renewed")
	}
	if n, err := expired.RecoverStale(ctx); err != nil || n != 1 {
		t.Fatalf("recover expired remote: %d %v", n, err)
	}
	stored, err := expired.Get(ctx, parked.WorkspaceID, remote.ID)
	if err != nil || stored.Status != taskqueue.StatusFailed || stored.WorkerID != "runtime-worker" || stored.Error != taskqueue.RuntimeLeaseExpiredError {
		t.Fatalf("remote was silently requeued or exit assumed: %+v %v", stored, err)
	}
	if _, err := retry.Retry(ctx, request); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("retried before remote exit: %v", err)
	}
	if err := expired.AcknowledgeExecutionStopped(ctx, remote.ID, "runtime-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := retry.Retry(ctx, request); err != nil {
		t.Fatal(err)
	}
	if got := retryCheckpoint(t, h, h.readRun(t, parked.RunID)).CompletedOutputs["research"]; string(got) != `{"text":"retained evidence"}` {
		t.Fatalf("lost completed output: %s", got)
	}
	// Restarting the stale sweeper cannot create another remote execution.
	if n, err := expired.RecoverStale(ctx); err != nil || n != 0 {
		t.Fatalf("recovered terminal execution again: %d %v", n, err)
	}
}

func TestRuntimeRecoverySecondDisconnectKeepsFirstCompletedOutput(t *testing.T) {
	h, parked, retry, request := seedRuntimeRetry(t)
	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := retry.Retry(ctx, request); err != nil {
			t.Fatal(err)
		}
		h.runtime.resumeResult = RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
			NodeID: "deliver", CompletedOutputs: map[string]json.RawMessage{"research": json.RawMessage(`{"text":"retained evidence"}`)},
			WaitKind: WaitRuntime, WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`), UsageComplete: true,
		}}
		if ok, err := h.executor.ProcessNext(ctx, "retry-worker"); err != nil || !ok {
			t.Fatalf("retry %d: %v %v", attempt, ok, err)
		}
		if got := h.readRun(t, parked.RunID); got.Status != StatusParked {
			t.Fatalf("second interruption: %+v", got)
		}
	}
	if h.runtime.executeCalls != 0 || h.runtime.resumeCalls != 2 {
		t.Fatalf("restarted completed stages: execute=%d resume=%d", h.runtime.executeCalls, h.runtime.resumeCalls)
	}
	if _, err := retry.Retry(ctx, request); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"text":"final"}`), UsageComplete: true}
	if ok, err := h.executor.ProcessNext(ctx, "retry-worker"); err != nil || !ok {
		t.Fatalf("finish recovery: %v %v", ok, err)
	}
	if got := h.readRun(t, parked.RunID); got.Status != StatusSucceeded {
		t.Fatalf("final recovery status: %+v", got)
	}
}

func TestRuntimeCallerCancellationKeepsUnconfirmedProcessOwnership(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTask(t, "run-caller-stop")
	remote := seedRemoteStopTask(t, h, run)
	if err := h.tasks.Cancel(context.Background(), run.WorkspaceID, remote.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := h.tasks.Get(context.Background(), run.WorkspaceID, remote.ID)
	if err != nil || stored.Status != taskqueue.StatusCancelRequested || stored.WorkerID != "runtime-worker" {
		t.Fatalf("caller cleanup assumed process exit: %+v %v", stored, err)
	}
}
