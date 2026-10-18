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

func TestCarryForwardYieldedUsagePreservesEarlierObservedTotals(t *testing.T) {
	candidate := loomruntime.TerminalEntryV3{
		TokensIn: 10, TokensOut: 20, CostUSD: 0.25, ToolCalls: 1,
		SelfExclusive: loomruntime.TerminalUsage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.25, ToolCalls: 1},
		ChildBreakdown: []loomruntime.TerminalChildBreakdownV3{{
			SelfExclusive: loomruntime.TerminalUsage{InputTokens: 3, OutputTokens: 5, CostUSD: 0.1, ToolCalls: 1},
		}},
		SubtreeTotal: loomruntime.TerminalUsage{InputTokens: 13, OutputTokens: 25, CostUSD: 0.35, ToolCalls: 2},
	}
	marker := loomruntime.TerminalMarkerV1{
		UsageInputTokens: 12, UsageOutputTokens: 20, UsageCostUSD: 0.5, UsageToolCalls: 2,
	}

	carryForwardYieldedUsage(&candidate, marker)

	if got := candidate.SelfExclusive; got.InputTokens != 12 || got.OutputTokens != 20 || got.CostUSD != 0.5 || got.ToolCalls != 2 {
		t.Fatalf("self exclusive usage = %+v", got)
	}
	if candidate.TokensIn != 12 || candidate.TokensOut != 20 || candidate.CostUSD != 0.5 || candidate.ToolCalls != 2 {
		t.Fatalf("legacy terminal totals were not updated: %+v", candidate)
	}
	if got := candidate.SubtreeTotal; got.InputTokens != 15 || got.OutputTokens != 25 || got.CostUSD != 0.6 || got.ToolCalls != 3 {
		t.Fatalf("subtree total = %+v", got)
	}
}

func TestTeamTerminalCacheBreakdownAndCheckpointFloor(t *testing.T) {
	started := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	run := TeamRun{WorkspaceID: "workspace", RunID: "parent", TeamID: "team", WorkflowID: "workflow", WorkflowVersion: 1, RunSnapshotID: "snapshot"}
	lease := loomruntime.RunAttemptLease{WorkspaceID: run.WorkspaceID, RunID: run.RunID, AttemptGeneration: 1, AttemptID: uuid.New(), GraphName: frozenGraphName(run), RunStartedAt: started.Format(time.RFC3339Nano)}
	usage := loomruntime.UsageTotals{InputTokens: 100, CacheReadTokens: 80, CacheWriteTokens: 10}
	coverage := loomruntime.UsageCoverage{HasTokens: true, HasCost: true, HasCacheRead: true, HasCacheWrite: true}
	candidate, err := frozenTerminalCandidate(run, lease, "succeeded", "completed", started.Add(time.Second), usage, &coverage, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	child := loomruntime.TerminalChildBreakdownV3{RunID: "child", ParentRunID: run.RunID, ParentSeq: 1, Agent: "worker", TeamID: &run.TeamID, WorkflowID: &run.WorkflowID, WorkflowVersion: &run.WorkflowVersion, RunSnapshotID: &run.RunSnapshotID, SelfExclusive: candidate.SelfExclusive}
	if err := applyMemberTerminalBreakdown(&candidate, map[string]loomruntime.TerminalChildBreakdownV3{"child": child}); err != nil {
		t.Fatal(err)
	}
	if candidate.SelfExclusive.CacheReadTokens != 0 || candidate.SubtreeTotal.CacheReadTokens != 80 || candidate.SubtreeTotal.CacheWriteTokens != 10 || !candidate.SubtreeTotal.CacheReadComplete {
		t.Fatalf("cache was lost or double-counted in member breakdown: %+v", candidate)
	}
	floor := carryCheckpointUsageFloor(loomruntime.UsageTotals{}, usage)
	if floor != usage || carryCheckpointUsageFloor(floor, usage) != usage {
		t.Fatalf("yielded checkpoint cache was erased or counted twice: %+v", floor)
	}
}
