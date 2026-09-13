package teamrun

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

func TestStageRetryRequeuesOnlyExactInfrastructureFailedBranch(t *testing.T) {
	h, run, taskID := seedFanoutStageRetry(t)
	result, err := (&StageRetryService{Runs: &PGStore{Transactions: h.pool}, Tasks: h.tasks}).Retry(context.Background(), StageRetryRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics",
	})
	if err != nil {
		t.Fatalf("retry stage: %v", err)
	}
	if result.Status != "queued" || !result.PreservedCompletedStages {
		t.Fatalf("retry result = %#v", result)
	}
	replayed, err := (&StageRetryService{Runs: &PGStore{Transactions: h.pool}, Tasks: h.tasks}).Retry(context.Background(), StageRetryRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics",
	})
	if err != nil || replayed.Status != "queued" {
		t.Fatalf("replay exact stage retry: result=%#v err=%v", replayed, err)
	}
	stored, err := h.tasks.Get(context.Background(), run.WorkspaceID, taskID)
	if err != nil || stored.Status != taskqueue.StatusQueued || stored.Error != "" {
		t.Fatalf("retried task status=%q error=%q err=%v", stored.Status, stored.Error, err)
	}
}

func seedFanoutStageRetry(t *testing.T) (*processNextHarness, TeamRun, string) {
	t.Helper()
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTask(t, "run-stage-retry")
	wait := json.RawMessage(`{"wait_type":"fanout_group","parked":true,"intent_id":"intent-1","group_id":"group-1","generation":"generation-1","resume_token":"token-1","parent_run_id":"run-stage-retry","join_node_id":"join"}`)
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_team_runs SET status='parked',current_executor_id=NULL,wait_kind='fanout',wait_detail=$1,
        resume_token_hash='seed-token',checkpoint_ref='seed-checkpoint' WHERE workspace_id=$2 AND run_id=$3`, wait, run.WorkspaceID, run.RunID); err != nil {
		t.Fatalf("park run: %v", err)
	}
	taskID, err := fanout.DeriveLegTaskID("group-1", "physics", "generation-1")
	if err != nil {
		t.Fatalf("derive task id: %v", err)
	}
	task := &taskqueue.Task{
		ID: taskID, WorkspaceID: run.WorkspaceID, IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: run.WorkflowID, WorkflowVersion: run.WorkflowVersion,
		RunSnapshotID: run.RunSnapshotID, Source: "fanout", Kind: "team_workflow", ContextKey: "group-1",
		Payload: json.RawMessage(`{"schema_version":1,"kind":"fanout_leg"}`),
	}
	if err := h.tasks.Enqueue(context.Background(), task); err != nil {
		t.Fatalf("enqueue failed branch: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_task_queue SET status='failed',error='team_run_execution_unrecoverable: request timed out' WHERE workspace_id=$1 AND id=$2`, run.WorkspaceID, taskID); err != nil {
		t.Fatalf("fail branch: %v", err)
	}

	return h, run, taskID
}

func TestStageRetryResumesParkedCurrentRuntimeStageFromCheckpoint(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, running := h.seedRunningWorkflowTask(t, "run-runtime-retry")
	task, err := h.tasks.Get(context.Background(), running.WorkspaceID, taskID)
	if err != nil {
		t.Fatalf("read source task: %v", err)
	}
	detail, err := json.Marshal(RuntimeWaitDetailV1{SchemaVersion: 1, WaitType: "runtime", NodeID: "deliver"})
	if err != nil {
		t.Fatalf("marshal runtime wait: %v", err)
	}
	parked, err := h.executor.parkRunning(context.Background(), running, task,
		executorIdentity(taskID, "seed-worker"), RuntimePark{
			NodeID: "deliver", CompletedOutputs: map[string]json.RawMessage{"research": json.RawMessage(`{"done":true}`)},
			WaitKind: WaitRuntime, WaitDetail: detail, UsageComplete: true,
		})
	if err != nil {
		t.Fatalf("park runtime stage: %v", err)
	}
	if parked.Status != StatusParked || parked.WaitKind == nil || *parked.WaitKind != WaitRuntime {
		t.Fatalf("parked run = %#v", parked)
	}
	service := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool}, Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks,
		Now: func() time.Time { return h.now.Add(time.Minute) }}
	result, err := service.Retry(context.Background(), StageRetryRequest{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID, NodeID: "deliver",
	})
	if err != nil {
		t.Fatalf("retry current runtime stage: %v", err)
	}
	if result.Status != taskqueue.StatusQueued || !result.PreservedCompletedStages || len(result.AffectedNodeIDs) != 1 {
		t.Fatalf("retry result = %#v", result)
	}
	resumed := h.readRun(t, running.RunID)
	if resumed.Status != StatusRunning || resumed.CurrentExecutorID == nil {
		t.Fatalf("resumed run = %#v", resumed)
	}
	checkpoint, err := NewPGCheckpointStore().GetTx(context.Background(), h.mustBeginTx(t), running.WorkspaceID, running.RunID)
	if err != nil {
		t.Fatalf("read retry checkpoint: %v", err)
	}
	if checkpoint.NodeID != "deliver" || len(checkpoint.CompletedOutputs) != 1 {
		t.Fatalf("retry checkpoint = %#v", checkpoint)
	}
	replayed, err := service.Retry(context.Background(), StageRetryRequest{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID, NodeID: "deliver",
	})
	if err != nil || replayed.Status != taskqueue.StatusQueued {
		t.Fatalf("replay current-stage retry: result=%#v err=%v", replayed, err)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_task_queue SET status='completed' WHERE workspace_id=$1 AND id=$2`, running.WorkspaceID, taskID); err != nil {
		t.Fatalf("retire source task: %v", err)
	}
	h.executor.Now = func() time.Time { return h.now.Add(2 * time.Minute) }
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"done":true}`), UsageComplete: true}
	processed, err := h.executor.ProcessNext(context.Background(), "runtime-retry-worker")
	if err != nil || !processed {
		t.Fatalf("process runtime retry: processed=%v err=%v", processed, err)
	}
	completed := h.readRun(t, running.RunID)
	if completed.Status != StatusSucceeded {
		t.Fatalf("completed retried run = %#v", completed)
	}
}
