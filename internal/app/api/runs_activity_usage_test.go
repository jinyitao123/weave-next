package api

import "testing"

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
