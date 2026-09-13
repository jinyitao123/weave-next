package runtimellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type receiptExecutor struct {
	result engine.RunResult
	err    error
}

func (e receiptExecutor) ExecRemote(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execspec.Attachment,
) (engine.RunResult, error) {
	return e.result, e.err
}

func (e receiptExecutor) ExecRemoteStructured(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execspec.Attachment, json.RawMessage,
) (engine.RunResult, error) {
	return e.result, e.err
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

func TestAdapterFailedPhysicalAttemptsAreConfirmedBeforeError(t *testing.T) {
	receipt := func(input, output int, cost float64) *engine.UsageReceipt {
		return &engine.UsageReceipt{
			InputTokens: input, OutputTokens: output, CostUSD: cost,
			HasTokens: true, HasCost: true, Source: engine.UsageSourceCLIReported,
			Scope: engine.UsageScopeInvocation, EngineVersion: "fixture-cli 1.0",
		}
	}
	executor := receiptExecutor{
		result: engine.RunResult{Attempts: []engine.UsageAttempt{
			{AttemptID: "task-failed-1", Status: "failed", Usage: receipt(9, 2, 0.08)},
			{AttemptID: "task-timeout-2", Status: "timeout", Usage: receipt(13, 4, 0.11)},
		}},
		err: errors.New("runtime exhausted attempts"),
	}
	record := &registry.AgentRecord{Name: "runtime-node", ID: "agent-failed", WorkspaceID: "workspace-1", Version: 1}
	adapter, err := New(executor, record.WorkspaceID, record, execution.AgentExecutionStamp{
		AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeLegacyOrchestrator,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := loom.State{"__run_id": "run-failed-usage"}
	if err := loomruntime.StoreUsageAccumulator(state, loomruntime.NewUsageAccumulator()); err != nil {
		t.Fatal(err)
	}
	ctx := loomruntime.WithUsageRunScope(t.Context())
	if err := loomruntime.BindUsageBeforeStep(ctx, "judge", state); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	boundary := loomruntime.NewUsageBoundaryLLM(adapter, func() (string, error) {
		sequence++
		return fmt.Sprintf("ua1_%032x", sequence), nil
	})
	if _, err := boundary.Chat(ctx, contract.ChatRequest{}); err == nil {
		t.Fatal("failed CLI inference returned nil error")
	}
	accumulator, err := loomruntime.LoadUsageAccumulator(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := accumulator.Totals(); got.InputTokens != 22 || got.OutputTokens != 6 || got.CostUSD != 0.19 {
		t.Fatalf("failed attempt totals = %+v", got)
	}
	coverage := accumulator.Coverage()
	if !coverage.HasTokens || !coverage.HasCost || len(coverage.Sources) != 1 || coverage.Sources[0] != engine.UsageSourceCLIReported {
		t.Fatalf("failed attempt coverage = %+v", coverage)
	}
}
