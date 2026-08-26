package teamorch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func TestServiceMapsEvaluationBaselineCASFailureToBlocked(t *testing.T) {
	service := NewService(
		fakeRoundDriver{result: Result{WorkspaceID: "workspace-1", BuildRunID: "run-1", Status: teambuild.StatusPublishing}},
		fakePublisher{err: fmt.Errorf("publish: %w", teambuild.ErrEvaluationBaselineChanged)},
	)
	result, err := service.Execute(context.Background(), "workspace-1", "run-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != teambuild.StatusBlocked || result.StopReason != "evaluation_baseline_cas_failed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSemanticRubricEvaluationUsesBuildOwnedExecutor(t *testing.T) {
	executor := &fakeSemanticJudgeExecutor{result: teambuild.SemanticJudgeResult{
		AttemptID: "attempt-1", RunID: "run-judge-1",
		Output: `{"schema_version":1,"rubric_scores":[{"dimension_id":"quality","score":8,"reason":"scenario-1 satisfies the expected result"}],"severe_defects":[]}`,
	}}
	phases := &ProductionPhases{Deps: PhaseDeps{SemanticJudge: executor}}
	round := RoundContext{
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

type fakeRoundDriver struct {
	result Result
	err    error
}

func (f fakeRoundDriver) Run(context.Context, string, string) (Result, error) {
	return f.result, f.err
}

type fakePublisher struct{ err error }

func (f fakePublisher) PublishStep(context.Context, string, string) error { return f.err }

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
