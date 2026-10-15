package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestModelCancellationActivityPersistsAfterCallerCancelledRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTaskBeforeAdmission(t, "model-activity-cancelled")
	store := &PGActivityStore{Transactions: h.pool}
	runtime := &WorkflowSerialRuntime{Activities: store}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), runtimeActivityScopeKey{}, runtimeActivityScope{
		NodeID: "player", MemberID: "worker-1", MemberVersion: 1,
	}))
	cancel()
	runtime.modelObserver(run)(ctx, workflow.ModelCallEvent{
		CallID: "model-call-1", Model: "frozen-model", Phase: "cancelled", ElapsedMS: 123,
		ErrorClass: "context_cancelled",
	})
	events, err := store.List(context.Background(), run.WorkspaceID, run.RunID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("cancelled observation lost: events=%d err=%v", len(events), err)
	}
	event := events[0]
	var detail map[string]any
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "model_cancelled" || event.NodeID != "player" || event.MemberID != "worker-1" ||
		detail["model_call_id"] != "model-call-1" || detail["error_class"] != "context_cancelled" {
		t.Fatalf("model cancellation identity missing: event=%+v detail=%v", event, detail)
	}
	if hidden, err := store.List(context.Background(), "another-workspace", run.RunID, 10); err != nil || len(hidden) != 0 {
		t.Fatalf("model observation crossed workspace: events=%d err=%v", len(hidden), err)
	}
}
