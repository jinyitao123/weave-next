package teambuild

import (
	"context"
	"errors"
	"testing"
)

func TestEvaluatedRoundCommandIsAtomicRealPG(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run := createAuthorizeTestRun(t, ctx, store, "evaluated-round-command", ModeCreate)
	report := EvaluationReport{SchemaVersion: 1, RoundNo: 1, Conclusion: ConclusionPass}
	hash, err := report.Hash()
	if err != nil {
		t.Fatal(err)
	}
	result := RoundResult{CandidateRef: "candidate-1", ReportRef: hash, Conclusion: ConclusionPass}
	// Saving the report succeeds, but the round is inadmissible before authorization.
	if _, err := store.RecordEvaluatedRound(ctx, run.WorkspaceID, run.BuildRunID, report, result); !errors.Is(err, ErrBuildRunNotRoundRunning) {
		t.Fatalf("unstarted round error = %v", err)
	}
	if _, err := store.GetRoundReport(ctx, run.WorkspaceID, run.BuildRunID, 1); !errors.Is(err, ErrRoundReportNotFound) {
		t.Fatalf("failed atomic command retained its report: %v", err)
	}
	run = authorizeBudgetTestRun(t, ctx, store, run)
	if _, err := store.TransitionStatus(ctx, run.WorkspaceID, run.BuildRunID, StatusAuthorized, StatusRoundRunning, "builder", "start evaluated round"); err != nil {
		t.Fatal(err)
	}
	round, err := store.RecordEvaluatedRound(ctx, run.WorkspaceID, run.BuildRunID, report, result)
	if err != nil || round.ReportRef != hash || round.RoundNo != 1 {
		t.Fatalf("committed round = %#v err = %v", round, err)
	}
	saved, err := store.GetRoundReport(ctx, run.WorkspaceID, run.BuildRunID, round.RoundNo)
	if err != nil || saved.ReportHash != round.ReportRef {
		t.Fatalf("round report reference is not resolvable: %#v err = %v", saved, err)
	}
}
