package teamorch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

type executionLeaseClock struct{ now time.Time }

func (c executionLeaseClock) Now() time.Time { return c.now }

func TestExecutionQueueRenewRejectsExpiredLeaseRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	runs := teambuild.New(pool, executionLeaseClock{now: now})
	run, err := runs.CreateBuildRun(ctx, "lease-workspace", "lease-build", teambuild.CreateRunParams{
		Brief: teambuild.BuildBrief{
			SchemaVersion: 1, Mode: teambuild.ModeCreate, BusinessDirection: "lease test", Task: "lease test", NewTeamName: "lease-team",
			SuccessCriteria: []string{"lease is fenced"}, AllowedAssets: teambuild.AssetScope{AllowedKinds: []string{"team"}, NamePrefix: "lease-team"},
			RoundBudget: teambuild.Budget{MaxInputTokens: 1}, TotalBudget: teambuild.Budget{MaxInputTokens: 1},
		},
		Contract: teambuild.EvaluationContract{
			SchemaVersion: 1, HardGates: teambuild.DefaultFloorHardGates(),
			Rubric:                 []teambuild.RubricDimension{{ID: "lease", Name: "lease", Description: "lease", MaxScore: 1, PassThreshold: 1}},
			PublicScenarios:        []teambuild.Scenario{{ID: "lease", Input: "lease", Expected: "lease"}},
			SevereDefectDefinition: "lease lost", RunCount: 1, MaxIterations: 3,
			PassRules: []string{"lease valid"}, BlockRules: []string{"lease invalid"}, InfraFailureRules: []string{"database unavailable"},
		},
		ExpiresAt: now.Add(time.Hour), CreatedBy: "lease-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_build_runs SET status='authorized', confirmed_by='lease-test' WHERE workspace_id=$1 AND build_run_id=$2`, run.WorkspaceID, run.BuildRunID); err != nil {
		t.Fatal(err)
	}
	queue := NewExecutionQueue(pool, runs, executionLeaseClock{now: now})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_build_execution_jobs
		(workspace_id,build_run_id,status,requested_at,started_at,updated_at,worker_id,lease_epoch,lease_until)
		VALUES ($1,$2,'running',$3,$3,$3,'worker-1',1,$4)`, run.WorkspaceID, run.BuildRunID, now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	job := ExecutionJob{WorkspaceID: run.WorkspaceID, BuildRunID: run.BuildRunID, WorkerID: "worker-1", LeaseEpoch: 1}
	if err := queue.Renew(ctx, job, time.Minute); err != nil {
		t.Fatalf("renew live lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_build_execution_jobs SET lease_until=$3 WHERE workspace_id=$1 AND build_run_id=$2`, run.WorkspaceID, run.BuildRunID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := queue.Renew(ctx, job, time.Minute); !errors.Is(err, ErrExecutionLeaseLost) {
		t.Fatalf("expired lease renewed: %v", err)
	}
}
