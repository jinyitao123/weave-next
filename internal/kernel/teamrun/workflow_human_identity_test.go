package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestHumanResumeBindsQuestionAndReplaysAfterNextWaitRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	ctx := context.Background()
	const runID = "run-human-question-identity"
	question := func(title, valueType string) RuntimeResult {
		detail, err := json.Marshal(HumanWaitDetailV1{
			SchemaVersion: 1, WaitType: "human", NodeID: "review", SuccessNodeID: "deliver",
			ResumeSchema: json.RawMessage(`{"type":"object","properties":{"decision":{"type":"` + valueType + `"}},"required":["decision"]}`),
			Task:         HumanTaskDetail{Title: title, Instructions: "Answer this question"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
			NodeID: "review", CompletedOutputs: map[string]json.RawMessage{"draft": json.RawMessage(`"retained work"`)},
			WaitKind: WaitHuman, WaitDetail: detail, UsageComplete: true,
		}}
	}
	h.runtime.executeResult = question("Question A", "string")
	h.enqueueWorkflowTask(t, runID)
	if processed, err := h.executor.ProcessNext(ctx, "worker-question-a"); err != nil || !processed {
		t.Fatalf("park A: processed=%v error=%v", processed, err)
	}
	reader := &HumanTaskReader{Pool: h.pool}
	first, err := reader.Get(ctx, "workspace-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	firstID := HumanInteractionID(first.Run)
	if firstID == "" {
		t.Fatal("question A has no identity")
	}
	changedTimestamp := first.Run
	changedTimestamp.UpdatedAt = changedTimestamp.UpdatedAt.Add(time.Minute)
	if HumanInteractionID(changedTimestamp) != firstID {
		t.Fatal("question identity depends on mutable display timestamp")
	}
	service := &HumanResumeService{Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks}
	payload := json.RawMessage(`{"decision":"answer A"}`)
	digest := sha256.Sum256(payload)
	validationCalls := 0
	request := CompleteHumanWaitRequest{
		WorkspaceID: "workspace-1", RunID: runID, InteractionID: firstID,
		Payload: payload, PayloadDigest: digest[:], IdempotencyKey: "answer-a", Actor: "user-1",
		OccurredAt: h.now.Add(time.Second), ValidatePayload: func(schema, value json.RawMessage) error {
			validationCalls++
			if string(schema) != string(first.Detail.ResumeSchema) || string(value) != string(payload) {
				t.Fatal("validator did not receive locked A schema and payload")
			}
			return nil
		},
	}
	accepted, err := service.Complete(ctx, request)
	if err != nil || accepted.Idempotent {
		t.Fatalf("answer A: result=%#v error=%v", accepted, err)
	}

	// The continuation reaches the same node in a new wait generation, with a
	// schema that would reject A's answer if replay were incorrectly revalidated.
	h.runtime.resumeResult = question("Question B", "boolean")
	h.executor.Now = func() time.Time { return h.now.Add(2 * time.Second) }
	if processed, err := h.executor.ProcessNext(ctx, "worker-question-b"); err != nil || !processed {
		t.Fatalf("park B: processed=%v error=%v", processed, err)
	}
	second, err := reader.Get(ctx, "workspace-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	secondID := HumanInteractionID(second.Run)
	if secondID == "" || secondID == firstID || second.Detail.NodeID != first.Detail.NodeID {
		t.Fatalf("same-node new wait not distinguished: A=%q B=%q", firstID, secondID)
	}
	countTasks := func() int {
		var count int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE workspace_id='workspace-1' AND context_key=$1`, runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeCount := countTasks()
	stale := request
	stale.IdempotencyKey = "old-form-new-click"
	if _, err := service.Complete(ctx, stale); !errors.Is(err, ErrTeamRunResumeStale) {
		t.Fatalf("stale A answer reached B: %v", err)
	}
	replay, err := service.Complete(ctx, request)
	if err != nil || !replay.Idempotent || replay.TaskID != accepted.TaskID || replay.Run.Status != StatusParked {
		t.Fatalf("lost A response did not replay while B waits: result=%#v error=%v", replay, err)
	}
	if validationCalls != 1 || countTasks() != beforeCount {
		t.Fatal("stale answer or replay validated B or enqueued another continuation")
	}
	after, err := reader.Get(ctx, "workspace-1", runID)
	if err != nil || HumanInteractionID(after.Run) != secondID {
		t.Fatalf("B changed after stale/replayed A answer: %#v %v", after.Run, err)
	}

	// Reusing the accepted key for different content remains a conflict even
	// though it is checked before the current interaction identity.
	conflict := request
	conflict.Payload = json.RawMessage(`{"decision":true}`)
	conflictDigest := sha256.Sum256(conflict.Payload)
	conflict.PayloadDigest = conflictDigest[:]
	if _, err := service.Complete(ctx, conflict); !errors.Is(err, ErrTeamRunStateConflict) {
		t.Fatalf("different replay payload accepted: %v", err)
	}

	invalid := conflict
	invalid.InteractionID, invalid.IdempotencyKey = secondID, "answer-b-invalid"
	invalidPayload := errors.New("B payload rejected")
	invalid.ValidatePayload = func(schema, value json.RawMessage) error {
		if string(schema) != string(second.Detail.ResumeSchema) || string(value) != string(invalid.Payload) {
			t.Fatal("validator did not receive locked B schema")
		}
		return invalidPayload
	}
	if _, err := service.Complete(ctx, invalid); !errors.Is(err, invalidPayload) {
		t.Fatalf("locked schema validation did not abort: %v", err)
	}
	if countTasks() != beforeCount {
		t.Fatal("invalid B answer enqueued continuation")
	}
	h.assertRun(t, runID, StatusParked, nil)
}
