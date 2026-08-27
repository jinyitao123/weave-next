package teamorch

import (
	"context"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
)

func TestCompilerCandidateG1BlocksBeforeRuntimeDependenciesRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	builds := teambuild.New(pool, teambuild.RealClock{})
	run, err := builds.CreateBuildRun(ctx, "workspace-g1", "build-g1", teambuild.CreateRunParams{
		Brief: teambuild.BuildBrief{
			SchemaVersion: 1, Mode: teambuild.ModeCreate,
			BusinessDirection: "验证候选启动预算闸", Task: "执行候选测试",
			NewTeamName: "g1-budget-team", SuccessCriteria: []string{"候选测试通过"},
			AllowedAssets: teambuild.AssetScope{
				AllowedKinds: []string{"agent", "team", "workflow"}, NamePrefix: "g1-budget-team",
			},
			RoundBudget: teambuild.Budget{MaxInputTokens: 10},
			TotalBudget: teambuild.Budget{MaxInputTokens: 10},
		},
		Contract: teambuild.EvaluationContract{
			SchemaVersion: 1, HardGates: teambuild.DefaultFloorHardGates(),
			Rubric: []teambuild.RubricDimension{{
				ID: "quality", Name: "Quality", Description: "Candidate quality",
				MaxScore: 10, PassThreshold: 7,
			}},
			PublicScenarios:        []teambuild.Scenario{{ID: "scenario-1", Input: "input", Expected: "output"}},
			SevereDefectDefinition: "unsafe output", RunCount: 1, MaxIterations: 3,
			PassRules: []string{"all gates pass"}, BlockRules: []string{"hard gate fails"},
			InfraFailureRules: []string{"runtime unavailable"},
		},
		ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedBy: "admin-1",
		ExecutionStrategy: teambuild.ExecutionStrategyCompilerV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builds.RecordBudgetUsage(ctx, run.WorkspaceID, run.BuildRunID, teambuild.BudgetCharge{
		WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID, RoundNo: 1,
		SourceKind:  teambuild.UsageSourceKindBuildAgent,
		SourceRole:  teambuild.SourceRoleFixedWorkflowRoot,
		SourceRunID: "materialization-usage", InputTokens: 11,
	}); err != nil {
		t.Fatal(err)
	}

	// Build is the only dependency supplied. Reaching target resolution,
	// candidate runtime, or semantic judge would fail this test; G1 must return
	// first without any candidate/LLM call.
	phases := &ProductionPhases{Deps: PhaseDeps{Build: builds}}
	evaluation, err := phases.Evaluate(ctx, RoundContext{
		WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID, RoundNo: 1, Run: run,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Conclusion != teambuild.ConclusionBlocked ||
		evaluation.Diagnosis.Class != teameval.FailureClassBudgetExhausted ||
		evaluation.Diagnosis.OriginalErrorCode != teambuild.BudgetExhaustedReason {
		t.Fatalf("G1 evaluation = %#v", evaluation)
	}
	blocked, err := builds.TransitionStatus(
		ctx, run.WorkspaceID, run.BuildRunID,
		teambuild.StatusPlanning, teambuild.StatusBlocked,
		"controller", teambuild.BudgetExhaustedReason,
	)
	if err != nil || blocked.Status != teambuild.StatusBlocked {
		t.Fatalf("G1 blocked run = %#v, err = %v", blocked, err)
	}
	var candidateSources int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM weave_team_build_run_usage_sources
		WHERE workspace_id=$1 AND build_run_id=$2 AND source_kind='candidate_runtime'
	`, run.WorkspaceID, run.BuildRunID).Scan(&candidateSources); err != nil || candidateSources != 0 {
		t.Fatalf("candidate sources after G1 = %d, err = %v", candidateSources, err)
	}
}
