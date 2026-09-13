package fanout

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestCreatorAttemptUnavailablePredicates(t *testing.T) {
	plan := WorkflowResumePlan{CreatorAttemptGeneration: 2, CreatorAttemptID: "attempt-a"}
	now := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)

	if creatorAttemptFinalOrMoved(plan, CreatorLeaseState{
		AttemptGeneration: 2, AttemptID: "attempt-a",
		LeaseExpiresAt: now.Add(time.Minute),
	}) {
		t.Fatal("matching nonterminal lease should be resumable")
	}
	if !creatorAttemptFinalOrMoved(plan, CreatorLeaseState{
		AttemptGeneration: 2, AttemptID: "attempt-a", MarkerPhase: "final",
		LeaseExpiresAt: now.Add(time.Minute),
	}) {
		t.Fatal("final creator marker should not be resumable")
	}
	if !creatorAttemptFinalOrMoved(plan, CreatorLeaseState{
		AttemptGeneration: 3, AttemptID: "attempt-a",
		LeaseExpiresAt: now.Add(time.Minute),
	}) {
		t.Fatal("moved attempt generation should not be resumable")
	}
	if !creatorAttemptFinalOrMoved(plan, CreatorLeaseState{
		AttemptGeneration: 2, AttemptID: "attempt-b",
		LeaseExpiresAt: now.Add(time.Minute),
	}) {
		t.Fatal("moved attempt ID should not be resumable")
	}
	if creatorAttemptFinalOrMoved(plan, CreatorLeaseState{
		AttemptGeneration: 2, AttemptID: "attempt-a",
		LeaseExpiresAt: now.Add(-time.Second),
	}) {
		t.Fatal("expired creator lease alone must not block a checkpoint-backed claim")
	}
	if !creatorAttemptUnavailableAfterMissingCheckpoint(plan, CreatorLeaseState{
		AttemptGeneration: 2, AttemptID: "attempt-a",
		LeaseExpiresAt: now.Add(-time.Second),
	}, now) {
		t.Fatal("expired creator lease should close only after the checkpoint is missing")
	}
}

func TestReconcileGroupClosesClaimedResumeWhenCheckpointMissingAndCreatorLeaseExpired(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	now := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	store := New(pool, testutil.NewFakeClock(now))
	workspaceID := "workspace-" + uuid.NewString()
	parentRunID := "run-" + uuid.NewString()
	creatorAttemptID := uuid.NewString()
	intent := seedDecidedWorkflowResumeGroup(t, ctx, pool, store, workflowSeed{
		now: now, workspaceID: workspaceID, parentRunID: parentRunID,
		creatorAttemptID: creatorAttemptID,
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim: %v", err)
	}
	if _, _, err := store.CASClaimResumeTx(ctx, tx, ClaimResumeRequest{
		WorkspaceID: workspaceID, GroupID: intent.GroupID,
		GroupCompletionID:         groupCompletionID(t, ctx, pool, workspaceID, intent.GroupID),
		ClaimID:                   uuid.NewString(),
		PreviousAttemptGeneration: 1,
		PreviousAttemptID:         creatorAttemptID,
		ClaimedAt:                 now.Add(time.Second),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("claim resume: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}

	resumer := &observingResumer{}
	coordinator := &WorkflowCoordinator{
		Transactions: pool,
		Store:        store,
		Checkpoints:  missingCheckpointReader{},
		Resumer:      resumer,
		CreatorLeases: staticCreatorLeaseReader{lease: CreatorLeaseState{
			WorkspaceID: workspaceID, RunID: parentRunID,
			AttemptGeneration: 1, AttemptID: creatorAttemptID,
			MarkerPhase: "absent", LeaseExpiresAt: now.Add(time.Second),
		}},
		Now: func() time.Time { return now.Add(2 * time.Second) },
	}

	if err := coordinator.ReconcileGroup(ctx, workspaceID, intent.GroupID); err != nil {
		t.Fatalf("reconcile group: %v", err)
	}
	if resumer.planned != 0 || resumer.resumed != 0 {
		t.Fatalf("resumer calls planned=%d resumed=%d, want 0/0", resumer.planned, resumer.resumed)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM weave_fanout_group
		WHERE workspace_id=$1 AND group_id=$2`, workspaceID, intent.GroupID).Scan(&status); err != nil {
		t.Fatalf("read group status: %v", err)
	}
	if status != string(WorkflowGroupClosed) {
		t.Fatalf("status = %q, want %q", status, WorkflowGroupClosed)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM weave_fanout_audit
		WHERE workspace_id=$1 AND group_id=$2 AND event_type='workflow_resume_closed'`,
		workspaceID, intent.GroupID).Scan(&auditCount); err != nil {
		t.Fatalf("count close audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("close audit count = %d, want 1", auditCount)
	}
}

type workflowSeed struct {
	now              time.Time
	workspaceID      string
	parentRunID      string
	creatorAttemptID string
}

func seedDecidedWorkflowResumeGroup(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	store *Store,
	seed workflowSeed,
) ParkIntent {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	intent, err := store.CreateParkIntentTx(ctx, tx, PrepareParkRequest{
		WorkspaceID:                seed.workspaceID,
		ParentRunID:                seed.parentRunID,
		WorkflowID:                 "workflow-" + uuid.NewString(),
		WorkflowVersion:            1,
		RunSnapshotID:              "snapshot-" + uuid.NewString(),
		NodeID:                     "parallel",
		PreviousCheckpointSequence: 0,
		NodeEntryOrdinal:           0,
		CreatorEpoch:               1,
		CreatorAttemptGeneration:   1,
		CreatorAttemptID:           seed.creatorAttemptID,
		ActivationDeadline:         seed.now.Add(time.Hour),
		ResumeToken:                "resume-token-" + uuid.NewString(),
		JoinPolicy: JoinPolicy{
			Kind: JoinAllSuccess, DeadlineAt: seed.now.Add(time.Hour), MaxDeadlineSeconds: 3600,
		},
		Legs: []PlannedLeg{
			workflowSeedLeg("leg-a", "a", 0),
			workflowSeedLeg("leg-b", "b", 1),
		},
	})
	if err != nil {
		t.Fatalf("create park intent: %v", err)
	}
	if _, err := store.CASActivateIntentTx(ctx, tx, seed.workspaceID, intent.IntentID, intent.Generation, 1, seed.now); err != nil {
		t.Fatalf("activate intent: %v", err)
	}
	for _, legID := range []string{"leg-a", "leg-b"} {
		if _, err := store.RecordLegTerminalTx(ctx, tx, LegCompletionRequest{
			WorkspaceID: seed.workspaceID, GroupID: intent.GroupID, LegID: legID,
			Generation: intent.Generation, Terminal: LegSucceeded,
			Result: json.RawMessage(`{"ok":true}`), CompletedAt: seed.now.Add(time.Second),
		}); err != nil {
			t.Fatalf("complete leg %s: %v", legID, err)
		}
	}
	evaluation, decided, err := store.TryDecideGroupTx(ctx, tx, seed.workspaceID, intent.GroupID, seed.now.Add(2*time.Second), 0)
	if err != nil {
		t.Fatalf("decide group: %v", err)
	}
	if !decided || evaluation.Decision != JoinSucceeded {
		t.Fatalf("decided=%v decision=%s, want succeeded decision", decided, evaluation.Decision)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return intent
}

func workflowSeedLeg(legID, branchID string, ordinal int) PlannedLeg {
	return PlannedLeg{
		LegID:           legID,
		BranchID:        branchID,
		BranchOrdinal:   ordinal,
		FrozenBundleRef: json.RawMessage(`{"bundle":"test"}`),
		InputRef:        json.RawMessage(`{"input":"test"}`),
		MayYieldProof:   json.RawMessage(`{"may_yield":false}`),
	}
}

func groupCompletionID(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID string,
	groupID string,
) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT group_completion_id FROM weave_fanout_group
		WHERE workspace_id=$1 AND group_id=$2`, workspaceID, groupID).Scan(&id); err != nil {
		t.Fatalf("read group completion ID: %v", err)
	}
	return id
}

type missingCheckpointReader struct{}

func (missingCheckpointReader) GetYieldedCheckpoint(context.Context, string, string, int64) (YieldedCheckpoint, error) {
	return YieldedCheckpoint{}, ErrYieldedCheckpointUnavailable
}

type staticCreatorLeaseReader struct {
	lease CreatorLeaseState
}

func (r staticCreatorLeaseReader) CreatorLeaseState(context.Context, CreatorLeaseIdentity) (CreatorLeaseState, error) {
	return r.lease, nil
}

type observingResumer struct {
	planned int
	resumed int
}

func (r *observingResumer) PlanResumeAttempt(context.Context, string, string, int64) (string, error) {
	r.planned++
	return uuid.NewString(), nil
}

func (r *observingResumer) ResumeParkedRun(context.Context, ResumeParkedRunRequest) (ResumeResult, error) {
	r.resumed++
	return ResumeResult{}, errors.New("resume should not be called")
}
