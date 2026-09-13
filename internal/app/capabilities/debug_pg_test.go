package capabilities

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

func TestDebugSnapshotDoesNotPublishOrFollowDraftEditsRealPG(t *testing.T) {
	pool, store, production := executionFixture(t)
	service := NewService(store, store)
	d, err := store.GetDraft(t.Context(), "ws", "cap")
	if err != nil {
		t.Fatal(err)
	}
	d.Steps[0].Instruction = "debug original"
	request := DebugRequest{WorkspaceID: "ws", ApplicationID: "developer", RequestID: "debug-request", Definition: d, Input: json.RawMessage(`{"x":2}`)}
	debug, replayed, err := service.Debug(t.Context(), request)
	if err != nil || replayed || debug.RunKind != "debug" || debug.Revision != 0 {
		t.Fatalf("debug=%+v %v %v", debug, replayed, err)
	}
	d.Steps[0].Instruction = "edited after submit"
	if err := service.SaveDraft(t.Context(), DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	// Consume the older production task first; it must retain the original published definition.
	task, ok, err := store.ClaimTask(t.Context())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if task.InvocationID != production.InvocationID || task.Plan.Steps[0].Instruction != "run" {
		t.Fatalf("production changed: %+v", task)
	}
	if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	task, ok, err = store.ClaimTask(t.Context())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if task.InvocationID != debug.InvocationID || task.RunKind != "debug" || task.Plan.Revision != 0 || task.Plan.Steps[0].Instruction != "debug original" {
		t.Fatalf("debug followed mutable draft: %+v", task)
	}
	if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{"checked":true}`), nil); err != nil {
		t.Fatal(err)
	}
	var versions, snapshots int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_revisions WHERE workspace_id='ws'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_debug_snapshots WHERE workspace_id='ws'`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || snapshots != 1 {
		t.Fatalf("debug published a version: versions=%d snapshots=%d", versions, snapshots)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_debug_snapshots SET definition='{}' WHERE workspace_id='ws'`); err == nil {
		t.Fatal("snapshot rewrite accepted")
	}
}

func TestDebugReplayAndOwnershipAcrossConnectionsRealPG(t *testing.T) {
	pool, store, _ := executionFixture(t)
	d, err := store.GetDraft(t.Context(), "ws", "cap")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, store)
	request := DebugRequest{WorkspaceID: "ws", ApplicationID: "developer", RequestID: "debug", Definition: d, Input: json.RawMessage(`{"x":1}`)}
	first, _, err := service.Debug(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	separate, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer separate.Close()
	restarted := NewPGStore(separate)
	request.Input = json.RawMessage(`{ "x":1.0 }`)
	replay, replayed, err := NewService(restarted, restarted).Debug(t.Context(), request)
	if err != nil || !replayed || replay.InvocationID != first.InvocationID {
		t.Fatalf("replay=%+v %v %v", replay, replayed, err)
	}
	request.Definition.Steps[0].Instruction = "changed"
	if _, _, err := service.Debug(t.Context(), request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed snapshot reused key: %v", err)
	}
	if _, err := restarted.GetInvocation(t.Context(), "ws", "app", first.InvocationID); !errors.Is(err, ErrInvocationNotFound) {
		t.Fatalf("other app read debug: %v", err)
	}
	if _, err := restarted.CancelInvocation(t.Context(), "ws", "developer", first.InvocationID); err != nil {
		t.Fatal(err)
	}
}

func TestDebugWithoutSavedDraftOrPublishedRevisionRealPG(t *testing.T) {
	pool, store, _ := executionFixture(t)
	d, err := store.GetDraft(t.Context(), "ws", "cap")
	if err != nil {
		t.Fatal(err)
	}
	d.CapabilityID = "unsaved"
	invocation, _, err := NewService(store, store).Debug(t.Context(), DebugRequest{WorkspaceID: "ws", ApplicationID: "developer", RequestID: "unsaved", Definition: d, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_capability_definitions WHERE capability_id='unsaved')+(SELECT count(*) FROM weave_capability_revisions WHERE capability_id='unsaved')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("debug changed authoring records: count=%d err=%v", count, err)
	}
	if invocation.DefinitionHash == "" || invocation.RunKind != "debug" {
		t.Fatalf("missing debug identity: %+v", invocation)
	}
}
