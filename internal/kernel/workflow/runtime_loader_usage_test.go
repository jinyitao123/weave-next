package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type runtimeCLIReceiptExecutor struct {
	result engine.RunResult
	err    error
}

func (e runtimeCLIReceiptExecutor) ExecRemote(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execenv.Attachment,
) (engine.RunResult, error) {
	return e.result, e.err
}

func TestRuntimeCLIEntryPreservesFailedAttemptsAndMissingReceipt(t *testing.T) {
	receipt := &engine.UsageReceipt{
		InputTokens: 31, OutputTokens: 7, CostUSD: 0.4,
		HasTokens: true, HasCost: true, Source: engine.UsageSourceCLIReported,
		Scope: engine.UsageScopeInvocation, EngineVersion: "fixture-cli 1.0",
	}
	record := &registry.AgentRecord{WorkspaceID: "workspace-1", ID: "agent-1", Version: 1}
	entry, err := NewRuntimeCLIEntry(runtimeCLIReceiptExecutor{
		result: engine.RunResult{Attempts: []engine.UsageAttempt{
			{AttemptID: "task-failed", Status: "failed", Usage: receipt, Events: []engine.Event{{Kind: "tool_call", Tool: "shell", CallID: "call-1"}}},
			{AttemptID: "task-timeout", Status: "timeout"},
		}, Events: []engine.Event{{Kind: "tool_call", Tool: "shell", CallID: "call-1"}}},
		err: errors.New("CLI attempts exhausted"),
	}, record, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := entry.ExecuteAccounted(t.Context(), "prompt")
	if execErr == nil {
		t.Fatal("failed CLI execution returned nil error")
	}
	if len(result.Attempts) != 2 || result.Attempts[0].InputTokens != 31 ||
		!result.Attempts[0].HasTokens || !result.Attempts[0].HasCost ||
		result.Attempts[0].Source != engine.UsageSourceCLIReported {
		t.Fatalf("failed receipt attempts = %#v", result.Attempts)
	}
	if result.Attempts[1].HasTokens || result.Attempts[1].HasCost ||
		result.Attempts[1].InputTokens != 0 || result.Attempts[1].CostUSD != 0 {
		t.Fatalf("missing receipt was not preserved as unknown dimensions: %#v", result.Attempts[1])
	}
	if len(result.Attempts[0].Events) != 1 || len(result.Events) != 1 {
		t.Fatalf("observed events were not preserved: %#v", result)
	}
}

func TestRuntimeCLIResultObservedEventsPrefersPhysicalAttempts(t *testing.T) {
	result := RuntimeCLIResult{
		Events: []RuntimeCLIEvent{{Kind: "tool_call", CallID: "aggregate"}},
		Attempts: []RuntimeCLIUsageAttempt{
			{AttemptID: "task-1", Events: []RuntimeCLIEvent{{Kind: "tool_call", CallID: "call-1"}}},
			{AttemptID: "task-2", Events: []RuntimeCLIEvent{{Kind: "tool_result", CallID: "call-2"}}},
		},
	}
	events := result.ObservedEvents(2)
	if len(events) != 2 || events[0].CallID != "task-1:call-1" || events[1].CallID != "task-2:call-2" {
		t.Fatalf("observed events = %#v", events)
	}
	if got := result.ObservedEvents(1); len(got) != 1 || got[0].CallID != "task-1:call-1" {
		t.Fatalf("limited events = %#v", got)
	}
	if got := (RuntimeCLIResult{Events: result.Events}).ObservedEvents(2); len(got) != 1 || got[0].CallID != "aggregate" {
		t.Fatalf("aggregate fallback = %#v", got)
	}
	if got := (RuntimeCLIResult{Attempts: []RuntimeCLIUsageAttempt{{AttemptID: "task-empty"}}, Events: result.Events}).ObservedEvents(2); len(got) != 1 || got[0].CallID != "aggregate" {
		t.Fatalf("empty-attempt fallback = %#v", got)
	}
}
