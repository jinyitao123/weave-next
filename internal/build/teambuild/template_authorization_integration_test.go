package teambuild_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
)

func TestTemplateAutoAuthorizationBindsRevisionAndEnforcesConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	clock := templateAuthorizationClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)}
	store := teambuild.New(pool, clock)
	policy := teambuild.TemplateAuthorizationPolicy{
		AutoBudgetThresholdUSD: 5, DailyBudgetUSD: 25, MonthlyBudgetUSD: 250, MaxConcurrent: 1,
	}

	run, token := createTemplateAuthorizationRun(t, ctx, store, "workspace-1", "auto-1", 2)
	authorized, receipt, err := store.AuthorizeTemplateBuildRun(ctx, run.WorkspaceID, run.BuildRunID, "user-1", token, policy)
	if err != nil {
		t.Fatalf("AuthorizeTemplateBuildRun() error = %v", err)
	}
	if authorized.Authorization.Authority != teambuild.AuthorizationTemplateAuto ||
		authorized.Authorization.RevisionToken == nil || *authorized.Authorization.RevisionToken != token ||
		authorized.Authorization.DecisionSubject != teambuild.TemplateAuthorizerSubject ||
		!strings.Contains(authorized.Authorization.DecisionReason, "within automatic threshold") ||
		authorized.ConfirmedBy != "user-1" {
		t.Fatalf("authorized run = %#v", authorized)
	}
	if !receipt.Valid() || receipt.Authority() != teambuild.AuthorizationTemplateAuto ||
		receipt.DecisionSubject() != teambuild.TemplateAuthorizerSubject || receipt.RevisionToken() == nil ||
		*receipt.RevisionToken() != token {
		t.Fatalf("receipt did not bind template decision: valid=%v authority=%q", receipt.Valid(), receipt.Authority())
	}

	second, secondToken := createTemplateAuthorizationRun(t, ctx, store, "workspace-1", "auto-2", 2)
	_, _, err = store.AuthorizeTemplateBuildRun(ctx, second.WorkspaceID, second.BuildRunID, "user-2", secondToken, policy)
	if !errors.Is(err, teambuild.ErrTemplateConcurrencyExceeded) {
		t.Fatalf("second authorization error = %v, want concurrency limit", err)
	}
}

func TestTemplateAutoAuthorizationOverThresholdStaysPlanning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := teambuild.New(pool, templateAuthorizationClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
	run, token := createTemplateAuthorizationRun(t, ctx, store, "workspace-2", "manual-required", 6)
	policy := teambuild.TemplateAuthorizationPolicy{
		AutoBudgetThresholdUSD: 5, DailyBudgetUSD: 25, MonthlyBudgetUSD: 250, MaxConcurrent: 2,
	}
	_, _, err := store.AuthorizeTemplateBuildRun(ctx, run.WorkspaceID, run.BuildRunID, "user-1", token, policy)
	if !errors.Is(err, teambuild.ErrTemplateAuthorizationRequired) {
		t.Fatalf("authorization error = %v, want manual authorization", err)
	}
	current, err := store.GetBuildRun(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != teambuild.StatusPlanning || current.Authorization.Authority != "" {
		t.Fatalf("rejected run mutated = %#v", current)
	}
}

func TestTemplateAutoAuthorizationEnforcesDailyAndMonthlyReservations(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	store := teambuild.New(pool, templateAuthorizationClock{now: now})
	policy := teambuild.TemplateAuthorizationPolicy{
		AutoBudgetThresholdUSD: 5, DailyBudgetUSD: 5, MonthlyBudgetUSD: 5, MaxConcurrent: 10,
	}

	dailyFirst, dailyFirstToken := createTemplateAuthorizationRun(t, ctx, store, "workspace-daily", "daily-1", 4)
	if _, _, err := store.AuthorizeTemplateBuildRun(ctx, dailyFirst.WorkspaceID, dailyFirst.BuildRunID, "user-1", dailyFirstToken, policy); err != nil {
		t.Fatal(err)
	}
	dailySecond, dailySecondToken := createTemplateAuthorizationRun(t, ctx, store, "workspace-daily", "daily-2", 2)
	if _, _, err := store.AuthorizeTemplateBuildRun(ctx, dailySecond.WorkspaceID, dailySecond.BuildRunID, "user-2", dailySecondToken, policy); !errors.Is(err, teambuild.ErrTemplateDailyQuotaExceeded) {
		t.Fatalf("daily authorization error = %v", err)
	}

	monthlyFirst, monthlyFirstToken := createTemplateAuthorizationRun(t, ctx, store, "workspace-monthly", "monthly-1", 4)
	if _, _, err := store.AuthorizeTemplateBuildRun(ctx, monthlyFirst.WorkspaceID, monthlyFirst.BuildRunID, "user-1", monthlyFirstToken, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE weave_team_build_runs
		SET authorization_decided_at=$3
		WHERE workspace_id=$1 AND build_run_id=$2
	`, monthlyFirst.WorkspaceID, monthlyFirst.BuildRunID, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("move first reservation to prior day: %v", err)
	}
	monthlySecond, monthlySecondToken := createTemplateAuthorizationRun(t, ctx, store, "workspace-monthly", "monthly-2", 2)
	if _, _, err := store.AuthorizeTemplateBuildRun(ctx, monthlySecond.WorkspaceID, monthlySecond.BuildRunID, "user-2", monthlySecondToken, policy); !errors.Is(err, teambuild.ErrTemplateMonthlyQuotaExceeded) {
		t.Fatalf("monthly authorization error = %v", err)
	}
}

func createTemplateAuthorizationRun(
	t *testing.T,
	ctx context.Context,
	store *teambuild.Store,
	workspaceID, buildRunID string,
	budget float64,
) (teambuild.TeamBuildRun, teambuild.BlueprintRevisionToken) {
	t.Helper()
	yaml := fmt.Sprintf(`schema: team-template/v1
name: %s-team
display_name: 自动授权团队
purpose: 持续完成调研
template: research_synthesis
template_parameters:
  lead_instruction: 协调调研
  parallel_worker_refs: [researcher, analyst]
  finalizer_ref: lead
  result_requirements: {researcher: 给出来源, analyst: 交叉验证, lead: 汇总报告}
members:
  - {name: lead, display_name: 负责人, role: avatar, responsibilities: [协调], capabilities: [delegation]}
  - {name: researcher, display_name: 调研员, role: worker, responsibilities: [调研], capabilities: [search]}
  - {name: analyst, display_name: 分析员, role: worker, responsibilities: [分析], capabilities: [analysis]}
lead: lead
delivery: {success_criteria: [结果可验证]}
budget: {max_cost_usd: %.2f}
`, buildRunID, budget)
	compiled, err := teamtemplate.CompileYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("compile template: %v", err)
	}
	run, err := store.CreateBuildRun(ctx, workspaceID, buildRunID, teambuild.CreateRunParams{
		Brief: compiled.Brief, Contract: compiled.Contract,
		ExpiresAt: time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC), CreatedBy: "user-1",
		ExecutionStrategy: teambuild.ExecutionStrategyTemplateInstantiate,
	})
	if err != nil {
		t.Fatalf("create build run: %v", err)
	}
	changeSet, err := teamforge.CompileTemplateInstantiateChangeSetV1(teamforge.EmptyCreateBaselineV1(), compiled.Blueprint)
	if err != nil {
		t.Fatalf("compile ChangeSet: %v", err)
	}
	blueprintJSON, err := json.Marshal(compiled.Blueprint)
	if err != nil {
		t.Fatal(err)
	}
	blueprintHash, err := compiled.Blueprint.BlueprintHash()
	if err != nil {
		t.Fatal(err)
	}
	changeSetJSON, err := changeSet.TemplateInstantiateCanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	changeSetHash, err := changeSet.TemplateInstantiateCanonicalHash()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.PersistCompilerAuthorizationBundle(ctx, workspaceID, buildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo: 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash:           teamforge.EmptyCreateBaselineHashV1,
		EvaluationContractHash: run.ContractHash,
	})
	if err != nil {
		t.Fatalf("persist revision: %v", err)
	}
	return run, teambuild.BlueprintRevisionToken{
		RevisionNo: revision.RevisionNo, BlueprintHash: revision.BlueprintHash, ChangeSetHash: revision.ChangeSetHash,
	}
}

type templateAuthorizationClock struct{ now time.Time }

func (c templateAuthorizationClock) Now() time.Time { return c.now }
