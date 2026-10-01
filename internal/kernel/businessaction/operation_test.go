package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// This fixture exercises the runtime/ledger boundary. PostgreSQL locking and
// provenance validation have separate store tests; this fixture does not claim
// to validate that transaction implementation.
func operationLedgerGuard(events *[]ActionOutcomeEvent) ActionOutcomeGuard {
	return func(_ context.Context, event ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		for index := len(*events) - 1; index >= 0; index-- {
			previous := (*events)[index]
			if previous.OperationID != event.OperationID {
				continue
			}
			if previous.ParamsSHA256 != event.ParamsSHA256 || previous.CapabilityID != event.CapabilityID || previous.RecordID != event.RecordID {
				return ActionOutcomeReplay{}, ErrActionOperationConflict
			}
			status := previous.Status
			if previous.Phase == "started" {
				status = ActionOutcomeStatusUnknown
			}
			return ActionOutcomeReplay{Blocked: true, SameOperation: true, Status: status, Result: previous.Result}, nil
		}
		return ActionOutcomeReplay{}, nil
	}
}

func TestForgeOperationReplaysOriginalReceiptAcrossChangedModelCallID(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "explicit failure"}[failure], func(t *testing.T) {
			var events []ActionOutcomeEvent
			content := `{"ok":true,"data":{"business_receipt":"receipt-1","next_state":"review"}}`
			if failure {
				content = `{"ok":false,"error":{"code":"STALE_RECORD","message":"刷新记录后办理"}}`
			}
			host := &outcomeTestHost{result: &contract.ToolResult{Content: content}}
			dispatcher, tool := newParamDispatcher(t, host)
			ctx := outcomeTestContext(&events, operationLedgerGuard(&events), nil)
			call := contract.ToolCall{ID: "call-original", Name: tool, Args: `{"params":{"line_id":"line-1"}}`}
			first, err := dispatcher.Dispatch(ctx, call)
			if err != nil || first == nil || first.IsError != failure || len(events) != 2 {
				t.Fatalf("first=%+v events=%+v err=%v", first, events, err)
			}
			call.ID = "new-model-call-after-restart"
			second, err := dispatcher.Dispatch(ctx, call)
			if err != nil || second == nil || second.IsError != failure || host.calls != 1 || len(events) != 2 {
				t.Fatalf("original operation was redispatched: result=%+v calls=%d err=%v", second, host.calls, err)
			}
			if second.CallID != call.ID || second.ToolName != tool || second.Content != events[1].Result.Content || events[1].Result.CallID != "call-original" {
				t.Fatalf("cached receipt or current-call correlation changed: result=%+v event=%+v", second, events[1])
			}
			if events[0].OperationID == "" || events[0].OperationID != events[1].OperationID {
				t.Fatalf("durable operation not retained: %+v", events)
			}
		})
	}
}

func TestForgeOperationRejectsChangedContentButAllowsIndependentIdenticalWrite(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{}
	dispatcher, tool := newParamDispatcher(t, host)
	ctx := outcomeTestContext(&events, operationLedgerGuard(&events), nil)
	call := contract.ToolCall{ID: "call-first", Name: tool, Args: `{"params":{"line_id":"line-1"}}`}
	if _, err := dispatcher.Dispatch(ctx, call); err != nil {
		t.Fatal(err)
	}
	call.ID, call.Args = "changed-call", `{"params":{"line_id":"line-2"}}`
	if _, err := dispatcher.Dispatch(ctx, call); !errors.Is(err, ErrActionOperationConflict) || host.calls != 1 {
		t.Fatalf("changed content did not fail closed: calls=%d err=%v", host.calls, err)
	}
	call.ID, call.Args = "independent-call", `{"params":{"line_id":"line-1"}}`
	ctx = execution.WithOperationID(ctx, "member-run/segment/000000000002")
	if _, err := dispatcher.Dispatch(ctx, call); err != nil || host.calls != 2 || len(events) != 4 {
		t.Fatalf("independent identical write was blocked: calls=%d events=%+v err=%v", host.calls, events, err)
	}
	if events[0].OperationID == events[2].OperationID || events[0].ParamsSHA256 != events[2].ParamsSHA256 {
		t.Fatalf("content digest was confused with operation identity: %+v", events)
	}
}

func TestTrackedForgeOperationRequiresControlledSlot(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events, allowAll, nil)
	ctx = execution.WithOperationID(ctx, "")
	if _, err := dispatcher.Dispatch(ctx, call); err == nil || host.calls != 0 || len(events) != 0 {
		t.Fatalf("model call ID or digest substituted for durable slot: calls=%d events=%+v err=%v", host.calls, events, err)
	}
}

func TestSystemForgeIdempotencyKeyHiddenProtectedAndStable(t *testing.T) {
	for _, key := range []string{"idempotency_key", "idempotencyKey"} {
		t.Run(key, func(t *testing.T) {
			id := "forge:action:sales_quote.AdjustPrice"
			host := &outcomeTestHost{}
			metadata := actionMetadata{Name: "AdjustPrice", ObjectName: "sales_quote", RequiresRecord: true,
				Params: []actionParam{{Name: "line_id", Type: "text", Required: true}, {Name: key, Type: "text", Required: true}}}
			dispatcher, err := newDispatcherWithResources(host, []string{id}, map[string]actionMetadata{"sales_quote.AdjustPrice": metadata},
				[]delegatedResource{recordResourceForTest("sales_quote", "record-a")})
			if err != nil {
				t.Fatal(err)
			}
			dispatcher.trackOutcomes, dispatcher.inputRevisionID = true, "revision-1"
			tools, _ := dispatcher.ListTools(t.Context())
			if strings.Contains(string(tools[0].InputSchema), key) {
				t.Fatalf("system key exposed to model: %s", tools[0].InputSchema)
			}
			var events []ActionOutcomeEvent
			ctx := outcomeTestContext(&events, allowAll, nil)
			args, _ := json.Marshal(map[string]any{"params": map[string]any{"line_id": "line-1", key: "model-override"}})
			rejected, err := dispatcher.Dispatch(ctx, contract.ToolCall{ID: "override", Name: tools[0].Name, Args: string(args)})
			if err != nil || rejected == nil || !rejected.IsError || host.calls != 0 || len(events) != 0 {
				t.Fatalf("model override accepted: result=%+v calls=%d err=%v", rejected, host.calls, err)
			}
			call := contract.ToolCall{ID: "call-1", Name: tools[0].Name, Args: `{"params":{"line_id":"line-1"}}`}
			for index := 0; index < 3; index++ {
				if index == 1 {
					call.ID = "changed-model-call-id"
				}
				if index == 2 {
					ctx = execution.WithOperationID(ctx, "member-run/segment/000000000002")
				}
				if _, err := dispatcher.Dispatch(ctx, call); err != nil {
					t.Fatal(err)
				}
			}
			upstreamKey := func(index int) string {
				var request struct {
					Params map[string]string `json:"params"`
				}
				if err := json.Unmarshal([]byte(host.sent[index].Args), &request); err != nil {
					t.Fatal(err)
				}
				return request.Params[key]
			}
			if upstreamKey(0) != events[0].OperationID || upstreamKey(0) != upstreamKey(1) || upstreamKey(0) == upstreamKey(2) {
				t.Fatalf("system key is unstable or not scoped per operation: requests=%+v events=%+v", host.sent, events)
			}
			_, err = newDispatcherWithResourcesAndBindings(host, []string{id}, map[string]actionMetadata{"sales_quote.AdjustPrice": metadata}, nil,
				[]frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: key, Source: frozen.BusinessSourceMaterialName}}}})
			if err == nil {
				t.Fatal("material binding replaced the system idempotency key")
			}
		})
	}
}

func TestForgeReservationRaceReadsOriginalReceiptWithoutRedispatch(t *testing.T) {
	for _, haveReceipt := range []bool{true, false} {
		t.Run(map[bool]string{true: "recoverable", false: "missing receipt"}[haveReceipt], func(t *testing.T) {
			var events []ActionOutcomeEvent
			host := &outcomeTestHost{}
			dispatcher, call := newTrackedOutcomeDispatcher(t, host)
			guardCalls := 0
			ctx := outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
				guardCalls++
				if guardCalls == 1 {
					return ActionOutcomeReplay{}, nil
				}
				replay := ActionOutcomeReplay{Blocked: true, SameOperation: true, Status: ActionOutcomeStatusSucceeded}
				if haveReceipt {
					replay.Result = &contract.ToolResult{CallID: "original", ToolName: "run_action", Content: `{"ok":true,"data":{"receipt":"race-winner"}}`}
				}
				return replay, nil
			}, func(ActionOutcomeEvent) error { return ErrActionAlreadyRecorded })
			result, err := dispatcher.Dispatch(ctx, call)
			if err != nil || result == nil || result.IsError == haveReceipt || host.calls != 0 || guardCalls != 2 {
				t.Fatalf("race not guarded: result=%+v calls=%d guards=%d err=%v", result, host.calls, guardCalls, err)
			}
			if !haveReceipt && !result.StopLoop {
				t.Fatal("unrecoverable original receipt did not stop member")
			}
		})
	}
}

func TestContentEqualityAndKnownOutcomeWithoutCacheCannotAuthorizeReplay(t *testing.T) {
	for _, replay := range []ActionOutcomeReplay{
		{Blocked: true, Status: ActionOutcomeStatusSucceeded, SameParams: true},
		{Blocked: true, Status: ActionOutcomeStatusSucceeded, SameOperation: true},
		{Blocked: true, Status: ActionOutcomeStatusFailed, SameOperation: true},
		{Blocked: true, Status: ActionOutcomeStatusSucceeded, SameOperation: true, Result: &contract.ToolResult{IsError: true, Content: `{"ok":true}`}},
		{Blocked: true, Status: ActionOutcomeStatusFailed, SameOperation: true, Result: &contract.ToolResult{Content: `{"ok":true}`}},
		{Blocked: true, Status: ActionOutcomeStatusUnknown, SameOperation: true, Result: &contract.ToolResult{Content: `{"ok":true}`}},
	} {
		var events []ActionOutcomeEvent
		host := &outcomeTestHost{}
		dispatcher, call := newTrackedOutcomeDispatcher(t, host)
		ctx := outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) { return replay, nil }, nil)
		result, err := dispatcher.Dispatch(ctx, call)
		if err != nil || result == nil || !result.IsError || !result.StopLoop || host.calls != 0 || len(events) != 0 {
			t.Fatalf("unsupported replay did not stop: result=%+v replay=%+v err=%v", result, replay, err)
		}
	}
}

func TestActionOutcomeCacheBoundedAndExcludesCredentialsAndPrivateState(t *testing.T) {
	original := &contract.ToolResult{CallID: "original", ToolName: "run_action", Content: `{"ok":true,"data":{"receipt":"receipt-1","accessToken":"credential","nested":{"api_key":"credential","note":"Authorization=credential Bearer credential https://user:credential@example.test"}},"params":{"price":99},"reasoning":"model-private","request":{"input":"model-private"}}`, StatePatch: map[string]any{"private": "model-private"}, StopLoop: true, ParkRef: "private"}
	cached := SanitizeActionOutcomeResult(original)
	if cached == nil || !strings.Contains(cached.Content, "receipt-1") || strings.Contains(cached.Content, "credential") || strings.Contains(cached.Content, "model-private") || cached.StatePatch != nil || cached.StopLoop || cached.ParkRef != "" {
		t.Fatalf("receipt leaked private data or lost business outcome: %+v", cached)
	}
	if !strings.Contains(original.Content, "credential") {
		t.Fatal("cache sanitizer mutated the live original receipt")
	}
	for _, content := range []string{"not a structured receipt", `{"ok":true,"data":"` + strings.Repeat("x", ActionOutcomeResultMaxBytes) + `"}`, `{"ok":true} {"extra":true}`} {
		if cached := SanitizeActionOutcomeResult(&contract.ToolResult{Content: content}); cached != nil {
			t.Fatalf("unsafe or oversized receipt cached: %d bytes", len(content))
		}
	}
}

func TestOversizedKnownForgeReceiptStopsRetryWithoutDroppingKnownStatus(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{result: &contract.ToolResult{Content: `{"ok":true,"data":"` + strings.Repeat("x", ActionOutcomeResultMaxBytes) + `"}`}}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events, operationLedgerGuard(&events), nil)
	first, err := dispatcher.Dispatch(ctx, call)
	if err != nil || first.IsError || events[1].Status != ActionOutcomeStatusSucceeded || events[1].Result != nil {
		t.Fatalf("known status was lost: events=%+v err=%v", events, err)
	}
	call.ID = "retry"
	second, err := dispatcher.Dispatch(ctx, call)
	if err != nil || second == nil || !second.IsError || !second.StopLoop || host.calls != 1 {
		t.Fatalf("oversized uncached receipt was retried: result=%+v calls=%d err=%v", second, host.calls, err)
	}
}

func TestSystemIdempotencyParameterRequiresUnconstrainedText(t *testing.T) {
	for _, param := range []actionParam{
		{Name: "idempotency_key", Type: "file"},
		{Name: "idempotency_key", Type: "number"},
		{Name: "idempotency_key", Type: "text", Multiple: true},
		{Name: "idempotencyKey", Type: "text", Enum: []string{"model-selected"}},
	} {
		if _, err := actionInputSchema(actionMetadata{Params: []actionParam{param}}); err == nil {
			t.Fatalf("unsafe system idempotency metadata accepted: %+v", param)
		}
	}
}

func TestDevelopmentIdempotencyKeyUsesOnlySyntheticSystemValue(t *testing.T) {
	id := "forge:action:sales_quote.AdjustPrice"
	dispatcher, err := newDevelopmentDispatcherWithBindings([]string{id}, []DevelopmentAction{{
		CapabilityID: id, Name: "AdjustPrice", ObjectName: "sales_quote",
		Params: []actionParam{{Name: "idempotency_key", Type: "text", Required: true}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := dispatcher.ListTools(t.Context())
	if strings.Contains(string(tools[0].InputSchema), "idempotency_key") || dispatcher.trackOutcomes {
		t.Fatalf("trial exposed system key or enabled real business outcomes: tools=%+v", tools)
	}
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "simulation", Name: tools[0].Name, Args: `{}`})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Content, "weave-development-operation") || !strings.Contains(result.Content, `"simulated":true`) {
		t.Fatalf("trial did not inject isolated synthetic key: result=%+v err=%v", result, err)
	}
}
