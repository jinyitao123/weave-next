package loomruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/storeext"
)

func replayInputs(t *testing.T, h *memberPGHarness) (string, []MemberJournalEntry, json.RawMessage) {
	t.Helper()
	runID := MemberRunID(h.request.WorkspaceID, h.request.ParentRunID, h.request.RunSnapshotID, h.request.NodeID, h.request.CallID)
	entries, err := ReadMemberJournal(t.Context(), h.pool, h.request.WorkspaceID, runID)
	if err != nil {
		t.Fatal(err)
	}
	var initial []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT initial_state FROM weave_workflow_member_runs WHERE workspace_id=$1 AND member_run_id=$2`,
		h.request.WorkspaceID, runID).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	return runID, entries, initial
}

func runMemberToCompletion(t *testing.T, h *memberPGHarness) {
	t.Helper()
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), h.request); err != nil {
		t.Fatal(err)
	}
}

func TestMemberReplayReproducesACompletedRunWithoutLiveCallsRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runMemberToCompletion(t, h)
	models, tools := h.model.calls.Load(), h.tools.calls.Load()
	runID, entries, initial := replayInputs(t, h)
	if len(entries) != 4 {
		t.Fatalf("recorded %d operations, want a model call, two tool calls and a model call", len(entries))
	}
	report := ReplayMemberSegment(t.Context(), runID, entries, initial)
	if report.Outcome != ReplayCompleted || report.Output != "done" || report.Replayed != 4 || len(report.HistoryProblems) != 0 {
		t.Fatalf("replay = %+v", report)
	}
	if h.model.calls.Load() != models || h.tools.calls.Load() != tools {
		t.Fatal("a replay made a live model or tool call")
	}
}

func TestMemberReplayShowsWhereAnInterruptedRunStoppedRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runner, _ := NewMemberRunner(h.records)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("expected interruption")
	}
	runID, entries, initial := replayInputs(t, h)
	report := ReplayMemberSegment(t.Context(), runID, entries, initial)
	if report.Outcome != ReplayJournalEnded || report.Replayed != 2 {
		t.Fatalf("replay of an interrupted run = %+v, want it to stop after the model call and the first tool", report)
	}
}

func TestMemberReplayReportsAToolWhoseOutcomeIsUnknownRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.tools.failAfterEffect = true
	runner, _ := NewMemberRunner(h.records)
	if _, err := runner.Run(t.Context(), h.request); err == nil {
		t.Fatal("expected the tool failure to stop the run")
	}
	runID, entries, initial := replayInputs(t, h)
	report := ReplayMemberSegment(t.Context(), runID, entries, initial)
	if report.Outcome != ReplayOutcomeUnknown {
		t.Fatalf("replay = %+v, want outcome_unknown", report)
	}
}

func TestMemberReplayDetectsADifferentInputAndABrokenHistoryRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runMemberToCompletion(t, h)
	runID, entries, initial := replayInputs(t, h)

	changed := strings.Replace(string(initial), "Write two entries then finish.", "Something else.", 1)
	if changed == string(initial) {
		t.Fatalf("initial state did not contain the task: %s", initial)
	}
	report := ReplayMemberSegment(t.Context(), runID, entries, json.RawMessage(changed))
	if report.Outcome != ReplayDiverged || !strings.Contains(report.Detail, "operation 1") || !strings.Contains(report.Detail, "message") {
		t.Fatalf("a changed task was not reported as a divergence at the first request: %+v", report)
	}

	tampered := append([]MemberJournalEntry(nil), entries...)
	// A recorded tool result that names a call nobody made leaves the next
	// request with an unanswered call and an orphan result.
	var result map[string]any
	if err := json.Unmarshal(tampered[2].Response, &result); err != nil {
		t.Fatal(err)
	}
	result["call_id"] = "nobody-asked"
	tampered[2].Response, _ = json.Marshal(result)
	report = ReplayMemberSegment(t.Context(), runID, tampered, initial)
	if len(report.HistoryProblems) == 0 && !(report.Outcome == ReplayLoopError && strings.Contains(report.Detail, "unmatched tool result")) {
		t.Fatalf("an orphan tool result went unnoticed: %+v", report)
	}
}

func TestReadMemberJournalIgnoresOtherMembersAndWorkspacesRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runMemberToCompletion(t, h)
	other := storeext.New(h.pool)
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO loom_store(namespace,key,value) VALUES('member-operation:workspace','other-member/000000000001/chat/000000000001','{}')`); err != nil {
		t.Fatal(err)
	}
	_ = other
	runID, entries, _ := replayInputs(t, h)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Segment, "other") {
			t.Fatalf("read another member's entry for %s", runID)
		}
	}
	if none, err := ReadMemberJournal(t.Context(), h.pool, "another-workspace", runID); err != nil || len(none) != 0 {
		t.Fatalf("read %d entries through another workspace (err=%v)", len(none), err)
	}
}
