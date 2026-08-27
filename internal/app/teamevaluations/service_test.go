package teamevaluations

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func TestPlaceholderProblemsRejectsEveryScenarioIdentifierFieldAndPrefixVariant(t *testing.T) {
	contract := teambuild.EvaluationContract{
		PublicScenarios: []teambuild.Scenario{{ID: " post_evaluation_placeholder_public"}},
		PerturbationScenarios: []teambuild.PerturbationScenario{
			{ID: "POST_EVALUATION_PLACEHOLDER-perturb", BaseScenarioID: "post_evaluation_placeholder_base"},
		},
	}
	problems := placeholderProblems(contract)
	if len(problems) != 3 {
		t.Fatalf("problems = %#v, want every identifier field rejected", problems)
	}
	wantPaths := []string{
		"/contract/public_scenarios/0/id",
		"/contract/perturbation_scenarios/0/id",
		"/contract/perturbation_scenarios/0/base_scenario_id",
	}
	for index, want := range wantPaths {
		if problems[index].Path != want || problems[index].Code != "evaluation_placeholder_forbidden" {
			t.Fatalf("problem[%d] = %#v, want path %q", index, problems[index], want)
		}
	}
}

func TestPlaceholderProblemsAllowsRealIdentifiers(t *testing.T) {
	contract := teambuild.EvaluationContract{
		PublicScenarios: []teambuild.Scenario{{ID: "invoice-reconciliation"}},
		PerturbationScenarios: []teambuild.PerturbationScenario{
			{ID: "invoice-reconciliation-late-fee", BaseScenarioID: "invoice-reconciliation"},
		},
	}
	if problems := placeholderProblems(contract); len(problems) != 0 {
		t.Fatalf("problems = %#v", problems)
	}
}

func TestDeterministicBuildRunIDIsWorkspaceScoped(t *testing.T) {
	key := uuid.MustParse("e7c03b33-bfac-4a34-a443-805319f7fa24")
	first := deterministicBuildRunID("workspace-1", key)
	if first != deterministicBuildRunID("workspace-1", key) {
		t.Fatal("same evaluation request did not converge")
	}
	if first == deterministicBuildRunID("workspace-2", key) {
		t.Fatal("different workspaces shared one build run identity")
	}
	if !strings.HasPrefix(first, "br-eval-") {
		t.Fatalf("build run id = %q", first)
	}
}

func TestEvaluationBudgetSupportsEveryMeasuredDimension(t *testing.T) {
	budget := Budget{MaxInputTokens: 10, MaxOutputTokens: 20, MaxToolCalls: 3, MaxCostUSD: 1.5}
	if err := validateEvaluationBudget(budget); err != nil {
		t.Fatal(err)
	}
	if got := evaluationBuildBudget(budget); got != (teambuild.Budget{
		MaxInputTokens: 10, MaxOutputTokens: 20, MaxToolCalls: 3, MaxCostUSD: 1.5,
	}) {
		t.Fatalf("build budget = %#v", got)
	}
	if err := validateEvaluationBudget(Budget{MaxToolCalls: 1}); err != nil {
		t.Fatalf("tool-call-only budget rejected: %v", err)
	}
	if err := validateEvaluationBudget(Budget{}); err == nil {
		t.Fatal("empty evaluation budget accepted")
	}
	if err := validateEvaluationBudget(Budget{MaxInputTokens: -1}); err == nil {
		t.Fatal("negative evaluation budget accepted")
	}
}
