package teamrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type failedBusinessToolCLI struct {
	result engine.RunResult
	err    error
}

func (cli failedBusinessToolCLI) ExecRemote(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, []execspec.Attachment) (engine.RunResult, error) {
	return cli.result, cli.err
}

func runSerialWorkerFailure(t *testing.T, entry workflow.RuntimeGraphEntry) serialMachineResult {
	t.Helper()
	artifact := &workflow.RuntimeArtifact{Entries: []workflow.RuntimeGraphEntry{entry}}
	worker := machine.Node{ID: "inspect", Type: machine.NodeWorker, Config: machine.WorkerConfig{
		AgentID: "worker", AgentVersion: 1, ResultRequirement: "Inspect the requested object.",
	}}
	graph := machine.GraphDefinition{EntryNodeID: worker.ID, Nodes: []machine.Node{
		worker,
		{ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: worker.ID}}},
	}, Edges: []machine.Edge{{ID: "worker-success", FromNodeID: worker.ID, ToNodeID: "deliver", Route: machine.RouteSuccess}}}
	result := runSerialMachine(context.Background(), graph, frozen.ArtifactPayloadV1{}, artifact, map[string]any{}, serialMachineStart{
		Run: TeamRun{WorkspaceID: "ws", ProjectID: "project", RunID: "run-1", RunSnapshotID: "snapshot-1", TeamID: "team", WorkflowID: "flow", WorkflowVersion: 1},
	})
	return result
}

func assertToolFailureAndUsageIncomplete(t *testing.T, result serialMachineResult, primary error, usageReason string) {
	t.Helper()
	if result.Status != serialFailed {
		t.Fatalf("status=%s err=%v, want failed run", result.Status, result.Err)
	}
	if primary != nil && !errors.Is(result.Err, primary) {
		t.Fatalf("final failure lost the primary tool error: %v", result.Err)
	}
	if !strings.Contains(result.Err.Error(), usageReason) {
		t.Fatalf("final failure lost the secondary usage error: %v", result.Err)
	}
	if result.UsageComplete || result.UsageIncompleteReason != UsageIncompleteReasonAttemptLost {
		t.Fatalf("invalid usage receipt was recorded as complete: %+v", result)
	}
	if totals := result.Usage.Totals(); totals.InputTokens != 0 || totals.OutputTokens != 0 || totals.CostUSD != 0 || totals.ToolCalls != 0 {
		t.Fatalf("invalid receipt was charged: %+v", totals)
	}
	if coverage := result.Usage.Coverage(); coverage.HasTokens || coverage.HasCost {
		t.Fatalf("invalid receipt reported complete usage coverage: %+v", coverage)
	}
	if summary := SanitizeCauseSummary(result.Err); summary == nil || !strings.Contains(*summary, usageReason) || (primary != nil && !strings.Contains(*summary, primary.Error())) {
		t.Fatalf("persisted cause summary does not preserve the independent diagnostics: %v", summary)
	}
}

func TestSerialMachineKeepsBusinessToolFailureWhenUsageReceiptIsInvalid(t *testing.T) {
	toolFailure := errors.New("business tool inventory_lookup failed: object was unavailable")
	cli, err := workflow.NewRuntimeCLIEntry(failedBusinessToolCLI{
		result: engine.RunResult{
			Status: "failed", Err: toolFailure.Error(),
			Attempts: []engine.UsageAttempt{{AttemptID: "attempt-1", Status: "failed", Usage: &engine.UsageReceipt{
				InputTokens: -1, HasTokens: true, HasCost: true, Source: engine.UsageSourceCLIReported,
			}}},
		},
		err: toolFailure,
	}, &registry.AgentRecord{WorkspaceID: "ws", ID: "worker", Name: "Inventory checker", Version: 1}, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	result := runSerialWorkerFailure(t, workflow.RuntimeGraphEntry{AgentID: "worker", AgentVersion: 1, CLI: cli})
	assertToolFailureAndUsageIncomplete(t, result, toolFailure, "usage input_tokens must be non-negative")
}

func TestSerialMachineKeepsUsageErrorWhenThereIsNoToolFailure(t *testing.T) {
	cli, err := workflow.NewRuntimeCLIEntry(failedBusinessToolCLI{
		result: engine.RunResult{Status: "failed", Attempts: []engine.UsageAttempt{{AttemptID: "attempt-1", Usage: &engine.UsageReceipt{InputTokens: -1, HasTokens: true, HasCost: true}}}},
	}, &registry.AgentRecord{WorkspaceID: "ws", ID: "worker", Name: "Inventory checker", Version: 1}, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	result := runSerialWorkerFailure(t, workflow.RuntimeGraphEntry{AgentID: "worker", AgentVersion: 1, CLI: cli})
	assertToolFailureAndUsageIncomplete(t, result, nil, "usage input_tokens must be non-negative")
}

func TestSerialMachineKeepsLoomBusinessToolFailureWhenUsageStateIsInvalid(t *testing.T) {
	toolFailure := errors.New("business action inventory_lookup failed: object was unavailable")
	graph := loom.NewGraph("inventory-worker", "business_action")
	graph.AddStep("business_action", func(_ context.Context, state loom.State) (loom.State, error) {
		state["__usage_accumulator"] = "invalid usage state"
		return nil, toolFailure
	}, nil)
	result := runSerialWorkerFailure(t, workflow.RuntimeGraphEntry{AgentID: "worker", AgentVersion: 1, Graph: graph})
	assertToolFailureAndUsageIncomplete(t, result, toolFailure, "decode usage accumulator checkpoint")
}

func TestSerialMachinePreservesConfirmedMemberReceiptsWhenCoverageIsIncomplete(t *testing.T) {
	graph := loom.NewGraph("inventory-worker", "record_usage")
	graph.AddStep("record_usage", func(_ context.Context, state loom.State) (loom.State, error) {
		accumulator := loomruntime.NewUsageAccumulator()
		callID, err := accumulator.NextCall("member-run", "model")
		if err != nil {
			return nil, err
		}
		if err := accumulator.StartAttempt(callID, "confirmed-attempt"); err != nil {
			return nil, err
		}
		if err := accumulator.ConfirmAttemptWithMetadata(callID, "confirmed-attempt", contract.Usage{
			InputTokens: 17, OutputTokens: 8, CostUSD: 0.01,
		}, 1, loomruntime.UsageAttemptMetadata{HasTokens: true, HasCost: true, Source: "test"}); err != nil {
			return nil, err
		}
		if err := accumulator.StartAttempt(callID, "unreported-attempt"); err != nil {
			return nil, err
		}
		if err := loomruntime.StoreUsageAccumulator(state, accumulator); err != nil {
			return nil, err
		}
		state["output"] = "inventory checked"
		return state, nil
	}, nil)

	result := runSerialWorkerFailure(t, workflow.RuntimeGraphEntry{AgentID: "worker", AgentVersion: 1, Graph: graph})
	if result.Status != serialCompleted {
		t.Fatalf("status=%s err=%v, want business run to complete with partial usage", result.Status, result.Err)
	}
	if result.UsageComplete || result.UsageIncompleteReason != UsageIncompleteReasonAttemptLost {
		t.Fatalf("missing member coverage was reported complete: usage_complete=%v reason=%q", result.UsageComplete, result.UsageIncompleteReason)
	}
	if totals := result.Usage.Totals(); totals.InputTokens != 17 || totals.OutputTokens != 8 || totals.CostUSD != 0.01 || totals.ToolCalls != 1 {
		t.Fatalf("confirmed member receipt was not preserved: %+v", totals)
	}
}
