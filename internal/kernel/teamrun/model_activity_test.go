package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
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
		detail["model_call_id"] != "model-call-1" || detail["error_class"] != "context_cancelled" ||
		detail["local_stopped"] != true || detail["upstream_cancel"] != "unknown" {
		t.Fatalf("model cancellation identity missing: event=%+v detail=%v", event, detail)
	}
	if hidden, err := store.List(context.Background(), "another-workspace", run.RunID, 10); err != nil || len(hidden) != 0 {
		t.Fatalf("model observation crossed workspace: events=%d err=%v", len(hidden), err)
	}
}

// The completed and failed activities carry the provider facts that were
// observed, 200 and observed zeros included, and leave out what was not.
func TestModelActivityRecordsObservedProviderFactsRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	_, run := h.seedRunningWorkflowTaskBeforeAdmission(t, "model-activity-facts")
	store := &PGActivityStore{Transactions: h.pool}
	runtime := &WorkflowSerialRuntime{Activities: store}
	ctx := context.WithValue(context.Background(), runtimeActivityScopeKey{}, runtimeActivityScope{NodeID: "player", MemberID: "worker-1", MemberVersion: 1})
	zero := 0
	runtime.modelObserver(run)(ctx, workflow.ModelCallEvent{
		CallID: "call-observed", Model: "frozen-model", Phase: "failed", ErrorClass: "provider_output_limit", Attempts: 2,
		Facts: &llmrouter.CallFacts{HTTPStatus: 200, ProviderRequestID: "req_1", End: "empty_response", FinishReason: "length",
			DoneSeen: true, ContentBytes: &zero, RequestBytes: 68000,
			Request: &llmrouter.CallRequest{Model: "deepseek-flash", Stream: true, ReasoningEffort: "low", ResponseFormat: "json_object", OutputLimitField: "max_tokens", OutputLimit: 4096}},
	})
	runtime.modelObserver(run)(ctx, workflow.ModelCallEvent{CallID: "call-unobserved", Model: "frozen-model", Phase: "completed"})
	events, err := store.List(context.Background(), run.WorkspaceID, run.RunID, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	details := map[string]map[string]any{}
	for _, event := range events {
		var detail map[string]any
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		details[detail["model_call_id"].(string)] = detail
	}
	observed := details["call-observed"]
	request, _ := observed["request"].(map[string]any)
	if observed["http_status"] != float64(200) || observed["provider_request_id"] != "req_1" || observed["protocol_end"] != "empty_response" ||
		observed["finish_reason"] != "length" || observed["content_bytes"] != float64(0) || observed["request_bytes"] != float64(68000) ||
		observed["attempts"] != float64(2) || request["output_limit_field"] != "max_tokens" || request["output_limit"] != float64(4096) || request["model"] != "deepseek-flash" {
		t.Fatalf("observed facts detail = %v", observed)
	}
	for _, key := range []string{"http_status", "provider_request_id", "protocol_end", "content_bytes", "request", "attempts"} {
		if _, present := details["call-unobserved"][key]; present {
			t.Fatalf("unobserved call reports %s: %v", key, details["call-unobserved"])
		}
	}
}
