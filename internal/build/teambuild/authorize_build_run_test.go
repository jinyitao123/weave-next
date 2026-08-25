package teambuild

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestAuthorizeBuildRunCreateRequiresCompilerRevision(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run := createAuthorizeTestRun(t, ctx, store, "build-create", ModeCreate)

	_, receipt, err := store.AuthorizeBuildRun(
		ctx, run.WorkspaceID, run.BuildRunID, "admin-1", nil,
		AuthorizeOptions{Authority: AuthorizationAutoBuild},
	)
	if !errors.Is(err, ErrCompilerRevisionRequired) {
		t.Fatalf("authorize err = %v, want ErrCompilerRevisionRequired", err)
	}
	if receipt.Valid() {
		t.Fatalf("receipt should not be valid on failed authorization: %#v", receipt)
	}
	assertAuthorizeRunStatus(t, store, run.WorkspaceID, run.BuildRunID, StatusPlanning)
}

func TestAuthorizeBuildRunOptimizeRequiresBaselineSources(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run := createAuthorizeTestRun(t, ctx, store, "build-optimize", ModeOptimize)

	_, receipt, err := store.AuthorizeBuildRun(
		ctx, run.WorkspaceID, run.BuildRunID, "admin-1", nil,
		AuthorizeOptions{Authority: AuthorizationAutoBuild},
	)
	if !errors.Is(err, ErrBaselineSourceUnavailable) {
		t.Fatalf("authorize err = %v, want ErrBaselineSourceUnavailable", err)
	}
	if receipt.Valid() {
		t.Fatalf("receipt should not be valid on failed authorization: %#v", receipt)
	}
	assertAuthorizeRunStatus(t, store, run.WorkspaceID, run.BuildRunID, StatusPlanning)
}

func TestAuthorizeBuildRunHashMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run := createAuthorizeTestRun(t, ctx, store, "build-hash-mismatch", ModeCreate)
	_, err := store.pool.Exec(ctx, `
		UPDATE weave_team_build_runs
		SET brief_hash='0000000000000000000000000000000000000000000000000000000000000000'
		WHERE workspace_id=$1 AND build_run_id=$2
	`, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatalf("corrupt brief hash: %v", err)
	}

	_, receipt, err := store.AuthorizeBuildRun(
		ctx, run.WorkspaceID, run.BuildRunID, "admin-1", nil,
		AuthorizeOptions{Authority: AuthorizationAutoBuild},
	)
	if err == nil || !strings.Contains(err.Error(), "stored hash does not match frozen content") {
		t.Fatalf("authorize err = %v, want stored hash mismatch", err)
	}
	if receipt.Valid() {
		t.Fatalf("receipt should not be valid on failed authorization: %#v", receipt)
	}
	assertAuthorizeRunStatus(t, store, run.WorkspaceID, run.BuildRunID, StatusPlanning)
}

func newAuthorizeTestStore(t *testing.T) *Store {
	t.Helper()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return New(pool, authorizeFixedClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
}

func createAuthorizeTestRun(
	t *testing.T,
	ctx context.Context,
	store *Store,
	buildRunID string,
	mode string,
) TeamBuildRun {
	t.Helper()
	run, err := store.CreateBuildRun(ctx, "workspace-1", buildRunID, CreateRunParams{
		Brief:     authorizeTestBrief(mode),
		Contract:  authorizeTestContract(),
		ExpiresAt: time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC),
		CreatedBy: "admin-1",
	})
	if err != nil {
		t.Fatalf("create build run: %v", err)
	}
	return run
}

func authorizeTestBrief(mode string) BuildBrief {
	brief := BuildBrief{
		SchemaVersion:     1,
		Mode:              mode,
		BusinessDirection: "make the platform build team workflows",
		Task:              "create or improve a team workflow",
		SuccessCriteria:   []string{"workflow can be executed"},
		RoundBudget:       Budget{MaxInputTokens: 1000},
		TotalBudget:       Budget{MaxInputTokens: 5000},
	}
	if mode == ModeOptimize {
		brief.TeamID = "team-1"
		brief.AllowedAssets = AssetScope{
			AllowedKinds: []string{"team", "agent", "workflow"},
			Refs: []AssetRef{
				{Kind: "team", ID: "team-1"},
				{Kind: "workflow", ID: FirstOptimizeWorkflowID("team-1")},
			},
		}
		return brief
	}
	brief.NewTeamName = "新的流程团队"
	brief.AllowedAssets = AssetScope{
		AllowedKinds: []string{"team", "agent", "workflow"},
		NamePrefix:   "新的流程团队",
	}
	return brief
}

func authorizeTestContract() EvaluationContract {
	return EvaluationContract{
		SchemaVersion: 1,
		HardGates:     DefaultFloorHardGates(),
		Rubric: []RubricDimension{{
			ID: "quality", Name: "Quality", Description: "Useful result",
			MaxScore: 10, PassThreshold: 7,
		}},
		PublicScenarios: []Scenario{{
			ID: "happy-path", Input: "build a workflow", Expected: "workflow ready",
		}},
		HiddenScenarioCount:    0,
		SevereDefectDefinition: "unsafe or unusable workflow",
		RunCount:               1,
		MaxIterations:          maxContractIterations,
		PassRules:              []string{"all floor gates pass"},
		BlockRules:             []string{"hard gate fails"},
		InfraFailureRules:      []string{"runtime unavailable"},
	}
}

func assertAuthorizeRunStatus(t *testing.T, store *Store, workspaceID, buildRunID, status string) {
	t.Helper()
	run, err := store.GetBuildRun(context.Background(), workspaceID, buildRunID)
	if err != nil {
		t.Fatalf("get build run: %v", err)
	}
	if run.Status != status || run.ConfirmedBy != "" || run.Authorization.Authority != "" || run.Baseline != nil {
		t.Fatalf("run was not left in planning: %#v", run)
	}
}

type authorizeFixedClock struct {
	now time.Time
}

func (c authorizeFixedClock) Now() time.Time { return c.now }
