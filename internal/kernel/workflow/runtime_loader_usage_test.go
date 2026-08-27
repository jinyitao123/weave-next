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
			{AttemptID: "task-failed", Status: "failed", Usage: receipt},
			{AttemptID: "task-timeout", Status: "timeout"},
		}},
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
}
