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

func TestUsageAccumulatorRejectsRetiredCheckpointFormats(t *testing.T) {
	for _, version := range []int{1, 2} {
		data := []byte(fmt.Sprintf(`{"schema_version":%d,"next_call_ordinals":{},"calls":[]}`, version))
		if _, err := UnmarshalUsageAccumulator(data); err == nil {
			t.Fatalf("retired schema %d was accepted", version)
		}
	}
	encoded, err := NewUsageAccumulator().MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"schema_version":3`)) {
		t.Fatalf("empty current accumulator changed format: %s", encoded)
	}
	if _, err := UnmarshalUsageAccumulator(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestUsageAccumulatorToolCallsCheckpointRoundTrip(t *testing.T) {
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

	withoutToolCalls := bytes.Replace(encoded, []byte(`,"tool_calls":2`), nil, 1)
	restoredWithoutToolCalls, err := UnmarshalUsageAccumulator(withoutToolCalls)
	if err != nil {
		t.Fatalf("current checkpoint with omitted optional tool_calls is unreadable: %v", err)
	}
	if got := restoredWithoutToolCalls.Totals().ToolCalls; got != 0 {
		t.Fatalf("omitted tool calls = %d, want zero", got)
	}
}
