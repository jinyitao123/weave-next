package capabilities

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

func capabilityTestContext(ctx context.Context, workspaceID string) context.Context {
	return execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, UserID: "test-user"})
}

// publishedCapabilityFixture publishes capability "cap" revision 1 in
// workspace "ws", referencing a workflow the test does not need to run.
func publishedCapabilityFixture(t *testing.T) (*pgxpool.Pool, *PGStore) {
	t.Helper()
	ctx := capabilityTestContext(t.Context(), "ws")
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	service := NewService(store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap", Name: "cap",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Workflow: &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 1}}
	if err := service.SaveDraft(ctx, DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, "ws", "cap", 1); err != nil {
		t.Fatal(err)
	}
	return pool, store
}
