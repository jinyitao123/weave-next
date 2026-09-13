package teamevaluations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestEvaluationBaselineCaptureDoesNotReenterProductPoolTransactionRealPG(t *testing.T) {
	fixture := newEvaluationPGFixture(t)
	cfg := fixture.pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(fixture.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)

	artifacts := workflow.NewArtifactStore(single, nil)
	workflows := workflowcatalog.New(single, workflow.RealClock{}, artifacts)
	agents := agentcatalog.New(single)
	organizations := orgstore.NewStore(single)
	builds := teambuild.New(single, teambuild.RealClock{})
	builds.SetBaselineSources(organizations, agents, workflows, artifacts)
	service := New(NewPGIdempotencyStore(single), builds, noopEvaluationSubmitter{}, Options{
		OrgStore: organizations, Registry: agents, Workflows: workflows, Artifacts: artifacts,
	})
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	outcome, err := service.Evaluate(ctx, fixture.workspaceID, "admin-1", fixture.team.ID, Request{
		Contract: fixture.contract, IdempotencyKey: uuid.NewString(), Budget: Budget{MaxCostUSD: 5},
	})
	if err != nil {
		t.Fatalf("single-connection evaluation baseline: %v", err)
	}
	if outcome.Status != teambuild.StatusAuthorized {
		t.Fatalf("single-connection evaluation outcome = %#v", outcome)
	}
	if _, err := builds.TransitionStatus(ctx, fixture.workspaceID, outcome.BuildRunID, teambuild.StatusAuthorized, teambuild.StatusRoundRunning, "worker", "candidate started"); err != nil {
		t.Fatal(err)
	}
	if _, err := builds.TransitionStatus(ctx, fixture.workspaceID, outcome.BuildRunID, teambuild.StatusRoundRunning, teambuild.StatusPublishing, "worker", "candidate passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := single.Exec(ctx, `UPDATE weave_team_workflows SET description='changed after authorization',updated_at=updated_at+interval '1 second' WHERE workspace_id=$1 AND id=$2`, fixture.workspaceID, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	tx, err := single.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := builds.VerifyEvaluationBaselineTx(ctx, tx, fixture.workspaceID, outcome.BuildRunID); !errors.Is(err, teambuild.ErrEvaluationBaselineChanged) {
		t.Fatalf("single-connection workflow CAS error = %v", err)
	}
}
