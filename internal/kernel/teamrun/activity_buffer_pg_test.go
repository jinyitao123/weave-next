package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestBufferedObservationsCommitBeforeMemberCompletionRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTaskBeforeAdmission(t, "buffered-observations")
	store := &PGActivityStore{Transactions: h.pool}
	buffer := NewBufferedActivityRecorder(store)
	defer buffer.Close()
	runtime := &WorkflowSerialRuntime{Activities: buffer}
	ctx := context.WithValue(t.Context(), runtimeActivityScopeKey{}, runtimeActivityScope{NodeID: "member", MemberID: "worker-1", MemberVersion: 1})
	observer := runtime.modelObserver(run)
	started := workflow.ModelCallEvent{CallID: "physical-call", Model: "model", Phase: workflow.ModelCallStarted}
	observer(ctx, started)
	observer(ctx, started) // replay of a committed observation is one row
	observer(ctx, workflow.ModelCallEvent{CallID: "physical-call", Model: "model", Phase: workflow.ModelCallCompleted, Phases: map[workflow.ModelCallPhase]int64{workflow.ModelCallFirstChunk: 12}})
	if err := buffer.Record(ctx, ActivityEvent{WorkspaceID: run.WorkspaceID, RunID: run.RunID, Kind: "member_completed", Detail: json.RawMessage(`{}`), OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, err := store.List(ctx, run.WorkspaceID, run.RunID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var relevant []ActivityEvent
	for _, event := range events {
		if event.Kind == "model_started" || event.Kind == "model_completed" || event.Kind == "member_completed" {
			relevant = append(relevant, event)
		}
	}
	if len(relevant) != 3 || relevant[0].Kind != "model_started" || relevant[1].Kind != "model_completed" || relevant[2].Kind != "member_completed" {
		t.Fatalf("observation order or deduplication lost: %+v", relevant)
	}
}

func TestObservationQueueDeadlineLeavesDurableGapRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTaskBeforeAdmission(t, "observation-gap")
	store := &PGActivityStore{Transactions: h.pool}
	// Hold consumption until the bounded queue has actually timed out, then
	// resume it against real PostgreSQL and independently read the gap record.
	buffer := &BufferedActivityRecorder{store: store, queue: make(chan activityWrite, 1), stop: make(chan struct{}), done: make(chan struct{})}
	event := ActivityEvent{WorkspaceID: run.WorkspaceID, RunID: run.RunID, Kind: "model_started", Detail: json.RawMessage(`{}`), OccurredAt: time.Now()}
	if err := buffer.Record(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	event.Kind = "model_completed"
	if err := buffer.Record(ctx, event); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue did not report its deadline: %v", err)
	}
	go buffer.run()
	defer buffer.Close()
	if err := buffer.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := store.List(t.Context(), run.WorkspaceID, run.RunID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "model_observation_gap" {
			var detail map[string]any
			if json.Unmarshal(event.Detail, &detail) != nil || detail["dropped"] != float64(1) {
				t.Fatalf("invalid gap: %s", event.Detail)
			}
			return
		}
	}
	t.Fatal("a lost observation had no durable gap")
}
