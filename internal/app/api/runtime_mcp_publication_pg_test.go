package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/kernelbindings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestPublishedCLIToTaskKeepsMCPContractAfterLiveEditsRealPG(t *testing.T) {
	base := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	ctx, cancel := context.WithTimeout(base, 15*time.Second)
	defer cancel()
	seed := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','x','admin');`); err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	key := []byte(strings.Repeat("k", 32))
	mcp := mcpregistry.New(pool, key)
	server, err := mcp.Create(ctx, "ws", "user", mcpregistry.UpsertServerRequest{Slug: "published", DisplayName: "Published", Transport: mcpregistry.TransportStreamableHTTP, URL: "http://127.0.0.1:1/original", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)
	if _, err := mcp.RecordProbeSuccess(ctx, "ws", server.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", InputSchema: schema}}); err != nil {
		t.Fatal(err)
	}
	rs := runtimes.NewStore(pool)
	runtime, _, err := rs.Create(ctx, "ws", "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []runtimes.EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture-cli 1", ProtocolVersion: "1", AuthMode: runtimes.AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
		t.Fatal(err)
	}
	workerID, _ := publishTeamDeliveryCLI(t, pool, key, runtime.ID, registry.MCPServerConfig{ServerID: server.ID, Filter: []string{"calculate"}})
	agents := kernelbindings.NewRegistry(pool)
	lead, err := agents.Get(ctx, "ws", "lead")
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{RunID: "snapshot", WorkspaceID: "ws", TeamID: "team", SourceRef: "fixture", SnapshotSchemaVersion: 2, Mode: "free_collab", LeadAvatarID: lead.ID, LeadAvatarVersion: lead.Version,
		WorkerVersions: json.RawMessage(`{}`), TeamWorkerSnapshot: json.RawMessage(`[]`), InlineDependencies: json.RawMessage(`{}`), RuntimeAssignment: json.RawMessage(`{}`),
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-05T00:00:00Z"}`), RunAssociations: json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`), TriggerSourceV2: json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`)})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := workflow.NewArtifactStore(pool, nil).GetArtifact(ctx, "ws", "flow", 1)
	if err != nil {
		t.Fatal(err)
	}
	envelope := frozen.ArtifactEnvelopeV1{WorkspaceID: saved.WorkspaceID, WorkflowID: saved.WorkflowID, WorkflowVersion: saved.WorkflowVersion, ArtifactSchemaVersion: saved.ArtifactSchemaVersion, CanonicalizationAlgorithm: saved.CanonicalizationAlgorithm, CanonicalizationVersion: saved.CanonicalizationVersion, HashAlgorithm: saved.HashAlgorithm, ContentHash: saved.ContentHash, Payload: saved.Payload}
	decoded, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, bundle := range decoded.Bundles {
		if bundle.Agent.AgentID == workerID {
			found = bundle.FactoryKey == compiler.StandardFrozenCLIToolsKey() && len(bundle.MCPBindings) == 1
		}
	}
	if !found {
		t.Fatal("CLI publication did not freeze MCP v3")
	}
	// Change both live sources before loading the saved artifact.
	worker, err := agents.Get(ctx, "ws", "worker")
	if err != nil {
		t.Fatal(err)
	}
	worker.MCPServers = nil
	if err := agents.Put(ctx, "ws", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := mcp.RecordProbeSuccess(ctx, "ws", server.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	executor := runtimes.NewExecutor(tasks, rs, "", "")
	loader := workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t), CLIExecutor: executor, RunSnapshotID: "snapshot"}
	artifact, err := loader.Load(ctx, envelope, workflow.NewRuntimeHostFactory(), memberIntegrationSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	var entry *workflow.RuntimeCLIEntry
	for _, candidate := range artifact.Entries {
		if candidate.AgentID == workerID {
			entry = candidate.CLI
		}
	}
	if entry == nil {
		t.Fatal("published worker did not load CLI entry")
	}
	invokeCtx := execution.WithInvocationID(execution.WithNodeID(ctx, "compute"), "published-cli-proof")
	done := make(chan error, 1)
	go func() { _, err := entry.ExecuteResult(invokeCtx, "Use the published tool"); done <- err }()
	var claimed *taskqueue.Task
	for claimed == nil {
		select {
		case err := <-done:
			t.Fatalf("execution ended before admission: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
		claimed, err = tasks.Claim(ctx, runtimes.RuntimeWorkerID("ws", runtime.ID), taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: runtime.ID, IdentityKind: taskqueue.IdentityAgent})
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	var payload runtimes.EngineExecRequest
	if err := json.Unmarshal(claimed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !validFrozenMCPTask(claimed, payload) || payload.FrozenMCP.AgentVersion != 1 || payload.FrozenMCP.Bindings[0].URL != "http://127.0.0.1:1/original" || !strings.Contains(string(payload.FrozenMCP.Bindings[0].Tools[0].InputSchema), "required") || len(payload.Env) != 0 {
		t.Fatal("admission rebuilt or dropped published authority")
	}
	terminal, _ := json.Marshal(runtimes.EngineExecResult{Subject: claimed.Subject, ClaimEpoch: claimed.ClaimEpoch, Output: "completed fixture", Status: "completed"})
	if err := tasks.CompleteClaimed(ctx, claimed.ID, claimed.WorkerID, terminal, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := entry.ExecuteResult(invokeCtx, "Use the published tool"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM weave_task_queue WHERE workspace_id='ws' AND kind='engine_exec'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("resume changed physical identity: count=%d err=%v", count, err)
	}
}
