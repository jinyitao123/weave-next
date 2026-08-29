package workflowhealth

import (
	"testing"
	"time"
)

func TestEvaluateOperationalHealth(t *testing.T) {
	policy := DefaultPolicy()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	key := versionKey{WorkspaceID: "workspace", WorkflowID: "workflow", Version: 1, ContentHash: "hash"}
	fact := func(id, status string, duration time.Duration) runFact {
		return runFact{RunID: id, Status: status, CreatedAt: now.Add(-duration), TerminalAt: now, FactsComplete: true, UsageComplete: true}
	}

	tests := []struct {
		name       string
		facts      []runFact
		conclusion string
		reason     string
	}{
		{"insufficient", []runFact{fact("run-1", "succeeded", time.Minute)}, "unknown", "insufficient_samples"},
		{"healthy", []runFact{
			fact("run-1", "succeeded", time.Minute), fact("run-2", "succeeded", 2*time.Minute), fact("run-3", "succeeded", 3*time.Minute),
		}, "healthy", ""},
		{"failure warning", []runFact{
			fact("run-1", "failed", time.Minute), fact("run-2", "succeeded", time.Minute), fact("run-3", "succeeded", time.Minute),
		}, "warning", "failure_rate"},
		{"slow warning", []runFact{
			fact("run-1", "succeeded", 11*time.Minute), fact("run-2", "succeeded", 12*time.Minute), fact("run-3", "succeeded", time.Minute),
		}, "warning", "slow_run_rate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluate(key, test.facts, policy, now)
			if got.Conclusion != test.conclusion {
				t.Fatalf("conclusion = %q, want %q", got.Conclusion, test.conclusion)
			}
			if test.reason != "" && (len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != test.reason) {
				t.Fatalf("reason codes = %#v, want %q", got.ReasonCodes, test.reason)
			}
		})
	}
}

func TestEvaluateIncompleteFactsRemainUnknown(t *testing.T) {
	policy := DefaultPolicy()
	now := time.Now().UTC()
	facts := make([]runFact, 3)
	for i := range facts {
		facts[i] = runFact{RunID: string(rune('a' + i)), Status: "succeeded", CreatedAt: now.Add(-time.Minute), TerminalAt: now, FactsComplete: true, UsageComplete: true}
	}
	facts[1].UsageComplete = false
	got := evaluate(versionKey{WorkspaceID: "workspace", WorkflowID: "workflow", Version: 1, ContentHash: "hash"}, facts, policy, now)
	if got.Conclusion != "unknown" || len(got.ReasonCodes) != 1 || got.ReasonCodes[0] != "facts_incomplete" {
		t.Fatalf("health = %#v", got.Health)
	}
}
