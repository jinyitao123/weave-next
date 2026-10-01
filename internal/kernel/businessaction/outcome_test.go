package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

type outcomeTestHost struct {
	calls      int
	sent       []contract.ToolCall
	result     *contract.ToolResult
	err        error
	onDispatch func()
}

func (*outcomeTestHost) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (host *outcomeTestHost) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	host.calls++
	host.sent = append(host.sent, call)
	if host.onDispatch != nil {
		host.onDispatch()
	}
	if host.result != nil {
		copy := *host.result
		copy.CallID, copy.ToolName = call.ID, call.Name
		return &copy, host.err
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: `{"ok":true}`}, host.err
}

func newTrackedOutcomeDispatcher(t *testing.T, host contract.ToolDispatcher) (*dispatcher, contract.ToolCall) {
	t.Helper()
	id := "forge:action:sales_contract.ContractSubmit"
	value, err := newDispatcherWithResources(host, []string{id}, map[string]actionMetadata{
		"sales_contract.ContractSubmit": {
			Name: "ContractSubmit", ObjectName: "sales_contract", Label: "提交指定合同版本", RequiresRecord: true,
		},
	}, []delegatedResource{recordResourceForTest("sales_contract", "record-a")})
	if err != nil {
		t.Fatal(err)
	}
	value.trackOutcomes = true
	value.inputRevisionID = "revision-1"
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	return value, contract.ToolCall{ID: "forge-call-1", Name: tools[0].Name, Args: `{}`}
}

func outcomeTestContext(
	events *[]ActionOutcomeEvent,
	guard ActionOutcomeGuard,
	recordError func(ActionOutcomeEvent) error,
) context.Context {
	ctx := execution.WithInvocationID(context.Background(), "snapshot/0/lead")
	ctx = execution.WithOperationID(ctx, "member-run/segment/000000000001")
	ctx = WithActionOutcomeGuard(ctx, guard)
	return WithActionOutcomeRecorder(ctx, func(_ context.Context, event ActionOutcomeEvent) error {
		if recordError != nil {
			if err := recordError(event); err != nil {
				return err
			}
		}
		*events = append(*events, event)
		return nil
	})
}

func TestForgeActionPersistsScopedReceiptBeforeCallAndParsesNativeEnvelope(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{result: &contract.ToolResult{Content: `{"ok":true,"data":{"id":"external-result"}}`}}
	guard := func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		return ActionOutcomeReplay{}, nil
	}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	host.onDispatch = func() {
		if len(events) != 1 || events[0].Phase != "started" {
			t.Errorf("Forge dispatch began before its durable start receipt: %+v", events)
		}
	}
	ctx := outcomeTestContext(&events, guard, nil)
	result, err := dispatcher.Dispatch(ctx, call)
	if err != nil || result == nil || result.IsError || host.calls != 1 || len(events) != 2 {
		t.Fatalf("result=%+v calls=%d events=%+v err=%v", result, host.calls, events, err)
	}
	if events[0].Phase != "started" || events[1].Phase != "result" || events[1].Status != ActionOutcomeStatusSucceeded ||
		events[1].ActionLabel != "提交指定合同版本" || events[1].InputRevisionID != "revision-1" ||
		events[1].RecordID != "record-a" || events[1].FrozenRecordSHA256 == "" || events[1].OperationSlot != execution.OperationID(ctx) {
		t.Fatalf("receipt lost frozen action provenance: %+v", events)
	}
	encoded, _ := json.Marshal(events)
	if !strings.Contains(string(encoded), "external-result") || events[1].Result == nil || events[0].Result != nil {
		t.Fatalf("native business receipt was not retained exclusively on result: %s", encoded)
	}
}

func TestForgeActionResultRequiresNativeSuccessMarker(t *testing.T) {
	for _, test := range []struct {
		name   string
		result *contract.ToolResult
		want   string
	}{
		{name: "explicit success", result: &contract.ToolResult{Content: `{"ok":true}`}, want: ActionOutcomeStatusSucceeded},
		{name: "explicit rejection", result: &contract.ToolResult{Content: `{"ok":false,"error":"rejected"}`}, want: ActionOutcomeStatusFailed},
		{name: "explicit error", result: &contract.ToolResult{Content: `{"error":"rejected"}`}, want: ActionOutcomeStatusFailed},
		{name: "null marker with mcp error", result: &contract.ToolResult{Content: `{"ok":null}`, IsError: true}, want: ActionOutcomeStatusUnknown},
		{name: "wrong type marker", result: &contract.ToolResult{Content: `{"ok":"false"}`}, want: ActionOutcomeStatusUnknown},
		{name: "null marker", result: &contract.ToolResult{Content: `{"ok":null}`}, want: ActionOutcomeStatusUnknown},
		{name: "missing marker", result: &contract.ToolResult{Content: `{"message":"done"}`}, want: ActionOutcomeStatusUnknown},
		{name: "contradictory envelope", result: &contract.ToolResult{Content: `{"ok":true,"error":"uncertain"}`}, want: ActionOutcomeStatusUnknown},
		{name: "contradictory mcp error", result: &contract.ToolResult{Content: `{"ok":true}`, IsError: true}, want: ActionOutcomeStatusUnknown},
		{name: "mcp error", result: &contract.ToolResult{Content: "rejected", IsError: true}, want: ActionOutcomeStatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []ActionOutcomeEvent
			dispatcher, call := newTrackedOutcomeDispatcher(t, &outcomeTestHost{result: test.result})
			ctx := outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
				return ActionOutcomeReplay{}, nil
			}, nil)
			_, err := dispatcher.Dispatch(ctx, call)
			if err != nil || len(events) != 2 || events[1].Status != test.want {
				t.Fatalf("events=%+v err=%v", events, err)
			}
		})
	}
}

func TestForgeActionNativeErrorRequiresExplicitFailureMeaning(t *testing.T) {
	for _, test := range []struct {
		name, content, want string
		isError             bool
	}{
		{"boolean false", `{"error":false}`, ActionOutcomeStatusUnknown, false},
		{"boolean true", `{"error":true}`, ActionOutcomeStatusUnknown, false},
		{"numeric zero", `{"error":0}`, ActionOutcomeStatusUnknown, false},
		{"numeric nonzero", `{"error":500}`, ActionOutcomeStatusUnknown, false},
		{"empty array", `{"error":[]}`, ActionOutcomeStatusUnknown, false},
		{"array of messages", `{"error":["rejected"]}`, ActionOutcomeStatusUnknown, false},
		{"empty object", `{"error":{}}`, ActionOutcomeStatusUnknown, false},
		{"opaque object", `{"error":{"detail":"opaque value"}}`, ActionOutcomeStatusUnknown, false},
		{"empty semantics", `{"error":{"message":" ","code":""}}`, ActionOutcomeStatusUnknown, false},
		{"wrong semantic types", `{"error":{"message":false,"code":0}}`, ActionOutcomeStatusUnknown, false},
		{"null error", `{"error":null}`, ActionOutcomeStatusUnknown, false},
		{"empty string", `{"error":" "}`, ActionOutcomeStatusUnknown, false},
		{"text rejection", `{"error":"rejected"}`, ActionOutcomeStatusFailed, false},
		{"structured message", `{"error":{"message":"record rejected"}}`, ActionOutcomeStatusFailed, false},
		{"structured code", `{"error":{"code":"STALE_RECORD"}}`, ActionOutcomeStatusFailed, false},
		{"success with false error", `{"ok":true,"error":false}`, ActionOutcomeStatusUnknown, false},
		{"success with numeric error", `{"ok":true,"error":0}`, ActionOutcomeStatusUnknown, false},
		{"success with array error", `{"ok":true,"error":[]}`, ActionOutcomeStatusUnknown, false},
		{"success with opaque error", `{"ok":true,"error":{}}`, ActionOutcomeStatusUnknown, false},
		{"success with null error", `{"ok":true,"error":null}`, ActionOutcomeStatusSucceeded, false},
		{"success with empty error text", `{"ok":true,"error":" "}`, ActionOutcomeStatusSucceeded, false},
		{"explicit rejection marker", `{"ok":false,"error":0}`, ActionOutcomeStatusFailed, false},
		{"invalid ok with valid error", `{"ok":null,"error":"rejected"}`, ActionOutcomeStatusUnknown, false},
		{"trusted MCP flag with false error", `{"error":false}`, ActionOutcomeStatusFailed, true},
		{"trusted MCP flag with opaque object", `{"error":{}}`, ActionOutcomeStatusFailed, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []ActionOutcomeEvent
			host := &outcomeTestHost{result: &contract.ToolResult{Content: test.content, IsError: test.isError}}
			dispatcher, call := newTrackedOutcomeDispatcher(t, host)
			result, err := dispatcher.Dispatch(outcomeTestContext(&events, allowAll, nil), call)
			if err != nil || result == nil || len(events) != 2 || events[1].Status != test.want {
				t.Fatalf("native marker incorrectly classified: result=%+v events=%+v err=%v", result, events, err)
			}
			if test.want == ActionOutcomeStatusUnknown && (!result.StopLoop || !result.IsError || events[1].Result != nil) {
				t.Fatalf("unknown receipt became resumable or cached: result=%+v event=%+v", result, events[1])
			}
		})
	}
}

// The runtime must retain unknown target protection even when the next caller
// represents a distinct durable operation. The guard fixture implements the
// existing activity-store contract over emitted facts; the PostgreSQL guard
// implementation has independent integration coverage and is not replaced.
func TestAmbiguousNativeErrorsKeepTargetBlockedAcrossContinuousOperations(t *testing.T) {
	for _, content := range []string{
		`{"error":false}`, `{"error":0}`, `{"error":[]}`, `{"error":{}}`,
		`{"error":{"message":false,"code":0}}`, `{"ok":true,"error":false}`,
		`{"error":"explicit rejection"}`,
	} {
		t.Run(content, func(t *testing.T) {
			var events []ActionOutcomeEvent
			host := &outcomeTestHost{result: &contract.ToolResult{Content: content}}
			dispatcher, call := newTrackedOutcomeDispatcher(t, host)
			guard := func(_ context.Context, incoming ActionOutcomeEvent) (ActionOutcomeReplay, error) {
				for _, previous := range events {
					if previous.Phase == "result" && previous.Status == ActionOutcomeStatusUnknown &&
						previous.InputRevisionID == incoming.InputRevisionID && previous.CapabilityID == incoming.CapabilityID && previous.RecordID == incoming.RecordID {
						return ActionOutcomeReplay{Blocked: true, Status: previous.Status, SameOperation: previous.OperationID == incoming.OperationID}, nil
					}
				}
				return ActionOutcomeReplay{}, nil
			}
			ctx := outcomeTestContext(&events, guard, nil)
			first, err := dispatcher.Dispatch(ctx, call)
			if err != nil || first == nil || host.calls != 1 || len(events) != 2 {
				t.Fatalf("first result=%+v calls=%d events=%+v err=%v", first, host.calls, events, err)
			}
			call.ID = "next-model-call"
			ctx = execution.WithOperationID(ctx, "member-run/segment/000000000002")
			next, err := dispatcher.Dispatch(ctx, call)
			if content == `{"error":"explicit rejection"}` {
				if err != nil || next == nil || host.calls != 2 || len(events) != 4 || events[1].Status != ActionOutcomeStatusFailed {
					t.Fatalf("confirmed failure wrongly blocked a new explicit operation: calls=%d events=%+v err=%v", host.calls, events, err)
				}
				return
			}
			if err != nil || next == nil || !next.IsError || !next.StopLoop || host.calls != 1 || len(events) != 2 || events[1].Status != ActionOutcomeStatusUnknown {
				t.Fatalf("ambiguous error released target protection: next=%+v calls=%d events=%+v err=%v", next, host.calls, events, err)
			}
		})
	}
}

func TestForgeActionTreatsTypedMCPFailureAsFailed(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{err: fmt.Errorf("%w: action rejected", mcphost.ErrDispatchExplicitFailure)}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events,
		func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
			return ActionOutcomeReplay{}, nil
		}, nil)
	result, err := dispatcher.Dispatch(ctx, call)
	if err != nil || result == nil || !result.IsError || host.calls != 1 || len(events) != 2 || events[1].Status != ActionOutcomeStatusFailed {
		t.Fatalf("explicit MCP failure was misclassified: result=%+v calls=%d events=%+v err=%v", result, host.calls, events, err)
	}
}

func TestUnknownForgeActionCannotBeRetriedWithANewCallID(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{err: fmt.Errorf("%w: connection closed", mcphost.ErrDispatchOutcomeUnknown)}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	guard := func(_ context.Context, event ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		for index := range events {
			previous := events[index]
			if previous.Phase == "started" && previous.OperationID == event.OperationID {
				status := ActionOutcomeStatusUnknown
				if index+1 < len(events) && events[index+1].CallID == previous.CallID && events[index+1].Phase == "result" {
					status = events[index+1].Status
				}
				if status == ActionOutcomeStatusUnknown {
					return ActionOutcomeReplay{Blocked: true, SameOperation: true, Status: status}, nil
				}
			}
		}
		return ActionOutcomeReplay{}, nil
	}
	ctx := outcomeTestContext(&events, guard, nil)
	first, err := dispatcher.Dispatch(ctx, call)
	if err != nil || first == nil || !first.IsError || len(events) != 2 || events[1].Status != ActionOutcomeStatusUnknown {
		t.Fatalf("first result=%+v events=%+v err=%v", first, events, err)
	}
	call.ID = "forge-call-retry"
	second, err := dispatcher.Dispatch(ctx, call)
	if err != nil || second == nil || !second.IsError || !strings.Contains(second.Content, "核对") || !second.StopLoop || host.calls != 1 || len(events) != 2 {
		t.Fatalf("unknown write was retried: second=%+v calls=%d events=%+v err=%v", second, host.calls, events, err)
	}
}

func TestForgeActionPersistenceFailureStopsBeforeOrAfterCall(t *testing.T) {
	startErr := errors.New("activity store unavailable")
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events,
		func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
			return ActionOutcomeReplay{}, nil
		},
		func(event ActionOutcomeEvent) error {
			if event.Phase == "started" {
				return startErr
			}
			return nil
		})
	if _, err := dispatcher.Dispatch(ctx, call); !errors.Is(err, startErr) || host.calls != 0 {
		t.Fatalf("start persistence failure reached Forge: calls=%d err=%v", host.calls, err)
	}

	var started []ActionOutcomeEvent
	host = &outcomeTestHost{}
	dispatcher, call = newTrackedOutcomeDispatcher(t, host)
	ctx = outcomeTestContext(&started,
		func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
			return ActionOutcomeReplay{}, nil
		},
		func(event ActionOutcomeEvent) error {
			if event.Phase == "result" {
				return startErr
			}
			return nil
		})
	if _, err := dispatcher.Dispatch(ctx, call); !errors.Is(err, startErr) || host.calls != 1 || len(started) != 1 || started[0].Phase != "started" {
		t.Fatalf("result persistence failure was hidden: calls=%d receipts=%+v err=%v", host.calls, started, err)
	}
}

func TestUnresolvedStartReservationStopsConcurrentForgeDispatch(t *testing.T) {
	var events []ActionOutcomeEvent
	host := &outcomeTestHost{}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events,
		func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
			return ActionOutcomeReplay{}, nil
		},
		func(event ActionOutcomeEvent) error {
			if event.Phase == "started" {
				return ErrActionOutcomeUnresolved
			}
			return nil
		})
	result, err := dispatcher.Dispatch(ctx, call)
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content, "未确认") || host.calls != 0 || len(events) != 0 {
		t.Fatalf("unresolved reservation reached Forge: result=%+v calls=%d events=%+v err=%v", result, host.calls, events, err)
	}
}

func TestExplicitNativeErrorsReplayOriginalFailureReceipts(t *testing.T) {
	for _, content := range []string{
		`{"error":"record rejected"}`,
		`{"error":{"code":"STALE_RECORD"}}`,
		`{"error":{"message":"refresh the business record"}}`,
	} {
		t.Run(content, func(t *testing.T) {
			var events []ActionOutcomeEvent
			host := &outcomeTestHost{result: &contract.ToolResult{Content: content}}
			dispatcher, call := newTrackedOutcomeDispatcher(t, host)
			ctx := outcomeTestContext(&events, operationLedgerGuard(&events), nil)
			first, err := dispatcher.Dispatch(ctx, call)
			if err != nil || first == nil || !first.IsError || len(events) != 2 || events[1].Status != ActionOutcomeStatusFailed || events[1].Result == nil {
				t.Fatalf("explicit native error lost its trusted receipt: result=%+v events=%+v err=%v", first, events, err)
			}
			call.ID = "changed-call-on-recovery"
			replayed, err := dispatcher.Dispatch(ctx, call)
			if err != nil || replayed == nil || !replayed.IsError || replayed.Content != events[1].Result.Content || replayed.CallID != call.ID || host.calls != 1 || len(events) != 2 {
				t.Fatalf("explicit native error was redispatched instead of replayed: result=%+v calls=%d events=%+v err=%v", replayed, host.calls, events, err)
			}
		})
	}
}
