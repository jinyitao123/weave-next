package fanout

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

// advancedResumer stands in for the parent run resumer once another
// coordinator has admitted the claim and moved the parent past its park.
type advancedResumer struct {
	observingResumer
	advanced bool
	reads    int
}

func (r *advancedResumer) AdvancedResume(_ context.Context, req AdvancedResumeRequest) (ResumeResult, bool, error) {
	r.reads++
	if !r.advanced {
		return ResumeResult{}, false, nil
	}
	receipt, _ := json.Marshal(resumeReceiptV1{
		WorkspaceID: req.WorkspaceID, ParentRunID: req.ParentRunID, GroupCompletionID: req.GroupCompletionID,
		ClaimID: req.ClaimID, PreviousAttemptGeneration: req.PreviousAttemptGeneration,
		NewAttemptGeneration: req.NewAttemptGeneration, NewAttemptID: req.NewAttemptID,
		ResumedCheckpointSequence: req.CheckpointSequence, GraphAdvanceEvidenceID: "resume-task",
		AdvancedAt: time.Date(2026, 8, 31, 1, 2, 4, 0, time.UTC),
	})
	return ResumeResult{Status: ResumeAlreadyAdvanced, ClaimID: req.ClaimID, ResumeReceiptID: "fre1_receipt",
		AttemptGeneration: req.NewAttemptGeneration, AttemptID: req.NewAttemptID, ResumeReceipt: receipt}, true, nil
}

// A reconciler that reaches a group another coordinator is resuming finds the
// parent no longer parked. That parent was resumed by this very claim, so the
// group is not closed as if its creator were gone: a durable advance is
// recorded, a live resumed attempt is left to finish, and only an attempt that
// is no longer live leads to the old close.
func TestReconcileGroupFinishesAnAdmittedResumeInsteadOfClosingIt(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	now := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	store := New(pool, testutil.NewFakeClock(now))
	for _, tc := range []struct {
		name       string
		advanced   bool
		leaseUntil time.Duration
		want       WorkflowGroupStatus
	}{
		{name: "advance already durable", advanced: true, leaseUntil: time.Minute, want: WorkflowGroupResumed},
		{name: "resumed attempt still live", advanced: false, leaseUntil: time.Minute, want: WorkflowGroupDecided},
		{name: "resumed attempt gone", advanced: false, leaseUntil: -time.Second, want: WorkflowGroupClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspaceID := "workspace-" + uuid.NewString()
			parentRunID := "run-" + uuid.NewString()
			creatorAttemptID, resumedAttemptID := uuid.NewString(), uuid.NewString()
			intent := seedDecidedWorkflowResumeGroup(t, ctx, pool, store, workflowSeed{
				now: now, workspaceID: workspaceID, parentRunID: parentRunID, creatorAttemptID: creatorAttemptID,
			})
			claimID := uuid.NewString()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.CASClaimResumeTx(ctx, tx, ClaimResumeRequest{
				WorkspaceID: workspaceID, GroupID: intent.GroupID,
				GroupCompletionID: groupCompletionID(t, ctx, pool, workspaceID, intent.GroupID), ClaimID: claimID,
				PreviousAttemptGeneration: 1, PreviousAttemptID: creatorAttemptID, ClaimedAt: now.Add(time.Second),
			}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("claim resume: %v", err)
			}
			if _, _, err := store.CASAdmitResumeTx(ctx, tx, AdmitResumeRequest{
				WorkspaceID: workspaceID, GroupID: intent.GroupID, ClaimID: claimID,
				PreviousAttemptGeneration: 1, PreviousAttemptID: creatorAttemptID,
				NewAttemptGeneration: 2, NewAttemptID: resumedAttemptID,
			}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("admit resume: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			resumer := &advancedResumer{advanced: tc.advanced}
			coordinator := &WorkflowCoordinator{
				Transactions: pool, Store: store, Checkpoints: missingCheckpointReader{}, Resumer: resumer,
				// The parent now runs as the attempt this claim admitted.
				CreatorLeases: staticCreatorLeaseReader{lease: CreatorLeaseState{
					WorkspaceID: workspaceID, RunID: parentRunID, AttemptGeneration: 2, AttemptID: resumedAttemptID,
					MarkerPhase: "absent", LeaseExpiresAt: now.Add(2*time.Second + tc.leaseUntil),
				}},
				Now: func() time.Time { return now.Add(2 * time.Second) },
			}
			if err := coordinator.ReconcileGroup(ctx, workspaceID, intent.GroupID); err != nil {
				t.Fatalf("reconcile group: %v", err)
			}
			if resumer.reads != 1 || resumer.resumed != 0 {
				t.Fatalf("advance reads=%d resumes=%d, want 1/0", resumer.reads, resumer.resumed)
			}
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM weave_fanout_group WHERE workspace_id=$1 AND group_id=$2`,
				workspaceID, intent.GroupID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != string(tc.want) {
				t.Fatalf("group status = %q, want %q", status, tc.want)
			}
		})
	}
}

// A close judged from a plan read before another coordinator claimed and
// admitted the resume must not close the group under that resumer: the lease
// it saw move was moved by the admission itself.
func TestWorkflowResumeCloseLeavesAGroupClaimedSinceThePlanWasRead(t *testing.T) {
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
		now: now, workspaceID: workspaceID, parentRunID: parentRunID, creatorAttemptID: creatorAttemptID,
	})
	coordinator := &WorkflowCoordinator{Transactions: pool, Store: store, Now: func() time.Time { return now.Add(2 * time.Second) }}
	stale, err := coordinator.readResumePlan(ctx, workspaceID, intent.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	claimID := uuid.NewString()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CASClaimResumeTx(ctx, tx, ClaimResumeRequest{
		WorkspaceID: workspaceID, GroupID: intent.GroupID, GroupCompletionID: stale.GroupCompletionID, ClaimID: claimID,
		PreviousAttemptGeneration: 1, PreviousAttemptID: creatorAttemptID, ClaimedAt: now.Add(time.Second),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("claim resume: %v", err)
	}
	if _, _, err := store.CASAdmitResumeTx(ctx, tx, AdmitResumeRequest{
		WorkspaceID: workspaceID, GroupID: intent.GroupID, ClaimID: claimID,
		PreviousAttemptGeneration: 1, PreviousAttemptID: creatorAttemptID,
		NewAttemptGeneration: 2, NewAttemptID: uuid.NewString(),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("admit resume: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.closeWorkflowResumeGroup(ctx, stale, "test_creator_lease_conflict"); err != nil {
		t.Fatalf("close with a stale plan: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM weave_fanout_group WHERE workspace_id=$1 AND group_id=$2`,
		workspaceID, intent.GroupID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(WorkflowGroupDecided) {
		t.Fatalf("group status = %q, want %q", status, WorkflowGroupDecided)
	}
}
