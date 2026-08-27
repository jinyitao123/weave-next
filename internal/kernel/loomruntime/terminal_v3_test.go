package loomruntime

import "testing"

func TestTerminalV3ValidatesDimensionLevelCLIUsageMetadata(t *testing.T) {
	falseValue := false
	trueValue := true
	entry := TerminalEntryV3{
		SchemaVersion: 3, RunID: "run-1", Agent: "agent-1", Tenant: "workspace-1",
		AttributionScope: TerminalAttributionLegacyUnattributed,
		Status:           "failed", StopReason: "execution_unrecoverable",
		StartedAt: "2026-08-27T10:00:00Z", EndedAt: "2026-08-27T10:00:01Z", DurationMs: 1000,
		TokensIn: 21, TokensOut: 4,
		UsageComplete: &falseValue, UsageIncompleteReason: "CLI node without usage receipt",
		UsageHasTokens: &trueValue, UsageHasCost: &falseValue, UsageSources: []string{"cli-reported"},
		SelfExclusive:  TerminalUsage{InputTokens: 21, OutputTokens: 4},
		ChildBreakdown: []TerminalChildBreakdownV3{},
		SubtreeTotal:   TerminalUsage{InputTokens: 21, OutputTokens: 4},
	}
	if err := ValidateTerminalV3(entry); err != nil {
		t.Fatalf("dimension-level CLI terminal rejected: %v", err)
	}
	entry.UsageComplete = &trueValue
	if err := ValidateTerminalV3(entry); err == nil {
		t.Fatal("cost-incomplete terminal accepted usage_complete=true")
	}
}
