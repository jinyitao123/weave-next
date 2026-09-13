package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func seedRuntimeRetry(t *testing.T) (*processNextHarness, TeamRun, *StageRetryService, StageRetryRequest) {
	t.Helper()
	h := newProcessNextHarness(t)
	taskID, run := h.seedRunningWorkflowTask(t, "retry-run")
	task, err := h.tasks.Get(context.Background(), run.WorkspaceID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	parked, err := h.executor.parkRunning(context.Background(), run, task, *run.CurrentExecutorID, RuntimePark{
		NodeID: "deliver", CompletedOutputs: map[string]json.RawMessage{"research": json.RawMessage(`{"text":"retained evidence"}`)},
		WaitKind: WaitRuntime, WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`),
		UsageComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_task_queue SET status='completed' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	h.executor.Now = func() time.Time { return h.now.Add(time.Minute) }
	s := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool}, Checkpoints: NewPGCheckpointStore(),
		Tasks: h.tasks, Now: h.executor.Now}
	return h, parked, s, StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "deliver"}
}

func retryCheckpoint(t *testing.T, h *processNextHarness, run TeamRun) WorkflowCheckpointV1 {
	t.Helper()
	tx := h.mustBeginTx(t)
	defer func() { _ = tx.Rollback(context.Background()) }()
	checkpoint, err := h.executor.Checkpoints.GetTx(context.Background(), tx, run.WorkspaceID, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func TestStageRetryConcurrentRequestsShareOneContinuation(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	request.IdempotencyKey = "same-explicit-retry"
	const requests = 8
	var wg sync.WaitGroup
	results := make(chan error, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every caller races the same current failure through the public command.
			result, err := s.Retry(context.Background(), request)
			if err == nil && result.Status != taskqueue.StatusQueued {
				err = errors.New("concurrent retry did not return queued")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var tasks, transitions int
	if err := h.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM weave_task_queue WHERE source='runtime_retry'),
		(SELECT count(*) FROM weave_team_run_transitions WHERE source='runtime_retry')`).Scan(&tasks, &transitions); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || transitions != 1 || h.readRun(t, parked.RunID).Generation != parked.Generation+1 {
		t.Fatalf("duplicate continuation: tasks=%d transitions=%d", tasks, transitions)
	}
}

type failingRetryEnqueue struct{ failedTaskRequeuer }

func (failingRetryEnqueue) EnqueueTx(context.Context, pgx.Tx, *taskqueue.Task) error {
	return errors.New("queue unavailable")
}

func TestStageRetryEnqueueFailureRollsBackRunAndCheckpoint(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	before := retryCheckpoint(t, h, parked)
	s.Tasks = failingRetryEnqueue{s.Tasks}
	if _, err := s.Retry(context.Background(), request); err == nil {
		t.Fatal("retry succeeded while enqueue failed")
	}
	if after := h.readRun(t, parked.RunID); !reflect.DeepEqual(parked, after) {
		t.Fatalf("failed enqueue changed run: %#v", after)
	}
	if after := retryCheckpoint(t, h, parked); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed enqueue changed checkpoint: %#v", after)
	}
	var transitions int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_team_run_transitions WHERE source='runtime_retry'`).Scan(&transitions); err != nil || transitions != 0 {
		t.Fatalf("rolled back retry transitions=%d err=%v", transitions, err)
	}
}

func TestStageRetryRejectsStopEvenBeforeQueuedTasksAreCancelled(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "parked", true: "retry_queued"}[queued], func(t *testing.T) {
			h, parked, s, request := seedRuntimeRetry(t)
			if queued {
				if _, err := s.Retry(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			// Model the crash window after the durable stop and before child-task cancellation.
			cancel := &CancelService{Transactions: h.pool, Runs: s.Runs, Now: h.executor.Now}
			if _, err := cancel.RequestCancel(context.Background(), CancelRequest{
				WorkspaceID: parked.WorkspaceID, RunID: parked.RunID, CancelActor: "user", CancelReason: "stop",
				GraceDeadline: h.now.Add(2 * time.Minute), IdempotencyKey: "stop",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Retry(context.Background(), request); !errors.Is(err, ErrTeamRunStateConflict) {
				t.Fatalf("retry after stop err=%v", err)
			}
			if queued {
				if processed, err := h.executor.ProcessNext(context.Background(), "stopped-worker"); err != nil || !processed {
					t.Fatalf("consume stopped continuation: processed=%v err=%v", processed, err)
				}
				if h.runtime.resumeCalls != 0 {
					t.Fatal("stopped continuation executed")
				}
			}
		})
	}
}

func TestStageRetryRejectsCheckpointFromEarlierStage(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	checkpoint := retryCheckpoint(t, h, parked)
	checkpoint.TeamRunGeneration--
	tx := h.mustBeginTx(t)
	if _, err := s.Checkpoints.PutTx(context.Background(), tx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), request); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("stale checkpoint retry err=%v", err)
	}
	if h.readRun(t, parked.RunID).Status != StatusParked {
		t.Fatal("stale checkpoint resumed run")
	}
}

func TestRuntimeRetryRejectsContinuationFromEarlierGeneration(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	if _, err := s.Retry(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	oldTask, err := h.tasks.Claim(context.Background(), "stale-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", IdentityKind: taskqueue.IdentityTeamWorkflow, RunSnapshotID: parked.RunSnapshotID,
	})
	if err != nil || oldTask == nil {
		t.Fatalf("claim first retry: %v", err)
	}
	running := h.readRun(t, parked.RunID)
	checkpoint := retryCheckpoint(t, h, running)
	if _, err := h.executor.parkRunning(context.Background(), running, oldTask, *running.CurrentExecutorID, RuntimePark{
		NodeID: checkpoint.NodeID, CompletedOutputs: checkpoint.CompletedOutputs,
		WaitKind: WaitRuntime, WaitDetail: parked.WaitDetail, UsageComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	before := h.readRun(t, parked.RunID)
	if err := h.executor.processRuntimeRetry(context.Background(), oldTask, "stale-worker"); err != nil {
		t.Fatal(err)
	}
	if h.runtime.resumeCalls != 0 || h.readRun(t, parked.RunID).Generation != before.Generation {
		t.Fatal("old continuation executed the current checkpoint")
	}
	h.assertTask(t, oldTask.ID, taskqueue.StatusFailed, "", string(ErrorCodeIdentityMismatch))
}

type retryCheckpointRuntime struct {
	RuntimeRunner
	checkpoint WorkflowCheckpointV1
	calls      int
}

func (r *retryCheckpointRuntime) ResumeCheckpoint(_ context.Context, _ TeamRun, _ *taskqueue.Task, checkpoint WorkflowCheckpointV1) (RuntimeResult, error) {
	r.calls++
	r.checkpoint = checkpoint
	return RuntimeResult{Status: RuntimeCompleted, Output: checkpoint.CompletedOutputs["research"], UsageComplete: true}, nil
}

type failingRetryAcknowledgement struct{ ExecutorTaskStore }

func (failingRetryAcknowledgement) CompleteClaimed(context.Context, string, string, json.RawMessage, string) error {
	return errors.New("acknowledgement unavailable")
}

func TestRuntimeRetryRetainsOutputsAndAcknowledgesCompletedRunAfterCrash(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	before := retryCheckpoint(t, h, parked)
	runtime := &retryCheckpointRuntime{RuntimeRunner: h.runtime}
	h.executor.Runtime = runtime
	h.executor.Tasks = failingRetryAcknowledgement{h.executor.Tasks}
	if _, err := s.Retry(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := h.executor.ProcessNext(context.Background(), "first-worker"); err == nil {
		t.Fatal("missing expected acknowledgement failure")
	}
	if h.readRun(t, parked.RunID).Status != StatusSucceeded || runtime.calls != 1 ||
		!reflect.DeepEqual(before.CompletedOutputs, runtime.checkpoint.CompletedOutputs) || runtime.checkpoint.NodeID != "deliver" {
		t.Fatalf("resume did not retain outputs at selected stage: %#v", runtime.checkpoint)
	}
	// Reclaim the unacknowledged task after the worker is lost.
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_task_queue SET status='queued',worker_id=NULL,lease_expires_at=NULL WHERE source='runtime_retry'`); err != nil {
		t.Fatal(err)
	}
	h.executor.Tasks = h.tasks
	if processed, err := h.executor.ProcessNext(context.Background(), "replacement-worker"); err != nil || !processed {
		t.Fatalf("reclaim completed retry: processed=%v err=%v", processed, err)
	}
	if runtime.calls != 1 || h.runtime.executeCalls != 0 {
		t.Fatal("reclaimed terminal task reran workflow")
	}
	h.assertTask(t, runtimeRetryTaskID(parked.WorkspaceID, parked.RunID, request.NodeID, parked.ResumeGeneration), taskqueue.StatusCompleted, parked.RunID, "")
}

func TestRuntimeRetrySourceTaskReclaimCannotStealContinuation(t *testing.T) {
	for _, observation := range []string{"before_park", "parked", "retry_queued"} {
		t.Run(observation, func(t *testing.T) {
			h, parked, s, request := seedRuntimeRetry(t)
			observed := parked
			if observation == "before_park" {
				observed.Status = StatusRunning
				observed.Generation--
				observed.ResumeGeneration--
				executorID := executorIdentity(parked.SourceTaskID, "seed-worker")
				observed.CurrentExecutorID = &executorID
				observed.WaitKind, observed.WaitDetail, observed.ResumeTokenHash, observed.CheckpointRef = nil, nil, nil, nil
			}
			if _, err := s.Retry(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			before := h.readRun(t, parked.RunID)
			if observation == "retry_queued" {
				observed = before
			}
			// The source worker parked durably, then died before acknowledging its task.
			if _, err := h.pool.Exec(context.Background(), `UPDATE weave_task_queue SET status='running',worker_id='source-reclaim',lease_expires_at=$2 WHERE id=$1`,
				parked.SourceTaskID, h.now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			source, err := h.tasks.Get(context.Background(), parked.WorkspaceID, parked.SourceTaskID)
			if err != nil {
				t.Fatal(err)
			}
			h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"done":true}`), UsageComplete: true}
			if err := h.executor.processConsumedWorkflowRun(context.Background(), source, "source-reclaim", observed); err != nil {
				t.Fatal(err)
			}
			if h.runtime.resumeCalls != 0 || !reflect.DeepEqual(before, h.readRun(t, parked.RunID)) {
				t.Fatal("source task stole and executed the new retry continuation")
			}
			h.assertTask(t, source.ID, taskqueue.StatusCompleted, parked.RunID, "")
			if processed, err := h.executor.ProcessNext(context.Background(), "retry-worker"); err != nil || !processed {
				t.Fatalf("original continuation no longer executable: processed=%v err=%v", processed, err)
			}
			if h.runtime.resumeCalls != 1 || h.readRun(t, parked.RunID).Status != StatusSucceeded {
				t.Fatal("retry continuation did not finish once")
			}
		})
	}
}
