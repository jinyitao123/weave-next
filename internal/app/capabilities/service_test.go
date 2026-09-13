package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/capability"
)

func TestServicePublishesAndReplaysInvocation(t *testing.T) {
	store := NewMemoryStore()
	service := NewService(store, store)
	d := capability.Definition{
		SchemaVersion: 1, CapabilityID: "cap-1", Name: "Review",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Roles: []capability.Role{{ID: "r", Name: "Reviewer"}},
		Steps: []capability.Step{{ID: "s", Name: "Review", RoleID: "r", Kind: capability.StepWorker, Instruction: "review"}},
	}
	if err := service.SaveDraft(context.Background(), DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(context.Background(), "ws", "cap-1", 1); err != nil {
		t.Fatal(err)
	}
	req := InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", InvocationID: "inv-1", RequestID: "req-1", CapabilityID: "cap-1", Revision: 1, Input: json.RawMessage(`{"x":1}`)}
	first, replayed, err := service.Invoke(context.Background(), req)
	if err != nil || replayed || first.Status != "queued" || first.ResultState != "unavailable" || first.TaskID == "" {
		t.Fatalf("first=%+v replayed=%v err=%v", first, replayed, err)
	}
	second, replayed, err := service.Invoke(context.Background(), req)
	if err != nil || !replayed || second.InvocationID != first.InvocationID {
		t.Fatalf("second=%+v replayed=%v err=%v", second, replayed, err)
	}
}

func TestServiceRejectsChangedIdempotentRequest(t *testing.T) {
	store := NewMemoryStore()
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap-1", Name: "Review", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "Reviewer"}}, Steps: []capability.Step{{ID: "s", Name: "Review", RoleID: "r", Kind: capability.StepWorker, Instruction: "review"}}}
	_ = service.SaveDraft(context.Background(), DraftRequest{WorkspaceID: "ws", Definition: d})
	_, _ = service.Publish(context.Background(), "ws", "cap-1", 1)
	base := InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", InvocationID: "inv-1", RequestID: "req-1", CapabilityID: "cap-1", Revision: 1, Input: json.RawMessage(`{"x":1}`)}
	_, _, _ = service.Invoke(context.Background(), base)
	base.Input = json.RawMessage(`{"x":2}`)
	if _, _, err := service.Invoke(context.Background(), base); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestServiceCancelsQueuedInvocation(t *testing.T) {
	store := NewMemoryStore()
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap-1", Name: "Review", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "Reviewer"}}, Steps: []capability.Step{{ID: "s", Name: "Review", RoleID: "r", Kind: capability.StepWorker, Instruction: "review"}}}
	if err := service.SaveDraft(context.Background(), DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(context.Background(), "ws", "cap-1", 1); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := service.Invoke(context.Background(), InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", InvocationID: "inv-1", RequestID: "req-1", CapabilityID: "cap-1", Revision: 1, Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := service.CancelInvocation(context.Background(), "ws", "app", invocation.InvocationID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	if again, err := service.CancelInvocation(context.Background(), "ws", "app", invocation.InvocationID); err != nil || again.Status != "cancelled" {
		t.Fatalf("expected idempotent cancellation, got %v", err)
	}
}

type fixtureExecutor struct{}

func (fixtureExecutor) Execute(context.Context, InvocationTask) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

func TestRunOneCompletesClaimedTask(t *testing.T) {
	store := NewMemoryStore()
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap-1", Name: "Review", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "Reviewer"}}, Steps: []capability.Step{{ID: "s", Name: "Review", RoleID: "r", Kind: capability.StepWorker, Instruction: "review"}}}
	_ = service.SaveDraft(context.Background(), DraftRequest{WorkspaceID: "ws", Definition: d})
	_, _ = service.Publish(context.Background(), "ws", "cap-1", 1)
	invocation, _, _ := service.Invoke(context.Background(), InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", InvocationID: "inv-1", RequestID: "req-1", CapabilityID: "cap-1", Revision: 1, Input: json.RawMessage(`{"x":1}`)})
	if invocation.TaskID == "" {
		t.Fatal("expected queued task")
	}
	processed, err := RunOne(context.Background(), store, fixtureExecutor{})
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	completed, err := service.GetInvocation(context.Background(), "ws", "app", invocation.InvocationID)
	if err != nil || completed.Status != "completed" || completed.ResultState != "available" {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
}
