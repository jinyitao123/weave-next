package teamrun

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/fanout"
)

func TestFanoutJoinProjectionUnwrapsLogicalResultAndBindsArtifactSources(t *testing.T) {
	logical := json.RawMessage(`{"revision":1}`)
	physical, err := json.Marshal(fanoutLegExecutionResultV1{
		SchemaVersion: 1, InternalKind: fanoutLegExecutionResultKind,
		Output: logical, ArtifactTaskIDs: []string{"engine-task-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := fanout.JoinProjectionV1{
		SchemaVersion: 1, Decision: fanout.JoinSucceeded,
		Results: map[string]json.RawMessage{"digital": physical}, Errors: map[string]string{},
	}
	join := fanout.JoinResultV1{SchemaVersion: 1, Decision: fanout.JoinSucceeded, Legs: []fanout.JoinResultLegV1{{
		LegID: "leg-digital", BranchID: "digital", DecisionState: fanout.LegDecisionSucceeded, Result: physical,
	}}}
	extended, err := joinProjectionWithLegs(projection, join, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(extended.Results["digital"]) != string(logical) || string(extended.Legs[0].Result) != string(logical) ||
		!reflect.DeepEqual(extended.Legs[0].ArtifactTaskIDs, []string{"engine-task-1"}) {
		t.Fatalf("physical fanout result was not projected correctly: %+v", extended)
	}
}

func TestCorrectionConfirmationPersistsFanoutReplaySnapshot(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "run-selective-correction")
	corrections := &CorrectionStore{Transactions: h.pool, Runs: NewPGStore()}
	h.executor.Corrections = corrections
	requested, err := corrections.Request(context.Background(), RequestCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-selective-correction", TargetKind: "member", TargetMemberID: "digital-worker",
		Instruction: "revise only the digital output", IdempotencyKey: "request-selective", Actor: "user-1", OccurredAt: h.now,
	})
	if err != nil {
		t.Fatalf("request correction: %v", err)
	}
	replay := &CorrectionFanoutReplayV1{
		SchemaVersion: 1, ParallelNodeID: "parallel", JoinNodeID: "join", TargetNodeID: "digital",
		Legs: []CorrectionFanoutReplayLegV1{
			{LegID: "leg-research", NodeID: "research", BranchOrdinal: 0, Result: json.RawMessage(`{"kept":true}`), ArtifactTaskIDs: []string{"task-research"}, Artifacts: []CorrectionFrozenArtifactV1{}},
			{LegID: "leg-digital", NodeID: "digital", BranchOrdinal: 1, Result: json.RawMessage(`{"revision":1}`), ArtifactTaskIDs: []string{"task-digital"}, Artifacts: []CorrectionFrozenArtifactV1{}},
		},
	}
	replay.SourceProjectionHash, err = correctionFanoutSourceProjectionHash(*replay)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := json.Marshal(CorrectionWaitDetailV1{
		SchemaVersion: 1, WaitType: "correction", CorrectionID: requested.CorrectionID,
		TargetKind: "member", TargetMemberID: "digital-worker", Instruction: requested.Instruction,
		SafeNodeID: "join", RestartNodeID: "digital", AffectedNodeIDs: []string{"digital", "join", "deliver"},
		PreservedNodeIDs: []string{"lead", "research"}, FanoutReplay: replay,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.runtime.executeResult = RuntimeResult{Status: RuntimeParked, Park: &RuntimePark{
		NodeID: "join", WaitKind: WaitCorrection, WaitDetail: detail,
		CompletedOutputs: map[string]json.RawMessage{"lead": json.RawMessage(`{"baseline":true}`), "join": json.RawMessage(`{"old":true}`)},
		ArtifactTaskIDs:  map[string][]string{"join": {"task-research", "task-digital"}}, UsageComplete: true,
	}}
	if processed, err := h.executor.ProcessNext(context.Background(), "worker-1"); err != nil || !processed {
		t.Fatalf("park correction: processed=%v err=%v", processed, err)
	}
	service := &CorrectionResumeService{Transactions: h.pool, Runs: NewPGStore(), Corrections: corrections,
		Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks, Now: func() time.Time { return h.now.Add(time.Second) }}
	if _, err := service.Confirm(context.Background(), ConfirmCorrectionRequest{
		WorkspaceID: "workspace-1", RunID: "run-selective-correction", CorrectionID: requested.CorrectionID,
		Disposition: "apply", IdempotencyKey: "confirm-selective", Actor: "user-1", OccurredAt: h.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("confirm correction: %v", err)
	}
	tx := h.mustBeginTx(t)
	checkpoint, err := NewPGCheckpointStore().GetTx(context.Background(), tx, "workspace-1", "run-selective-correction")
	_ = tx.Rollback(context.Background())
	if err != nil {
		t.Fatalf("read corrected checkpoint: %v", err)
	}
	if checkpoint.NodeID != "digital" || checkpoint.CompletedOutputs["join"] != nil || checkpoint.CompletedOutputs["lead"] == nil ||
		len(checkpoint.Corrections) != 1 || !reflect.DeepEqual(checkpoint.Corrections[0].FanoutReplay, replay) {
		t.Fatalf("fanout replay snapshot was not durably preserved: %+v", checkpoint)
	}
}

func TestMemberCorrectionFreezesAndReplaysOnlyTargetFanoutLeg(t *testing.T) {
	projection := fanoutJoinProjectionV1{
		SchemaVersion: 1, Decision: "succeeded",
		Results: map[string]json.RawMessage{"research": json.RawMessage(`{"kept":true}`), "digital": json.RawMessage(`{"revision":1}`)},
		Errors:  map[string]string{},
		Legs: []fanoutJoinLegV1{
			{NodeID: "research", LegID: "leg-research", BranchOrdinal: 0, DecisionDisposition: "succeeded", Result: json.RawMessage(`{"kept":true}`), Error: json.RawMessage(`null`), ArtifactTaskIDs: []string{"task-research"}},
			{NodeID: "digital", LegID: "leg-digital", BranchOrdinal: 1, DecisionDisposition: "succeeded", Result: json.RawMessage(`{"revision":1}`), Error: json.RawMessage(`null`), ArtifactTaskIDs: []string{"task-digital"}},
		},
	}
	replay, ok := buildFanoutCorrectionReplaySnapshot(
		"parallel", "join", "digital", map[string]bool{"research": true, "digital": true}, projection,
	)
	if !ok || replay.TargetNodeID != "digital" || replay.JoinNodeID != "join" {
		t.Fatalf("selective replay snapshot not planned: %+v", replay)
	}

	loader := func(_ context.Context, ids []string) ([]deliverable.WorkflowArtifact, error) {
		switch {
		case reflect.DeepEqual(ids, []string{"task-research"}):
			return []deliverable.WorkflowArtifact{{Path: "research.md", ContentType: "text/markdown", Content: "kept evidence"}}, nil
		case reflect.DeepEqual(ids, []string{"task-digital"}):
			return []deliverable.WorkflowArtifact{{Path: "app/index.html", ContentType: "text/html", Content: "<main>v1</main>"}}, nil
		default:
			t.Fatalf("loaded artifact ids = %v", ids)
			return nil, nil
		}
	}
	if err := freezeCorrectionFanoutArtifacts(context.Background(), &replay, loader); err != nil {
		t.Fatalf("freeze target artifacts: %v", err)
	}
	if err := validateCorrectionFanoutReplay(replay, "member", "digital-worker", "digital"); err != nil {
		t.Fatalf("validate frozen replay: %v", err)
	}
	artifacts, err := verifiedCorrectionTargetArtifacts(context.Background(), replay, loader)
	if err != nil {
		t.Fatalf("verify frozen correction artifacts: %v", err)
	}
	contextJSON, err := correctionFrozenPromptContext(replay, replay.Legs[1], artifacts)
	if err != nil {
		t.Fatalf("build frozen correction context: %v", err)
	}
	var frozenContext struct {
		PriorResult json.RawMessage `json:"prior_result"`
		Artifacts   []struct {
			Content string `json:"content"`
		} `json:"artifacts"`
	}
	if json.Unmarshal([]byte(contextJSON), &frozenContext) != nil || string(frozenContext.PriorResult) != `{"revision":1}` ||
		len(frozenContext.Artifacts) != 1 || frozenContext.Artifacts[0].Content != "<main>v1</main>" {
		t.Fatalf("frozen correction context omitted prior inputs: %s", contextJSON)
	}

	merged, artifactIDs, err := mergeFanoutCorrectionReplayResult(replay, map[string]any{"revision": 2}, []string{"member:new-digital"})
	if err != nil {
		t.Fatalf("merge selective correction: %v", err)
	}
	if string(merged.Results["research"]) != `{"kept":true}` || string(merged.Results["digital"]) != `{"revision":2}` ||
		!reflect.DeepEqual(artifactIDs, []string{"task-research", "member:new-digital"}) {
		t.Fatalf("selective merge changed preserved results: merged=%+v artifacts=%v", merged, artifactIDs)
	}

	driftLoader := func(_ context.Context, ids []string) ([]deliverable.WorkflowArtifact, error) {
		if reflect.DeepEqual(ids, []string{"task-research"}) {
			return []deliverable.WorkflowArtifact{{Path: "research.md", ContentType: "text/markdown", Content: "changed evidence"}}, nil
		}
		return []deliverable.WorkflowArtifact{{Path: "app/index.html", ContentType: "text/html", Content: "<main>v1</main>"}}, nil
	}
	if _, err := verifiedCorrectionTargetArtifacts(context.Background(), replay, driftLoader); err == nil || !strings.Contains(err.Error(), "manifest changed") {
		t.Fatalf("artifact drift was not rejected: %v", err)
	}
}

func TestMemberCorrectionFallsBackForLegacyFanoutProjection(t *testing.T) {
	legacy := fanoutJoinProjectionV1{SchemaVersion: 1, Decision: "succeeded", Results: map[string]json.RawMessage{}, Errors: map[string]string{}}
	if replay, ok := buildFanoutCorrectionReplaySnapshot(
		"parallel", "join", "right", map[string]bool{"left": true, "right": true}, legacy,
	); ok {
		t.Fatalf("legacy projection was unsafely reused: %+v", replay)
	}
}
