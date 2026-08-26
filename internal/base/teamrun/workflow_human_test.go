package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestHumanResumeQueuesContinuationAdvancesCheckpointAndBindsDigest(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeParked,
		Park: &RuntimePark{
			NodeID: "review", CompletedOutputs: map[string]json.RawMessage{"draft": json.RawMessage(`{"text":"ready"}`)},
			WaitKind: WaitHuman,
			WaitDetail: json.RawMessage(`{
				"schema_version":1,"wait_type":"human","node_id":"review","success_node_id":"deliver",
				"resume_schema":{"type":"object","properties":{"decision":{"type":"string"}},"required":["decision"]},
				"task":{"title":"终审","instructions":"确认交付物","audience_ref":"editor"}
			}`),
			UsageComplete: true,
		},
	}
	sourceTaskID := h.enqueueWorkflowTask(t, "run-human-resume")
	processed, err := h.executor.ProcessNext(context.Background(), "worker-1")
	if err != nil || !processed {
		t.Fatalf("park human run: processed=%v err=%v", processed, err)
	}
	h.assertTask(t, sourceTaskID, "completed", "run-human-resume", "")
	h.assertRun(t, "run-human-resume", StatusParked, nil)

	payload := json.RawMessage(`{"decision":"approve"}`)
	digest := sha256.Sum256(payload)
	service := &HumanResumeService{
		Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks,
		Now: func() time.Time { return h.now.Add(time.Second) },
	}
	request := CompleteHumanWaitRequest{
		WorkspaceID: "workspace-1", RunID: "run-human-resume", Payload: payload, PayloadDigest: digest[:],
		IdempotencyKey: "approve-once", Actor: "user-1", OccurredAt: h.now.Add(time.Second),
	}
	completed, err := service.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("complete human wait: %v", err)
	}
	if completed.Idempotent || completed.TaskID == "" || completed.Run.Status != StatusRunning {
		t.Fatalf("unexpected completion: %#v", completed)
	}
	tx := h.mustBeginTx(t)
	checkpoint, err := NewPGCheckpointStore().GetTx(context.Background(), tx, "workspace-1", "run-human-resume")
	if err != nil {
		t.Fatalf("read advanced checkpoint: %v", err)
	}
	if checkpoint.NodeID != "deliver" || string(checkpoint.CompletedOutputs["review"]) != string(payload) {
		t.Fatalf("checkpoint was not advanced with payload: %#v", checkpoint)
	}
	_ = tx.Rollback(context.Background())

	replay, err := service.Complete(context.Background(), request)
	if err != nil || !replay.Idempotent || replay.TaskID != completed.TaskID {
		t.Fatalf("idempotent replay: result=%#v err=%v", replay, err)
	}
	conflictingPayload := json.RawMessage(`{"decision":"reject"}`)
	conflictingDigest := sha256.Sum256(conflictingPayload)
	conflictRequest := request
	conflictRequest.Payload = conflictingPayload
	conflictRequest.PayloadDigest = conflictingDigest[:]
	if _, err := service.Complete(context.Background(), conflictRequest); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("same key different payload err=%v, want state conflict", err)
	}

	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"delivered":true}`), UsageComplete: true}
	processed, err = h.executor.ProcessNext(context.Background(), "worker-2")
	if err != nil || !processed {
		t.Fatalf("process human continuation: processed=%v err=%v", processed, err)
	}
	if h.runtime.resumeCalls != 1 || h.runtime.executeCalls != 1 {
		t.Fatalf("runtime calls execute=%d resume=%d, want 1/1", h.runtime.executeCalls, h.runtime.resumeCalls)
	}
	h.assertTask(t, completed.TaskID, "completed", "run-human-resume", "")
	h.assertRun(t, "run-human-resume", StatusSucceeded, nil)
}
