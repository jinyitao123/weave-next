package teamrun

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

func TestFrozenTerminalCandidateMarksIncompleteReceipt(t *testing.T) {
	started := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	run := TeamRun{
		WorkspaceID: "workspace-1", RunID: "run-1", TeamID: "team-1",
		WorkflowID: "workflow-1", WorkflowVersion: 1, RunSnapshotID: "snapshot-1",
	}
	lease := loomruntime.RunAttemptLease{
		WorkspaceID: "workspace-1", RunID: "run-1", AttemptGeneration: 1,
		AttemptID: uuid.New(), GraphName: frozenGraphName(run),
		RunStartedAt: started.Format(time.RFC3339Nano),
	}

	candidate, err := frozenTerminalCandidate(
		run, lease, "failed", string(ErrorCodeRuntimeIncompatible), started.Add(time.Second),
		loomruntime.UsageTotals{}, &loomruntime.UsageCoverage{HasTokens: false, HasCost: false},
		true, "", nil,
	)
	if err != nil {
		t.Fatalf("build terminal candidate: %v", err)
	}
	if candidate.UsageComplete == nil || *candidate.UsageComplete {
		t.Fatalf("usage_complete = %v, want false", candidate.UsageComplete)
	}
	if candidate.UsageIncompleteReason != UsageIncompleteReasonAttemptLost {
		t.Fatalf("usage_incomplete_reason = %q", candidate.UsageIncompleteReason)
	}
	if err := loomruntime.ValidateTerminalV3(candidate); err != nil {
		t.Fatalf("validate terminal candidate: %v", err)
	}
}
