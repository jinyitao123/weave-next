package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
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
	h.executor.Now = func() time.Time { return h.now.Add(2 * time.Second) }
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

func TestHumanResumeReviewerDoesNotReplaceFrozenExecutionSubject(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
		NodeID: "review", WaitKind: WaitHuman, UsageComplete: true,
		WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"human","node_id":"review","success_node_id":"deliver","resume_schema":{"type":"object"},"task":{"title":"复核","instructions":"确认"}}`),
	}}
	h.enqueueWorkflowTask(t, "run-cross-reviewer")
	if processed, err := h.executor.ProcessNext(context.Background(), "worker-park"); err != nil || !processed {
		t.Fatalf("park run: processed=%v error=%v", processed, err)
	}
	payload := json.RawMessage(`{"decision":"approved"}`)
	digest := sha256.Sum256(payload)
	service := &HumanResumeService{Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks}
	reviewer, err := execution.BindSubject(context.Background(), execution.Subject{WorkspaceID: "workspace-1", UserID: "reviewer-2"})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Complete(reviewer, CompleteHumanWaitRequest{
		WorkspaceID: "workspace-1", RunID: "run-cross-reviewer", Payload: payload, PayloadDigest: digest[:],
		IdempotencyKey: "reviewer-accept", Actor: "reviewer-2", OccurredAt: h.now.Add(time.Second),
	})
	if err != nil || accepted.Idempotent {
		t.Fatalf("cross-reviewer complete: result=%#v error=%v", accepted, err)
	}
	var actorSubject []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT actor_subject FROM weave_task_queue WHERE id=$1`, accepted.TaskID).Scan(&actorSubject); err != nil {
		t.Fatal(err)
	}
	var subject execution.Subject
	if err := json.Unmarshal(actorSubject, &subject); err != nil {
		t.Fatal(err)
	}
	if subject.UserID != "user-1" {
		t.Fatalf("continuation subject=%+v, want original user-1", subject)
	}
}

func TestHumanTimeoutQueuesContinuationAndAdvancesTimeoutEdge(t *testing.T) {
	h := newProcessNextHarness(t)
	parkHumanRunForTimeout(t, h, "run-human-timeout")
	sweeper := &HumanTimeoutSweeper{
		Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks,
		Now: func() time.Time { return h.now.Add(time.Second) }, BatchSize: 10,
	}
	count, err := sweeper.Sweep(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("sweep human timeout: count=%d err=%v", count, err)
	}
	h.assertRun(t, "run-human-timeout", StatusRunning, nil)
	tx := h.mustBeginTx(t)
	checkpoint, err := NewPGCheckpointStore().GetTx(context.Background(), tx, "workspace-1", "run-human-timeout")
	if err != nil {
		t.Fatalf("read timeout checkpoint: %v", err)
	}
	if checkpoint.NodeID != "timeout-deliver" || string(checkpoint.CompletedOutputs["review"]) != `{"node_id":"review","status":"timeout"}` {
		t.Fatalf("unexpected timeout checkpoint: %#v", checkpoint)
	}
	_ = tx.Rollback(context.Background())
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"timed_out":true}`), UsageComplete: true}
	h.executor.Now = func() time.Time { return h.now.Add(2 * time.Second) }
	processed, err := h.executor.ProcessNext(context.Background(), "worker-timeout")
	if err != nil || !processed || h.runtime.resumeCalls != 1 {
		t.Fatalf("process timeout continuation: processed=%v resume=%d err=%v", processed, h.runtime.resumeCalls, err)
	}
	h.assertRun(t, "run-human-timeout", StatusSucceeded, nil)
}

func TestHumanCompleteAndTimeoutOnlyOneCASWins(t *testing.T) {
	h := newProcessNextHarness(t)
	parkHumanRunForTimeout(t, h, "run-human-race")
	payload := json.RawMessage(`{"decision":"approve"}`)
	digest := sha256.Sum256(payload)
	service := &HumanResumeService{
		Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks,
		Now: func() time.Time { return h.now.Add(time.Second) },
	}
	sweeper := &HumanTimeoutSweeper{
		Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks,
		Now: func() time.Time { return h.now.Add(time.Second) }, BatchSize: 10,
	}
	start := make(chan struct{})
	completeResult := make(chan error, 1)
	timeoutResult := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		<-start
		_, err := service.Complete(context.Background(), CompleteHumanWaitRequest{
			WorkspaceID: "workspace-1", RunID: "run-human-race", Payload: payload, PayloadDigest: digest[:],
			IdempotencyKey: "race-complete", Actor: "user-1", OccurredAt: h.now.Add(time.Second),
		})
		completeResult <- err
	}()
	go func() {
		<-start
		count, err := sweeper.Sweep(context.Background())
		timeoutResult <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()
	close(start)
	completeErr := <-completeResult
	timeout := <-timeoutResult
	completeWon := completeErr == nil
	timeoutWon := timeout.err == nil && timeout.count == 1
	if completeWon == timeoutWon {
		t.Fatalf("exactly one CAS must win: complete_err=%v timeout_count=%d timeout_err=%v", completeErr, timeout.count, timeout.err)
	}
	if completeErr != nil && !errors.Is(completeErr, ErrTeamRunStateConflict) {
		t.Fatalf("losing complete error = %v", completeErr)
	}
	var transitions int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_team_run_transitions
		WHERE workspace_id='workspace-1' AND run_id='run-human-race'
		  AND from_status='parked' AND to_status='running'`).Scan(&transitions); err != nil {
		t.Fatalf("count resume transitions: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("parked->running transitions = %d, want 1", transitions)
	}
}

func TestHumanTaskReaderUsesStableUpdatedAtRunIDCursor(t *testing.T) {
	h := newProcessNextHarness(t)
	for _, runID := range []string{"run-human-list-a", "run-human-list-b"} {
		h.runtime.executeResult = RuntimeResult{
			Status: RuntimeParked,
			Park: &RuntimePark{
				NodeID: "review", CompletedOutputs: map[string]json.RawMessage{"draft": json.RawMessage(`{"deliverable_ref":"artifact-1"}`)},
				WaitKind: WaitHuman,
				WaitDetail: json.RawMessage(`{
					"schema_version":1,"wait_type":"human","node_id":"review","success_node_id":"deliver",
					"resume_schema":{"type":"object"},
					"task":{"title":"终审","instructions":"确认交付物","audience_ref":"editor"}
				}`),
				UsageComplete: true,
			},
		}
		h.enqueueWorkflowTask(t, runID)
		processed, err := h.executor.ProcessNext(context.Background(), "worker-"+runID)
		if err != nil || !processed {
			t.Fatalf("park list run %q: processed=%v err=%v", runID, processed, err)
		}
	}
	reader := &HumanTaskReader{Pool: h.pool}
	count, err := reader.Count(context.Background(), "workspace-1")
	if err != nil || count != 2 {
		t.Fatalf("task count = %d err = %v, want 2", count, err)
	}
	first, more, err := reader.List(context.Background(), "workspace-1", nil, "", 1)
	if err != nil || !more || len(first) != 1 {
		t.Fatalf("first page: items=%d more=%v err=%v", len(first), more, err)
	}
	if first[0].Run.RunID != "run-human-list-b" || first[0].Detail.Task.AudienceRef != "editor" ||
		first[0].CompletedOutputs != nil {
		t.Fatalf("unexpected first task: %#v", first[0])
	}
	detail, err := reader.Get(context.Background(), "workspace-1", first[0].Run.RunID)
	if err != nil || string(detail.CompletedOutputs["draft"]) != `{"deliverable_ref":"artifact-1"}` {
		t.Fatalf("task detail = %#v err = %v", detail, err)
	}
	second, more, err := reader.List(
		context.Background(), "workspace-1", &first[0].Run.UpdatedAt, first[0].Run.RunID, 1,
	)
	if err != nil || more || len(second) != 1 || second[0].Run.RunID != "run-human-list-a" {
		t.Fatalf("second page: items=%#v more=%v err=%v", second, more, err)
	}
}

func parkHumanRunForTimeout(t *testing.T, h *processNextHarness, runID string) {
	t.Helper()
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeParked,
		Park: &RuntimePark{
			NodeID: "review", CompletedOutputs: map[string]json.RawMessage{}, WaitKind: WaitHuman,
			WaitDetail: json.RawMessage(`{
				"schema_version":1,"wait_type":"human","node_id":"review",
				"success_node_id":"deliver","timeout_node_id":"timeout-deliver",
				"resume_schema":{"type":"object","properties":{"decision":{"type":"string"}},"required":["decision"]},
				"task":{"title":"终审","instructions":"确认交付物"},
				"deadline_at":"2026-08-25T09:59:00Z"
			}`),
			UsageComplete: true,
		},
	}
	h.enqueueWorkflowTask(t, runID)
	processed, err := h.executor.ProcessNext(context.Background(), "worker-park")
	if err != nil || !processed {
		t.Fatalf("park human timeout run: processed=%v err=%v", processed, err)
	}
	h.assertRun(t, runID, StatusParked, nil)
}
