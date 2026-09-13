package teamconstruction

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestCLIReceiptTerminalFlowsIntoEvaluationReportRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	falseValue, trueValue := false, true
	entry := loomruntime.TerminalEntryV3{
		SchemaVersion: 3, RunID: "candidate-cli-report", Agent: "candidate", Tenant: "workspace-cli-report",
		AttributionScope: loomruntime.TerminalAttributionLegacyUnattributed,
		Status:           "success", StopReason: "completed",
		StartedAt: "2026-08-27T10:00:00Z", EndedAt: "2026-08-27T10:00:01Z", DurationMs: 1000,
		TokensIn: 17, TokensOut: 5,
		UsageComplete: &falseValue, UsageIncompleteReason: "CLI receipt did not report cost",
		UsageHasTokens: &trueValue, UsageHasCost: &falseValue, UsageSources: []string{"cli-reported"},
		SelfExclusive:  loomruntime.TerminalUsage{InputTokens: 17, OutputTokens: 5},
		ChildBreakdown: []loomruntime.TerminalChildBreakdownV3{},
		SubtreeTotal:   loomruntime.TerminalUsage{InputTokens: 17, OutputTokens: 5},
	}
	if err := loomruntime.ValidateTerminalV3(entry); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeext.New(pool).MutateValue(ctx, "audit:"+entry.Tenant, entry.RunID, func([]byte, bool) ([]byte, error) {
		return raw, nil
	}); err != nil {
		t.Fatal(err)
	}
	phases := &ProductionPhases{Deps: Dependencies{
		Pool: pool, Agents: agentcatalog.New(pool), TeamWorkers: agentcatalog.NewTeamWorkerRepository(pool),
	}}
	report := phases.assembleReport(
		ctx,
		teamorch.RoundContext{WorkspaceID: entry.Tenant, RoundNo: 1, Run: teambuild.TeamBuildRun{}},
		&org.Team{ID: "team-cli-report", Name: "team-cli-report"},
		&workflow.PublicationCandidate{ContentHash: "candidate-hash", WorkflowID: "workflow-cli", WorkflowVersion: 1},
		[]candidateScenarioRun{{Scenario: evaluationScenario{ID: "scenario-1"}, RunID: entry.RunID, Status: "success"}},
		nil,
	)
	if report.InputTokens != 17 || report.OutputTokens != 5 || report.CostUSD != 0 ||
		report.UsageHasTokens == nil || !*report.UsageHasTokens ||
		report.UsageHasCost == nil || *report.UsageHasCost ||
		len(report.UsageSources) != 1 || report.UsageSources[0] != "cli-reported" ||
		report.UsageComplete == nil || *report.UsageComplete {
		t.Fatalf("CLI evaluation report = %#v", report)
	}
}

func TestCLIReportedCandidateUsageBlocksBeforeRuntimeDependenciesRealPG(t *testing.T) {
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
	round := teamorch.RoundContext{WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID, RoundNo: 1, Run: run}
	// The candidate terminal marker is populated from the TeamRun terminal,
	// whose CLI self-exclusive usage is sourced only from the validated
	// cli-reported receipt. No price/token estimate is introduced here.
	charge := usageChargeFromCandidateMarker(round, teambuild.SourceRoleFixedWorkflowRoot, "candidate-cli-run", loomruntime.TerminalMarkerV1{
		UsageInputTokens: 11,
	})
	if charge.SourceKind != teambuild.UsageSourceKindCandidateRuntime || charge.InputTokens != 11 {
		t.Fatalf("CLI candidate charge = %#v", charge)
	}
	if _, err := builds.RecordBudgetUsage(ctx, run.WorkspaceID, run.BuildRunID, charge); err != nil {
		t.Fatal(err)
	}

	// Build is the only dependency supplied. Reaching target resolution,
	// candidate runtime, or semantic judge would fail this test; G1 must return
	// first without any candidate/LLM call.
	phases := &ProductionPhases{Deps: Dependencies{Build: builds}}
	evaluation, err := phases.Evaluate(ctx, round)
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
