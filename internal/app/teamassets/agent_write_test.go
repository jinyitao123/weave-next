package teamassets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestAgentCommandCommitsVersionAndRejectsUnresolvableModelRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	w := &AgentWriter{Pool: pool, Registry: agentcatalog.New(pool)}
	request := teamforge.AgentWriteRequest{WorkspaceID: "builder-command", Record: registry.AgentRecord{Name: "worker", Role: "worker"}}
	created, err := w.CommitAgent(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var reply registry.AgentRecord
	if err := json.Unmarshal(created.JSON, &reply); err != nil || reply.ID == "" || reply.Version != 1 || reply.ID != created.Record.ID {
		t.Fatalf("committed response = %#v, err = %v", reply, err)
	}
	request.Record = created.Record
	request.Record.Model = "missing-model"
	if _, err := w.CommitAgent(ctx, request); !errors.Is(err, teamforge.ErrWriteModelUnresolvable) {
		t.Fatalf("model rejection = %v", err)
	}
	stored, err := w.Registry.Get(ctx, request.WorkspaceID, request.Record.Name)
	if err != nil || stored.Version != 1 || stored.Model != "" {
		t.Fatalf("rejected command changed version: %#v, err = %v", stored, err)
	}
	request.Record = created.Record
	request.Record.DisplayName = "updated"
	updated, err := w.CommitAgent(ctx, request)
	if err != nil || updated.Record.Version != 2 || updated.Record.ID != created.Record.ID {
		t.Fatalf("next version = %#v, err = %v", updated, err)
	}
}

func TestAgentCommandRollbackLeavesNoWorkspaceOrVersionRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	w := &AgentWriter{Pool: pool, Registry: agentcatalog.New(pool)}
	invalidSchema := json.RawMessage(`{`)
	request := teamforge.AgentWriteRequest{WorkspaceID: "rejected-command", Record: registry.AgentRecord{
		Name: "worker", Role: "worker", OutputSchema: &invalidSchema,
	}}
	if _, err := w.CommitAgent(ctx, request); err == nil {
		t.Fatal("invalid asset serialization committed")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM weave_workspaces WHERE id=$1", request.WorkspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("transaction did not roll back workspace creation: count=%d err=%v", count, err)
	}
	request.Record.OutputSchema = nil
	request.Record.Engine = engine.Codex
	request.InternalGraph = true
	if _, err := w.CommitAgent(ctx, request); !errors.Is(err, teamforge.ErrWriteCLIGraphConflict) {
		t.Fatalf("internal graph command accepted incompatible engine: %v", err)
	}
}
