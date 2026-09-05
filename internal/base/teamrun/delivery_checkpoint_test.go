package teamrun

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDeliveryEvidenceSurvivesRealCheckpointParkAndRetry(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, run := h.seedRunningWorkflowTask(t, "run-delivery-evidence")
	task, err := h.tasks.Get(context.Background(), run.WorkspaceID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	message := `delivery_artifact_uncollected: file_not_collected: "outputs/acceptance.md"`
	parked, err := h.executor.parkRunning(context.Background(), run, task, *run.CurrentExecutorID, RuntimePark{
		NodeID: "deliver", WaitKind: WaitRuntime, WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`),
		CompletedOutputs: map[string]json.RawMessage{"finalizer": json.RawMessage(`"outputs/acceptance.md"`)},
		DeliveryErrors:   map[string]string{"finalizer": message}, UsageComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := retryCheckpoint(t, h, parked)
	if before.DeliveryErrors["finalizer"] != message {
		t.Fatalf("delivery evidence was lost during park: %#v", before.DeliveryErrors)
	}
	service := &StageRetryService{Transactions: h.pool, Runs: &PGStore{Transactions: h.pool}, Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks, Now: h.executor.Now}
	if _, err := service.Retry(context.Background(), StageRetryRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: "deliver", IdempotencyKey: "delivery-evidence-retry",
	}); err != nil {
		t.Fatal(err)
	}
	after := retryCheckpoint(t, h, h.readRun(t, run.RunID))
	if after.DeliveryErrors["finalizer"] != message || string(after.CompletedOutputs["finalizer"]) != string(before.CompletedOutputs["finalizer"]) {
		t.Fatalf("retry replaced final delivery evidence: %#v", after)
	}
}
