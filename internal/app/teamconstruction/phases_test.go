package teamconstruction

import (
	"context"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

func TestAllowsUnmeasuredUsageRequiresExplicitReasonedWaiver(t *testing.T) {
	if allowsUnmeasuredUsage(teambuild.BuildBrief{}) {
		t.Fatal("missing waiver allowed incomplete usage")
	}
	if allowsUnmeasuredUsage(teambuild.BuildBrief{UnmeasuredUsageWaiver: &teambuild.UnmeasuredUsageWaiver{Accepted: true}}) {
		t.Fatal("reasonless waiver allowed incomplete usage")
	}
	if allowsUnmeasuredUsage(teambuild.BuildBrief{UnmeasuredUsageWaiver: &teambuild.UnmeasuredUsageWaiver{Reason: "CLI receipt unavailable"}}) {
		t.Fatal("unaccepted waiver allowed incomplete usage")
	}
	if !allowsUnmeasuredUsage(teambuild.BuildBrief{UnmeasuredUsageWaiver: &teambuild.UnmeasuredUsageWaiver{
		Accepted: true, Reason: "CLI receipt unavailable during Phase 1",
	}}) {
		t.Fatal("explicit reasoned waiver did not allow incomplete usage")
	}
	incomplete := false
	if !blocksIncompleteUsage(teambuild.EvaluationReport{UsageComplete: &incomplete}, teambuild.BuildBrief{}) {
		t.Fatal("incomplete usage without waiver did not block")
	}
	if blocksIncompleteUsage(
		teambuild.EvaluationReport{UsageComplete: &incomplete},
		teambuild.BuildBrief{UnmeasuredUsageWaiver: &teambuild.UnmeasuredUsageWaiver{
			Accepted: true, Reason: "CLI receipt unavailable during Phase 1",
		}},
	) {
		t.Fatal("explicit waiver did not release incomplete-usage policy")
	}
}

func TestSemanticRubricEvaluationUsesBuildOwnedExecutor(t *testing.T) {
	executor := &fakeSemanticJudgeExecutor{result: teambuild.SemanticJudgeResult{
		AttemptID: "attempt-1", RunID: "run-judge-1",
		Output: `{"schema_version":1,"rubric_scores":[{"dimension_id":"quality","score":8,"reason":"scenario-1 satisfies the expected result"}],"severe_defects":[]}`,
	}}
	phases := &ProductionPhases{Deps: Dependencies{SemanticJudge: executor}}
	round := teamorch.RoundContext{
		WorkspaceID: "workspace-1", BuildRunID: "build-1", RoundNo: 1,
		Run: teambuild.TeamBuildRun{ContractHash: strings.Repeat("a", 64), Contract: teambuild.EvaluationContract{
			Rubric: []teambuild.RubricDimension{{ID: "quality", MaxScore: 10}},
		}},
	}
	result, err := phases.semanticRubricEvaluation(context.Background(), round, []candidateScenarioRun{{
		Scenario: evaluationScenario{ID: "scenario-1", Input: "input", Expected: "expected", InputVersion: "public"},
		RunID:    "candidate-1", Status: "succeeded", Output: "expected",
	}})
	if err != nil {
		t.Fatalf("semanticRubricEvaluation() error = %v", err)
	}
	if !executor.called || executor.request.BuildRunID != "build-1" || len(result.Scores) != 1 || result.Scores[0].Score != 8 {
		t.Fatalf("executor = %#v result = %#v", executor, result)
	}
}

func TestBudgetChargesUseTerminalToolCalls(t *testing.T) {
	round := teamorch.RoundContext{WorkspaceID: "workspace-1", BuildRunID: "build-1", RoundNo: 1}
	marker := loomruntime.TerminalMarkerV1{UsageToolCalls: 7}
	if got := usageChargeFromMarker(round, "builder", "run-1", marker).ToolCalls; got != 7 {
		t.Fatalf("build-agent tool calls = %d, want 7", got)
	}
	if got := usageChargeFromCandidateMarker(round, "candidate", "run-2", marker).ToolCalls; got != 7 {
		t.Fatalf("candidate tool calls = %d, want 7", got)
	}
}

type fakeSemanticJudgeExecutor struct {
	called  bool
	request teambuild.SemanticJudgeRequest
	result  teambuild.SemanticJudgeResult
	err     error
}

func (f *fakeSemanticJudgeExecutor) Execute(_ context.Context, request teambuild.SemanticJudgeRequest) (teambuild.SemanticJudgeResult, error) {
	f.called, f.request = true, request
	return f.result, f.err
}
