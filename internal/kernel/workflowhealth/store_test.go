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

// A workflow with its own slow-run threshold is judged by it and the others
// keep the deployment's; a policy without overrides keeps its config hash, so
// existing observations stay current (#107).
func TestEvaluateUsesAWorkflowsOwnSlowRunThreshold(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	facts := make([]runFact, 3)
	for i := range facts {
		facts[i] = runFact{RunID: string(rune('a' + i)), Status: "succeeded", CreatedAt: now.Add(-3 * time.Minute), TerminalAt: now, FactsComplete: true, UsageComplete: true}
	}
	policy := DefaultPolicy()
	policy.SlowRunOverrides = map[string]time.Duration{"chat": 2 * time.Minute}
	if err := validatePolicy(policy); err != nil {
		t.Fatal(err)
	}
	chat := evaluate(versionKey{WorkspaceID: "w", WorkflowID: "chat", Version: 1, ContentHash: "h"}, facts, policy, now)
	report := evaluate(versionKey{WorkspaceID: "w", WorkflowID: "report", Version: 1, ContentHash: "h"}, facts, policy, now)
	if chat.SlowCount != 3 || chat.Conclusion != "warning" || report.SlowCount != 0 || report.Conclusion != "healthy" {
		t.Fatalf("chat = %+v, report = %+v", chat.Health, report.Health)
	}
	plain := evaluate(versionKey{WorkspaceID: "w", WorkflowID: "report", Version: 1, ContentHash: "h"}, facts, DefaultPolicy(), now)
	empty := DefaultPolicy()
	empty.SlowRunOverrides = map[string]time.Duration{}
	if again := evaluate(versionKey{WorkspaceID: "w", WorkflowID: "report", Version: 1, ContentHash: "h"}, facts, empty, now); again.ConfigHash != plain.ConfigHash {
		t.Fatal("an empty override list changed the config hash")
	}
	if report.ConfigHash == plain.ConfigHash {
		t.Fatal("overrides are not part of the config hash, so changing them would not re-evaluate health")
	}
	for _, bad := range []map[string]time.Duration{{"": time.Minute}, {"chat": 0}} {
		policy.SlowRunOverrides = bad
		if validatePolicy(policy) == nil {
			t.Fatalf("override %v accepted", bad)
		}
	}
}
