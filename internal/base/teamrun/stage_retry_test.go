package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

func TestStageRetryRequeuesOnlyExactInfrastructureFailedBranch(t *testing.T) {
	h := newProcessNextHarness(t)
	_ = h.enqueueWorkflowTask(t, "run-stage-retry")
	run := h.readRun(t, "run-stage-retry")
	wait := json.RawMessage(`{"wait_type":"fanout_group","parked":true,"intent_id":"intent-1","group_id":"group-1","generation":"generation-1","resume_token":"token-1","parent_run_id":"run-stage-retry","join_node_id":"join"}`)
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_team_runs SET status='parked',wait_kind='fanout',wait_detail=$1 WHERE workspace_id=$2 AND run_id=$3`, wait, run.WorkspaceID, run.RunID); err != nil {
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

	result, err := (&StageRetryService{Runs: NewPGStore(), Tasks: h.tasks}).Retry(context.Background(), StageRetryRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "physics",
	})
	if err != nil {
		t.Fatalf("retry stage: %v", err)
	}
	if result.Status != "queued" || !result.PreservedCompletedStages {
		t.Fatalf("retry result = %#v", result)
	}
	replayed, err := (&StageRetryService{Runs: NewPGStore(), Tasks: h.tasks}).Retry(context.Background(), StageRetryRequest{
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
