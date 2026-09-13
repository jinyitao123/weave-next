package capabilities

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
)

func TestPublishedCapabilityClaimsFrozenExactMCPToolRealPG(t *testing.T) {
	ctx := t.Context()
	seed := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws-tool','ws-tool','Tool workspace')`); err != nil {
		t.Fatal(err)
	}
	registry := mcpregistry.New(pool, []byte(strings.Repeat("k", 32)))
	server, err := registry.Create(ctx, "ws-tool", "user", mcpregistry.UpsertServerRequest{
		Slug: "calculator", DisplayName: "Calculator", Transport: mcpregistry.TransportStreamableHTTP,
		URL: "http://127.0.0.1:19001/mcp", Headers: map[string]string{"Authorization": "Bearer fixture"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	write := false
	originalSchema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	if _, err := registry.RecordProbeSuccess(ctx, "ws-tool", server.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", InputSchema: originalSchema, ReadOnlyHint: &write}}); err != nil {
		t.Fatal(err)
	}
	definition := capability.Definition{
		SchemaVersion: 1, CapabilityID: "cap-tool", Name: "Calculate",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Roles: []capability.Role{{ID: "runner", Name: "Runner"}},
		Steps: []capability.Step{{ID: "calculate", Name: "Calculate", RoleID: "runner", Kind: capability.StepTool, ToolID: "calculate",
			InputBindings: map[string]capability.ValueRef{"count": {Source: "input", Path: "/count"}}}},
		Runtime:   capability.RuntimeRequirement{Engine: "codex"},
		Resources: capability.ResourceRequirement{Tools: []capability.ToolReference{{MCPServerID: server.ID, ToolName: "calculate"}}},
	}
	store := NewPGStore(pool)
	service := NewService(store, store)
	if err := service.SaveDraft(ctx, DraftRequest{WorkspaceID: "ws-tool", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	published, err := service.Publish(ctx, "ws-tool", "cap-tool", 1)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := registry.Update(ctx, "ws-tool", server.ID, mcpregistry.UpsertServerRequest{
		Slug: "calculator", DisplayName: "Calculator", Transport: mcpregistry.TransportStreamableHTTP,
		URL: "http://127.0.0.1:19002/mcp", Enabled: true,
	})
	if err != nil || updated.FunctionalRevision <= server.FunctionalRevision {
		t.Fatalf("MCP revision did not advance: %+v err=%v", updated, err)
	}
	if _, err := registry.RecordProbeSuccess(ctx, "ws-tool", server.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnlyHint: &write}}); err != nil {
		t.Fatal(err)
	}
	// Repeating publication after a live MCP edit reads the existing immutable
	// publication instead of rebuilding authority from the current catalog.
	if err := store.SaveRevision(ctx, "ws-tool", published); err != nil {
		t.Fatalf("idempotent publication depended on live MCP state: %v", err)
	}
	if _, _, err := service.Invoke(ctx, InvokeRequest{WorkspaceID: "ws-tool", ApplicationID: "app", InvocationID: "inv-tool", RequestID: "req-tool",
		CapabilityID: "cap-tool", Revision: 1, Input: json.RawMessage(`{"count":3}`)}); err != nil {
		t.Fatal(err)
	}
	task, claimed, err := store.ClaimTask(ctx)
	if err != nil || !claimed || len(task.ToolBindings) != 1 {
		t.Fatalf("claimed=%v bindings=%+v err=%v", claimed, task.ToolBindings, err)
	}
	binding := task.ToolBindings[0]
	if len(binding.Tools) != 1 {
		t.Fatalf("invocation did not retain one frozen tool: %+v", binding)
	}
	frozenSchema, _ := frozen.CanonicalizeJSON(binding.Tools[0].InputSchema)
	wantSchema, _ := frozen.CanonicalizeJSON(originalSchema)
	if binding.ServerID != server.ID || binding.ServerRevision != server.FunctionalRevision || binding.URL != "http://127.0.0.1:19001/mcp" ||
		binding.Tools[0].Name != "calculate" || string(frozenSchema) != string(wantSchema) ||
		len(binding.WriteTools) != 1 || binding.WriteTools[0] != "calculate" || binding.AccessRef.Kind != frozen.CredentialMCPServerAccess || binding.AccessRef.ResourceID != server.ID {
		t.Fatalf("invocation did not retain published MCP authority: %+v", binding)
	}
}
