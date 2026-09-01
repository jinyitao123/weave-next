package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCorrectionSafePointConfirmAndResume(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "run-correction")
	corrections := &CorrectionStore{Transactions: h.pool, Runs: NewPGStore()}
	h.executor.Corrections = corrections

	requested, err := corrections.Request(context.Background(), RequestCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-correction", TargetKind: "member",
		TargetMemberID: "reviewer", Instruction: "recheck the evidence boundary",
		IdempotencyKey: "request-correction-once", Actor: "user-1", OccurredAt: h.now,
	})
	if err != nil {
		t.Fatalf("request correction: %v", err)
	}
	detail, err := json.Marshal(CorrectionWaitDetailV1{
		SchemaVersion: 1, WaitType: "correction", CorrectionID: requested.CorrectionID,
		TargetKind: "member", TargetMemberID: "reviewer", Instruction: requested.Instruction,
		SafeNodeID: "deliver", RestartNodeID: "review",
		AffectedNodeIDs: []string{"review", "deliver"}, PreservedNodeIDs: []string{"research"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.runtime.executeResult = RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
		NodeID: "deliver", WaitKind: WaitCorrection, WaitDetail: detail,
		CompletedOutputs: map[string]json.RawMessage{
			"research": json.RawMessage(`{"draft":true}`),
			"review":   json.RawMessage(`{"accepted":false}`),
		}, UsageComplete: true,
	}}
	processed, err := h.executor.ProcessNext(context.Background(), "worker-1")
	if err != nil || !processed {
		t.Fatalf("reach correction safe point: processed=%v err=%v", processed, err)
	}
	tx := h.mustBeginTx(t)
	parked, err := NewPGStore().GetTx(context.Background(), tx, "workspace-1", "run-correction")
	if err != nil {
		t.Fatalf("read parked run: %v", err)
	}
	_ = tx.Rollback(context.Background())
	if parked.Status != StatusParked || parked.WaitKind == nil || *parked.WaitKind != WaitCorrection {
		t.Fatalf("run did not park for correction: %#v", parked)
	}
	active, present, err := corrections.GetActive(context.Background(), "workspace-1", "run-correction")
	if err != nil || !present || active.Status != CorrectionReady {
		t.Fatalf("ready correction: present=%v item=%#v err=%v", present, active, err)
	}

	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"revised":true}`), UsageComplete: true}
	service := &CorrectionResumeService{Transactions: h.pool, Runs: NewPGStore(), Corrections: corrections,
		Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(2 * time.Second) }}
	confirmed, err := service.Confirm(context.Background(), ConfirmCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-correction", CorrectionID: requested.CorrectionID,
		Disposition: "apply", IdempotencyKey: "confirm-correction-once", Actor: "user-1", OccurredAt: h.now.Add(2 * time.Second),
	})
	if err != nil || confirmed.Idempotent {
		t.Fatalf("confirm correction: result=%#v err=%v", confirmed, err)
	}
	tx = h.mustBeginTx(t)
	checkpoint, err := NewPGCheckpointStore().GetTx(context.Background(), tx, "workspace-1", "run-correction")
	if err != nil {
		t.Fatalf("read corrected checkpoint: %v", err)
	}
	_ = tx.Rollback(context.Background())
	if checkpoint.NodeID != "review" || checkpoint.CompletedOutputs["review"] != nil || checkpoint.CompletedOutputs["research"] == nil ||
		len(checkpoint.Corrections) != 1 || checkpoint.Corrections[0].Instruction != requested.Instruction {
		t.Fatalf("correction was not applied to checkpoint: %#v", checkpoint)
	}

	h.executor.Now = func() time.Time { return h.now.Add(3 * time.Second) }
	processed, err = h.executor.ProcessNext(context.Background(), "worker-2")
	if err != nil || !processed {
		t.Fatalf("resume correction: processed=%v err=%v", processed, err)
	}
	h.assertRun(t, "run-correction", StatusSucceeded, nil)
	active, present, err = corrections.GetActive(context.Background(), "workspace-1", "run-correction")
	if err != nil || present {
		t.Fatalf("applied correction remained active: present=%v item=%#v err=%v", present, active, err)
	}
	replayed, err := service.Confirm(context.Background(), ConfirmCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-correction", CorrectionID: requested.CorrectionID,
		Disposition: "apply", IdempotencyKey: "confirm-correction-once", Actor: "user-1", OccurredAt: h.now.Add(3 * time.Second),
	})
	if err != nil || !replayed.Idempotent {
		t.Fatalf("idempotent confirmation: result=%#v err=%v", replayed, err)
	}
}

func TestCorrectionCanBeRequestedWhileExternalMemberIsRunning(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "run-fanout-correction")
	if _, err := h.pool.Exec(context.Background(), `UPDATE weave_team_runs
		SET status='parked', wait_kind='fanout'
		WHERE workspace_id='workspace-1' AND run_id='run-fanout-correction'`); err != nil {
		t.Fatalf("park run for fanout: %v", err)
	}
	corrections := &CorrectionStore{Transactions: h.pool, Runs: NewPGStore()}
	requested, err := corrections.Request(context.Background(), RequestCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-fanout-correction", TargetKind: "team",
		Instruction:    "preserve completed evidence and revise the active branch",
		IdempotencyKey: "request-during-fanout", Actor: "user-1", OccurredAt: h.now,
	})
	if err != nil {
		t.Fatalf("request correction during fanout: %v", err)
	}
	if requested.Status != CorrectionRequested {
		t.Fatalf("correction status = %q, want %q", requested.Status, CorrectionRequested)
	}
}

func TestCorrectionAndActivityLedgersAreWorkspaceIsolated(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "run-isolated")
	ctx := context.Background()
	corrections := &CorrectionStore{Transactions: h.pool, Runs: NewPGStore()}
	requested, err := corrections.Request(ctx, RequestCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-isolated", TargetKind: "team",
		Instruction: "recheck the evidence", IdempotencyKey: "isolated-correction",
		Actor: "user-1", OccurredAt: h.now,
	})
	if err != nil {
		t.Fatalf("request correction: %v", err)
	}
	visible, err := corrections.List(ctx, "workspace-1", "run-isolated", 20)
	if err != nil || len(visible) != 1 || visible[0].CorrectionID != requested.CorrectionID {
		t.Fatalf("owner correction list = %#v, err=%v", visible, err)
	}
	hidden, err := corrections.List(ctx, "workspace-2", "run-isolated", 20)
	if err != nil || len(hidden) != 0 {
		t.Fatalf("foreign correction list = %#v, err=%v", hidden, err)
	}
	_, err = corrections.Request(ctx, RequestCorrectionRequest{
		WorkspaceID: "workspace-2", RunID: "run-isolated", TargetKind: "team",
		Instruction: "foreign request", IdempotencyKey: "foreign-correction",
		Actor: "user-2", OccurredAt: h.now,
	})
	if !errors.Is(err, ErrTeamRunIdentityMismatch) {
		t.Fatalf("foreign correction request error = %v", err)
	}

	activities := &PGActivityStore{Transactions: h.pool}
	err = activities.Record(ctx, ActivityEvent{
		WorkspaceID: "workspace-1", RunID: "run-isolated", EventID: "isolated-activity",
		Kind: "member_started", MemberID: "reviewer", Detail: json.RawMessage(`{"input":"evidence"}`),
		OccurredAt: h.now,
	})
	if err != nil {
		t.Fatalf("record activity: %v", err)
	}
	ownerEvents, err := activities.List(ctx, "workspace-1", "run-isolated", 20)
	if err != nil || len(ownerEvents) != 1 || ownerEvents[0].EventID != "isolated-activity" {
		t.Fatalf("owner activity list = %#v, err=%v", ownerEvents, err)
	}
	foreignEvents, err := activities.List(ctx, "workspace-2", "run-isolated", 20)
	if err != nil || len(foreignEvents) != 0 {
		t.Fatalf("foreign activity list = %#v, err=%v", foreignEvents, err)
	}
}
