package runtimellm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type receiptExecutor struct {
	result engine.RunResult
}

func (e receiptExecutor) ExecRemote(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execenv.Attachment,
) (engine.RunResult, error) {
	return e.result, nil
}

func (e receiptExecutor) ExecRemoteStructured(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execenv.Attachment, json.RawMessage,
) (engine.RunResult, error) {
	return e.result, nil
}

func TestAdapterMapsReportedAttemptsForJudgeAndPlannerChannels(t *testing.T) {
	receipt := func(input, output int, cost float64, hasCost bool) *engine.UsageReceipt {
		return &engine.UsageReceipt{
			InputTokens: input, OutputTokens: output, CostUSD: cost,
			HasTokens: true, HasCost: hasCost, Source: engine.UsageSourceCLIReported,
			Scope: engine.UsageScopeInvocation, EngineVersion: "fixture-cli 1.0",
		}
	}
	executor := receiptExecutor{result: engine.RunResult{
		Output: `{"content":"accepted","tool_calls":[]}`,
		Attempts: []engine.UsageAttempt{
			{AttemptID: "task-1", Status: "failed", Usage: receipt(10, 2, 0.1, true)},
			{AttemptID: "task-2", Status: "completed", Usage: receipt(20, 3, 0, false)},
		},
	}}
	record := &registry.AgentRecord{Name: "runtime-node", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1}
	adapter, err := New(executor, record.WorkspaceID, record, execution.AgentExecutionStamp{
		AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeLegacyOrchestrator,
	})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	response, err := adapter.Chat(t.Context(), contract.ChatRequest{Schema: &schema})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "accepted" || response.Usage.InputTokens != 30 ||
		response.Usage.OutputTokens != 5 || response.Usage.CostUSD != 0.1 {
		t.Fatalf("response=%+v", response)
	}
}

func TestAdapterDoesNotEstimateMissingReceipt(t *testing.T) {
	usage, err := contractUsage(engine.RunResult{})
	if err != nil || usage != (contract.Usage{}) {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}
