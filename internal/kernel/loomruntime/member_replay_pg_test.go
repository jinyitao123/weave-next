package loomruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
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
	report := replayMemberFixture(t, h, runID, entries, initial, 0)
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
	report := replayMemberFixture(t, h, runID, entries, initial, 0)
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
	report := replayMemberFixture(t, h, runID, entries, initial, 0)
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
	report := replayMemberFixture(t, h, runID, entries, json.RawMessage(changed), 0)
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
	report = replayMemberFixture(t, h, runID, tampered, initial, 0)
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

func replayFixtureConfiguration(total uint64) MemberReplayConfiguration {
	maxIterations := 3
	if total > 0 {
		maxIterations = 1
	}
	raw, _ := json.Marshal(map[string]any{"model": "fixture", "max_iterations": maxIterations, "controlled_total": total, "graph": "workspace:member"})
	digest, _ := frozen.HashCanonicalJSON(raw)
	return MemberReplayConfiguration{Digest: digest, compile: func(_ context.Context, j *replayJournal, defs []contract.ToolDef, before loom.StepHook) (*loom.Graph, error) {
		g := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
		g.SetHooks(loom.HookPoints{Before: []loom.StepHook{before}})
		opts := stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}
		if total > 0 {
			opts.MaxIterations = 1
			opts.Control = &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: total}
		}
		g.AddStep("chat", stdlib.NewToolLoopStep(stdlib.NewJournaledLLM(replayLiveLLM{}, j), stdlib.NewJournaledToolDispatcher(replayTools{defs: defs}, j, stdlib.JournaledToolOpts{SerializeWhenActive: true}), opts), loom.End())
		return g, nil
	}}
}

func replayMemberFixture(t *testing.T, h *memberPGHarness, id string, entries []MemberJournalEntry, initial json.RawMessage, total uint64) *MemberReplayReport {
	t.Helper()
	checkpoints, err := ReadMemberReplayCheckpoints(t.Context(), h.pool, h.request.WorkspaceID, id)
	if err != nil {
		t.Fatal(err)
	}
	// Initial-state divergence also changes the step-entry transcript. A replay
	// must compare that regenerated request rather than trust the recorded input.
	if len(checkpoints) > 0 {
		var source loom.State
		if json.Unmarshal(initial, &source) == nil {
			var entry loom.State
			_ = json.Unmarshal(checkpoints[0].State, &entry)
			entry["messages"] = source["messages"]
			checkpoints[0].State, _ = json.Marshal(entry)
		}
	}
	return ReplayMemberJournal(t.Context(), id, entries, checkpoints, replayFixtureConfiguration(total))
}

func TestMemberReplayAllControlledSegmentsUsesEntrySnapshotsRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.request.Graph = memberControlledGraph(h, 2)
	runner, _ := NewMemberRunner(h.records)
	paused, err := runner.Run(t.Context(), h.request)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := ReadMemberBudgetPause(paused, h.request.Graph.Name)
	if err != nil || pause == nil {
		t.Fatal("controlled run did not pause")
	}
	next := h.nextEpoch(t)
	next.Graph = memberControlledGraph(h, 2)
	tx, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := next.ParentGuard(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeMemberBudgetTx(t.Context(), tx, next.WorkspaceID, next.ParentRunID, next.CallID, "replay-grant", *pause, 2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	next.ResumeGrantID = "replay-grant"
	if _, err := runner.Run(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	id, entries, _ := replayInputs(t, h)
	checkpoints, err := ReadMemberReplayCheckpoints(t.Context(), h.pool, h.request.WorkspaceID, id)
	if err != nil {
		t.Fatal(err)
	}
	beforeModels, beforeTools := h.model.calls.Load(), h.tools.calls.Load()
	var before int64
	_ = h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store`).Scan(&before)
	report := ReplayMemberJournal(t.Context(), id, entries, checkpoints, replayFixtureConfiguration(2))
	if report.Outcome != ReplayCompleted || report.VerifiedSegments != 2 || len(report.Segments) != 2 || report.Replayed != len(entries) || !report.ConfigVerified {
		t.Fatalf("full segment replay: %+v", report)
	}
	if h.model.calls.Load() != beforeModels || h.tools.calls.Load() != beforeTools {
		t.Fatal("replay called live implementations")
	}
	var after int64
	_ = h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store`).Scan(&after)
	if before != after {
		t.Fatal("replay wrote to PostgreSQL")
	}
	// Unresolved effects in a later slice remain unknown; matching the first
	// slice cannot promote them into a successful whole-member replay.
	pending := append([]MemberJournalEntry(nil), entries...)
	for index := range pending {
		if pending[index].Segment == report.Segments[1] {
			pending[index].Response = nil
			break
		}
	}
	lost := ReplayMemberJournal(t.Context(), id, pending, checkpoints, replayFixtureConfiguration(2))
	if lost.Outcome != ReplayModelLost || lost.ConfigVerified {
		t.Fatalf("lost later model receipt was promoted: %+v", lost)
	}

	// Missing second entry is evidence loss, not a successful first-segment replay.
	second := report.Segments[1]
	kept := []MemberReplayCheckpoint{}
	for _, cp := range checkpoints {
		if cp.Segment != second {
			kept = append(kept, cp)
		}
	}
	incomplete := ReplayMemberJournal(t.Context(), id, entries, kept, replayFixtureConfiguration(2))
	if incomplete.Outcome != ReplayConfigurationUnavailable || incomplete.ConfigVerified {
		t.Fatalf("missing checkpoint was promoted: %+v", incomplete)
	}
	// A changed second-slice transcript must diverge even when the first matches.
	changed := append([]MemberReplayCheckpoint(nil), checkpoints...)
	for i := range changed {
		if changed[i].Segment == second {
			var state loom.State
			_ = json.Unmarshal(changed[i].State, &state)
			state["__toolloop_msgs"] = []contract.Message{{Role: "user", Content: "altered task"}}
			changed[i].State, _ = json.Marshal(state)
			break
		}
	}
	divergent := ReplayMemberJournal(t.Context(), id, entries, changed, replayFixtureConfiguration(2))
	if divergent.Outcome != ReplayDiverged && divergent.Outcome != ReplayLoopError {
		t.Fatalf("changed later segment was accepted: %+v", divergent)
	}
}

func TestMemberReplayRefusesCorruptInputHash(t *testing.T) {
	entries := []MemberJournalEntry{{Segment: "000000000001/chat", Cursor: 1, Kind: "model", Input: json.RawMessage(`{}`), InputHash: strings.Repeat("0", 64)}}
	report := ReplayMemberJournal(t.Context(), "private-run", entries, nil, replayFixtureConfiguration(0))
	if report.Outcome != ReplayDiverged || report.ConfigVerified {
		t.Fatal("tampered request digest was ignored")
	}
}
