package loomruntime

import (
	"bytes"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

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
