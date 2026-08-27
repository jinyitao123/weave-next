package loomruntime

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestUsageAccumulatorDeduplicatesReplayAndSumsPhysicalAttempts(t *testing.T) {
	accumulator := NewUsageAccumulator()
	callID, err := accumulator.NextCall("run-retry", "worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, attemptID := range []string{"task-failed", "task-completed"} {
		if err := accumulator.StartAttempt(callID, attemptID); err != nil {
			t.Fatal(err)
		}
	}
	failedUsage := contract.Usage{InputTokens: 11, OutputTokens: 3, CostUSD: 0.12}
	if err := accumulator.ConfirmAttemptWithMetadata(callID, "task-failed", failedUsage, 0, UsageAttemptMetadata{
		HasTokens: true, HasCost: true, Source: "cli-reported",
	}); err != nil {
		t.Fatal(err)
	}
	// Durable completion replay for the same physical task is idempotent.
	if err := accumulator.ConfirmAttemptWithMetadata(callID, "task-failed", failedUsage, 0, UsageAttemptMetadata{
		HasTokens: true, HasCost: true, Source: "cli-reported",
	}); err != nil {
		t.Fatalf("identical replay conflicts: %v", err)
	}
	if err := accumulator.ConfirmAttemptWithMetadata(callID, "task-completed", contract.Usage{
		InputTokens: 17, OutputTokens: 5,
	}, 0, UsageAttemptMetadata{HasTokens: true, HasCost: false, Source: "cli-reported"}); err != nil {
		t.Fatal(err)
	}
	if got := accumulator.Totals(); got.InputTokens != 28 || got.OutputTokens != 8 || got.CostUSD != 0.12 {
		t.Fatalf("totals = %+v, want failed+completed physical spend", got)
	}
	coverage := accumulator.Coverage()
	if !coverage.HasTokens || coverage.HasCost || fmt.Sprint(coverage.Sources) != "[cli-reported]" {
		t.Fatalf("coverage = %+v, want token-complete/cost-incomplete CLI source", coverage)
	}
	if err := accumulator.ConfirmAttempt(callID, "task-failed", contract.Usage{InputTokens: 12}, 0); !errors.Is(err, ErrUsageConflict) {
		t.Fatalf("changed replay error = %v, want ErrUsageConflict", err)
	}

	encoded, err := accumulator.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalUsageAccumulator(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Totals(); got != accumulator.Totals() {
		t.Fatalf("restored totals = %+v, want %+v", got, accumulator.Totals())
	}
}

func TestUsageAccumulatorReadsSchemaV1WithoutInventingAttemptUsage(t *testing.T) {
	callID := usageCallID("run-v1", "step-v1", 0)
	legacy := []byte(fmt.Sprintf(`{"schema_version":1,"next_call_ordinals":{"step-v1":1},"calls":[{"usage_call_id":%q,"run_id":"run-v1","step":"step-v1","call_ordinal":0,"attempt_ids":["attempt-a","attempt-b"],"confirmed":true,"input_tokens":7,"output_tokens":2,"cost_usd":0.5}]}`, callID))
	restored, err := UnmarshalUsageAccumulator(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Totals(); got.InputTokens != 7 || got.OutputTokens != 2 || got.CostUSD != 0.5 {
		t.Fatalf("legacy totals = %+v", got)
	}
	coverage := restored.Coverage()
	if coverage.HasTokens || coverage.HasCost {
		t.Fatalf("legacy multi-attempt coverage = %+v, want unknown attempt incomplete", coverage)
	}
}

func TestUsageAccumulatorToolCallsCheckpointCompatibility(t *testing.T) {
	accumulator := NewUsageAccumulator()
	callID, err := accumulator.NextCall("run-1", "step-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := accumulator.StartAttempt(callID, "attempt-1"); err != nil {
		t.Fatal(err)
	}
	if err := accumulator.ConfirmAttempt(callID, "attempt-1", contract.Usage{
		InputTokens: 3, OutputTokens: 5, CostUSD: 0.25,
	}, 2); err != nil {
		t.Fatal(err)
	}
	encoded, err := accumulator.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"tool_calls":2`)) {
		t.Fatalf("checkpoint omits measured tool calls: %s", encoded)
	}
	restored, err := UnmarshalUsageAccumulator(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Totals(); got.ToolCalls != 2 {
		t.Fatalf("restored tool calls = %d, want 2", got.ToolCalls)
	}

	legacy := bytes.Replace(encoded, []byte(`,"tool_calls":2`), nil, 1)
	restoredLegacy, err := UnmarshalUsageAccumulator(legacy)
	if err != nil {
		t.Fatalf("legacy checkpoint without tool_calls is unreadable: %v", err)
	}
	if got := restoredLegacy.Totals().ToolCalls; got != 0 {
		t.Fatalf("legacy tool calls = %d, want unknown historical zero", got)
	}
}
