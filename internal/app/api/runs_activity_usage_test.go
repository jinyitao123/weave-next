package api

import (
	"encoding/json"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"testing"
)

func TestDecodeRunActivityUsageProjectsTerminalFacts(t *testing.T) {
	got, ok := decodeRunActivityUsage([]byte(`{
		"run_id":"run-1","tokens_in":1200,"tokens_out":340,"cost_usd":0.125,"usage_complete":false
	}`))
	if !ok {
		t.Fatal("terminal usage was not decoded")
	}
	if got.TokensIn != 1200 || got.TokensOut != 340 || got.CostUSD != 0.125 || got.CompleteState != "partial" {
		t.Fatalf("usage = %#v", got)
	}
}

func TestDecodeRunActivityUsageTreatsLegacyTerminalAsComplete(t *testing.T) {
	got, ok := decodeRunActivityUsage([]byte(`{"run_id":"run-legacy","tokens_in":0,"tokens_out":0,"cost_usd":0}`))
	if !ok || got.CompleteState != "complete" {
		t.Fatalf("legacy usage = %#v, %v", got, ok)
	}
}

func TestDecodeRunActivityUsageRejectsMalformedRecord(t *testing.T) {
	if _, ok := decodeRunActivityUsage([]byte(`{"tokens_in":1}`)); ok {
		t.Fatal("usage without run identity was accepted")
	}
}

func TestDecodeRunActivityUsageIncludesMemberSubtreeOnce(t *testing.T) {
	entry := loomruntime.TerminalEntryV3{SchemaVersion: 3, RunID: "root", Agent: "lead", Tenant: "ws", AttributionScope: loomruntime.TerminalAttributionLegacyUnattributed,
		Status: "success", StopReason: "completed", StartedAt: "2026-09-07T00:00:00Z", EndedAt: "2026-09-07T00:00:01Z", DurationMs: 1000,
		TokensIn: 4, TokensOut: 2, SelfExclusive: loomruntime.TerminalUsage{InputTokens: 4, OutputTokens: 2},
		ChildBreakdown: []loomruntime.TerminalChildBreakdownV3{{RunID: "member", ParentRunID: "root", ParentSeq: 1, Agent: "worker", SelfExclusive: loomruntime.TerminalUsage{InputTokens: 6, OutputTokens: 3}}},
		SubtreeTotal:   loomruntime.TerminalUsage{InputTokens: 10, OutputTokens: 5}}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := decodeRunActivityUsage(raw)
	if !ok || got.TokensIn != 10 || got.TokensOut != 5 {
		t.Fatalf("subtree usage=%+v valid=%v", got, ok)
	}
	entry.SubtreeTotal.InputTokens = 11
	raw, _ = json.Marshal(entry)
	if _, ok := decodeRunActivityUsage(raw); ok {
		t.Fatal("inconsistent subtree total accepted")
	}
}
