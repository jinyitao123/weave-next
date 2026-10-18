package workflow

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestLoomUpgradeStreamRetainsCompletedReasoningBlock(t *testing.T) {
	block := contract.Reasoning{Text: "synthetic reasoning", Signature: "synthetic-signature"}
	fixture := &hostStreamFixture{chunks: []contract.StreamChunk{
		{Reasoning: block.Text},
		{Content: "synthetic answer"},
		{Done: true, FinishReason: "stop", ReasoningBlock: &block},
	}}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Reasoning) != 1 || response.Reasoning[0] != block {
		t.Fatalf("completed reasoning block lost by Weave stream assembly: got=%+v want=%+v", response.Reasoning, block)
	}
}

func TestLoomUpgradeStreamPreservesCacheSubtotalsWithoutAddingToInput(t *testing.T) {
	want := contract.Usage{InputTokens: 100, OutputTokens: 7, CacheReadTokens: 80, CacheWriteTokens: 10}
	fixture := &hostStreamFixture{chunks: []contract.StreamChunk{
		{Content: "synthetic answer"},
		{Done: true, FinishReason: "stop", Usage: &want},
	}}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
	if err != nil || response == nil || response.Usage != want {
		t.Fatalf("stream usage changed: response=%+v err=%v want=%+v", response, err, want)
	}
}
