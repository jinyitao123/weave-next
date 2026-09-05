package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

func TestStageRetryLostResponseCannotConsumeSecondDisconnectionAfterServerRestart(t *testing.T) {
	h, parked, service, request := seedRuntimeRetry(t)
	ctx := context.Background()
	request.IdempotencyKey = "first-click"
	if result, err := service.Retry(ctx, request); err != nil || result.Replayed {
		t.Fatalf("initial retry: %+v %v", result, err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
		NodeID: "deliver", CompletedOutputs: map[string]json.RawMessage{"research": json.RawMessage(`{"text":"retained evidence"}`)},
		WaitKind: WaitRuntime, WaitDetail: parked.WaitDetail, UsageComplete: true,
	}}
	if ok, err := h.executor.ProcessNext(ctx, "retry-worker"); err != nil || !ok {
		t.Fatalf("second disconnect: %v %v", ok, err)
	}
	secondFailure := h.readRun(t, parked.RunID)
	checkpoint := retryCheckpoint(t, h, secondFailure)
	// All transient service state is lost. The old browser command only has its
	// first click key, and never received the original HTTP response.
	restarted := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool}, Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks, Now: h.executor.Now}
	for range 3 {
		result, err := restarted.Retry(ctx, request)
		if err != nil || !result.Replayed || result.Status != "queued" {
			t.Fatalf("lost-response replay: %+v %v", result, err)
		}
		if got := h.readRun(t, parked.RunID); !reflect.DeepEqual(got, secondFailure) {
			t.Fatal("old command consumed the second failure")
		}
	}
	if got := retryCheckpoint(t, h, secondFailure); !reflect.DeepEqual(got, checkpoint) || string(got.CompletedOutputs["research"]) != `{"text":"retained evidence"}` {
		t.Fatal("old command changed saved output")
	}
	wrong := request
	wrong.NodeID = "research"
	if _, err := restarted.Retry(ctx, wrong); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("one command key changed stages: %v", err)
	}
	request.IdempotencyKey = "second-explicit-click"
	if result, err := restarted.Retry(ctx, request); err != nil || result.Replayed {
		t.Fatalf("new explicit retry: %+v %v", result, err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"text":"final"}`), UsageComplete: true}
	if ok, err := h.executor.ProcessNext(ctx, "retry-worker"); err != nil || !ok {
		t.Fatalf("final recovery: %v %v", ok, err)
	}
	if h.runtime.resumeCalls != 2 || h.runtime.executeCalls != 0 || h.readRun(t, parked.RunID).Status != StatusSucceeded {
		t.Fatal("duplicate physical execution or completed work restarted")
	}
	if result, err := restarted.Retry(ctx, request); err != nil || !result.Replayed {
		t.Fatalf("terminal receipt replay: %+v %v", result, err)
	}
}

func TestStageRetryFanoutReceiptSurvivesAnotherFailureAndStop(t *testing.T) {
	h, run, taskID := seedFanoutStageRetry(t)
	ctx := context.Background()
	service := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool}, Tasks: h.tasks}
	request := StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics", IdempotencyKey: "fanout-click"}
	// Retire the source row this harness leaves queued for crash/reclaim tests.
	if _, err := h.pool.Exec(ctx, `UPDATE weave_task_queue SET status='completed' WHERE workspace_id=$1 AND id=$2`, run.WorkspaceID, run.SourceTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retry(ctx, request); err != nil {
		t.Fatal(err)
	}
	claimed, err := h.tasks.Claim(ctx, "second-attempt", taskqueue.ClaimFilter{Kind: "team_workflow", IdentityKind: taskqueue.IdentityTeamWorkflow, RunSnapshotID: run.RunSnapshotID})
	if err != nil || claimed == nil || claimed.ID != taskID {
		t.Fatalf("claim second fanout execution: %+v %v", claimed, err)
	}
	if err := h.tasks.FailClaimed(ctx, taskID, "second-attempt", "runtime offline: second disconnect"); err != nil {
		t.Fatal(err)
	}
	if result, err := service.Retry(ctx, request); err != nil || !result.Replayed {
		t.Fatalf("fanout receipt replay: %+v %v", result, err)
	}
	if task, err := h.tasks.Get(ctx, run.WorkspaceID, taskID); err != nil || task.Status != taskqueue.StatusFailed {
		t.Fatalf("old fanout click repeated work: %+v %v", task, err)
	}
	stop := &CancelService{Transactions: h.pool, Runs: service.Runs, Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Minute) }}
	if _, err := stop.RequestCancel(ctx, retryStopRequest(h, run)); err != nil {
		t.Fatal(err)
	}
	if result, err := service.Retry(ctx, request); err != nil || !result.Replayed {
		t.Fatalf("stop erased original receipt: %+v %v", result, err)
	}
	request.IdempotencyKey = "new-click-after-stop"
	if _, err := service.Retry(ctx, request); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("new retry bypassed stop: %v", err)
	}
}

func TestStageRetryFailedTransactionDoesNotReserveRequestIdentity(t *testing.T) {
	h, parked, service, request := seedRuntimeRetry(t)
	request.IdempotencyKey = "retry-queue-failure"
	service.Tasks = failingRetryEnqueue{service.Tasks}
	if _, err := service.Retry(context.Background(), request); err == nil {
		t.Fatal("retry committed despite enqueue failure")
	}
	var receipts int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_team_run_activity_events WHERE kind='stage_retry_requested'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("rolled-back retry retained receipt: %d %v", receipts, err)
	}
	service.Tasks = h.tasks
	if result, err := service.Retry(context.Background(), request); err != nil || result.Replayed {
		t.Fatalf("failed request could not be retried: %+v %v", result, err)
	}
	if got := h.readRun(t, parked.RunID); got.Status != StatusRunning {
		t.Fatal("retry did not resume")
	}
}
