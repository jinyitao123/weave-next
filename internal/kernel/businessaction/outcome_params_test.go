package businessaction

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// newParamDispatcher returns a tracked dispatcher for an action with one
// declared text parameter and the frozen record "record-a".
func newParamDispatcher(t *testing.T, host contract.ToolDispatcher) (*dispatcher, string) {
	t.Helper()
	id := "forge:action:sales_quote.AdjustPrice"
	value, err := newDispatcherWithResources(host, []string{id}, map[string]actionMetadata{
		"sales_quote.AdjustPrice": {
			Name: "AdjustPrice", ObjectName: "sales_quote", Label: "调整报价明细单价", RequiresRecord: true,
			Params: []actionParam{{Name: "line_id", Type: "text", Required: true}},
		},
	}, []delegatedResource{recordResourceForTest("sales_quote", "record-a")})
	if err != nil {
		t.Fatal(err)
	}
	value.trackOutcomes = true
	value.inputRevisionID = "revision-1"
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	return value, tools[0].Name
}

func allowAll(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
	return ActionOutcomeReplay{}, nil
}

// The digest detects changed effective request content within a durable operation.
// It is independent of the model call ID and is never the production identity.
func TestForgeActionRecordsRequestDigestIndependentOfCallID(t *testing.T) {
	var events []ActionOutcomeEvent
	dispatcher, tool := newParamDispatcher(t, &outcomeTestHost{})
	ctx := outcomeTestContext(&events, allowAll, nil)
	calls := []contract.ToolCall{
		{ID: "call-1", Name: tool, Args: `{"params":{"line_id":"line-1"}}`},
		{ID: "call-2-after-restart", Name: tool, Args: `{"params":{"line_id":"line-1"}}`},
		{ID: "call-3", Name: tool, Args: `{"params":{"line_id":"line-2"}}`},
	}
	for _, call := range calls {
		if result, err := dispatcher.Dispatch(ctx, call); err != nil || result == nil || result.IsError {
			t.Fatalf("dispatch %s: result=%+v err=%v", call.ID, result, err)
		}
	}
	if len(events) != 6 {
		t.Fatalf("expected a start and a result receipt per call, got %d events", len(events))
	}
	digest := func(index int) string { return events[index].ParamsSHA256 }
	if len(digest(0)) != 64 {
		t.Fatalf("started receipt has no request digest: %+v", events[0])
	}
	if digest(0) != digest(1) {
		t.Fatalf("start and result receipts of one call carry different digests: %q vs %q", digest(0), digest(1))
	}
	if digest(0) != digest(2) {
		t.Fatalf("the same request under a new call ID produced a different digest: %q vs %q", digest(0), digest(2))
	}
	if digest(0) == digest(4) {
		t.Fatalf("different params produced the same digest %q", digest(0))
	}
}

// Parameter order in the model's JSON must not change the digest.
func TestForgeActionDigestIgnoresJSONKeyOrder(t *testing.T) {
	var events []ActionOutcomeEvent
	id := "forge:action:sales_quote.AdjustPrice"
	value, err := newDispatcherWithResources(&outcomeTestHost{}, []string{id}, map[string]actionMetadata{
		"sales_quote.AdjustPrice": {
			Name: "AdjustPrice", ObjectName: "sales_quote", Label: "调整报价明细单价", RequiresRecord: true,
			Params: []actionParam{{Name: "line_id", Type: "text", Required: true}, {Name: "note", Type: "text"}},
		},
	}, []delegatedResource{recordResourceForTest("sales_quote", "record-a")})
	if err != nil {
		t.Fatal(err)
	}
	value.trackOutcomes, value.inputRevisionID = true, "revision-1"
	tools, _ := value.ListTools(t.Context())
	ctx := outcomeTestContext(&events, allowAll, nil)
	for _, args := range []string{
		`{"params":{"line_id":"line-1","note":"x"}}`,
		`{"params":{"note":"x","line_id":"line-1"}}`,
	} {
		if _, err := value.Dispatch(ctx, contract.ToolCall{ID: "call-" + args[:12], Name: tools[0].Name, Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	if events[0].ParamsSHA256 == "" || events[0].ParamsSHA256 != events[2].ParamsSHA256 {
		t.Fatalf("key order changed the request digest: %q vs %q", events[0].ParamsSHA256, events[2].ParamsSHA256)
	}
}
