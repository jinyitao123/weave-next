package capabilities

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

func TestPGStorePersistsRevisionAndInvocationReplay(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	service := NewService(store, store)
	d := capability.Definition{
		SchemaVersion: 1, CapabilityID: "cap-pg", Name: "Review",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Roles: []capability.Role{{ID: "r", Name: "Reviewer"}},
		Steps: []capability.Step{{ID: "s", Name: "Review", RoleID: "r", Kind: capability.StepWorker, Instruction: "review"}},
	}
	if err := service.SaveDraft(t.Context(), DraftRequest{WorkspaceID: "ws-a", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(t.Context(), "ws-a", "cap-pg", 1); err != nil {
		t.Fatal(err)
	}
	request := InvokeRequest{WorkspaceID: "ws-a", ApplicationID: "app", InvocationID: "inv-a", RequestID: "req-a", CapabilityID: "cap-pg", Revision: 1, Input: json.RawMessage(`{"value":1}`)}
	first, replayed, err := service.Invoke(context.Background(), request)
	if err != nil || replayed || first.TaskID == "" {
		t.Fatalf("first invocation=%+v replayed=%v err=%v", first, replayed, err)
	}
	request.InvocationID = "inv-retry"
	second, replayed, err := service.Invoke(context.Background(), request)
	if err != nil || !replayed || second.InvocationID != "inv-a" {
		t.Fatalf("replayed invocation=%+v replayed=%v err=%v", second, replayed, err)
	}
	cancelled, err := service.CancelInvocation(t.Context(), "ws-a", "app", "inv-a")
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled invocation=%+v err=%v", cancelled, err)
	}
	var taskCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_invocation_tasks WHERE workspace_id=$1 AND invocation_id=$2 AND status='cancelled'`, "ws-a", "inv-a").Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("queued capability task count=%d err=%v", taskCount, err)
	}
	if _, _, err := service.Invoke(context.Background(), InvokeRequest{WorkspaceID: "ws-b", ApplicationID: "app", InvocationID: "inv-b", RequestID: "req-a", CapabilityID: "cap-pg", Revision: 1, Input: json.RawMessage(`{"value":1}`)}); err == nil {
		t.Fatal("different workspace should not see the published revision")
	}
}

func TestPGStoreRunsOneCapabilityTask(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap-run", Name: "Run", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "Runner"}}, Steps: []capability.Step{{ID: "s", Name: "Run", RoleID: "r", Kind: capability.StepWorker, Instruction: "run"}}}
	if err := service.SaveDraft(t.Context(), DraftRequest{WorkspaceID: "ws-run", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(t.Context(), "ws-run", "cap-run", 1); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := service.Invoke(t.Context(), InvokeRequest{WorkspaceID: "ws-run", ApplicationID: "app", InvocationID: "inv-run", RequestID: "req-run", CapabilityID: "cap-run", Revision: 1, Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := RunOne(t.Context(), store, fixtureExecutor{})
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	completed, err := service.GetInvocation(t.Context(), "ws-run", "app", invocation.InvocationID)
	if err != nil || completed.Status != "completed" || completed.ResultState != "available" || string(completed.Result) != `{"ok": true}` {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
}
