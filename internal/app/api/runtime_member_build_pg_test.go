package api

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type countedCloser struct{ closed *int }

func (c countedCloser) Close() error { *c.closed++; return nil }

// A step builds the runtime host of, and resolves credentials for, only the
// members it runs: loading a published artifact builds nothing, a member is
// built once however often it runs, and the artifact closes what it built.
func TestRuntimeLoaderBuildsAMemberOnlyWhenItRunsRealPG(t *testing.T) {
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "test"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	preparedPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer preparedPool.Close()
	pool = preparedPool
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('test','ws','test','x','admin')`); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	mcp := mcpregistry.New(pool, key)
	registered, err := mcp.Create(ctx, "ws", "test", mcpregistry.UpsertServerRequest{Slug: "compute", DisplayName: "Compute", Transport: mcpregistry.TransportStreamableHTTP, URL: "http://127.0.0.1:1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mcp.RecordProbeSuccess(ctx, "ws", registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "compute", InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatal(err)
	}
	_, _, saved := publishMemberIntegrationSample(t, pool, key, "ws", "test", "http://127.0.0.1:1", registered.ID, registered.FunctionalRevision, "compute", "Finish the assigned calculation.")
	envelope := frozen.ArtifactEnvelopeV1{WorkspaceID: saved.WorkspaceID, WorkflowID: saved.WorkflowID, WorkflowVersion: saved.WorkflowVersion, ArtifactSchemaVersion: saved.ArtifactSchemaVersion, CanonicalizationAlgorithm: saved.CanonicalizationAlgorithm, CanonicalizationVersion: saved.CanonicalizationVersion, HashAlgorithm: saved.HashAlgorithm, ContentHash: saved.ContentHash, Payload: saved.Payload}

	built, closed := map[string]int{}, 0
	factory := workflow.RuntimeHostFactoryFunc(func(_ context.Context, bundle frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		built[bundle.Agent.AgentID]++
		return compiler.FrozenBuildOpts{LLM: &memberIntegrationModel{}, Tools: &memberIntegrationExport{}}, countedCloser{&closed}, nil
	})
	loader := &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}
	artifact, err := loader.Load(ctx, envelope, factory, memberIntegrationSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Entries) != 2 || len(built) != 0 {
		t.Fatalf("load built hosts up front: entries=%d built=%v", len(artifact.Entries), built)
	}
	first, second := artifact.Entries[0], artifact.Entries[1]
	// Entries follow the members' ids, so which one is the durable worker varies.
	for _, entry := range artifact.Entries {
		if !entry.InProcess() || entry.Graph != nil {
			t.Fatalf("unbuilt member entry = %+v", entry)
		}
	}
	if !first.DurableMember() && !second.DurableMember() {
		t.Fatal("no unbuilt member entry is recognized as a durable member")
	}
	graph, err := first.LoadGraph()
	if err != nil || graph == nil {
		t.Fatalf("first member graph = %v %v", graph, err)
	}
	again, _ := artifact.Entries[0].LoadGraph()
	if again != graph || built[first.AgentID] != 1 || len(built) != 1 {
		t.Fatalf("member was built again or another member was built: %v", built)
	}
	if err := artifact.Close(); err != nil || closed != 1 {
		t.Fatalf("close = %v, closed hosts = %d", err, closed)
	}
	// A member first reached after the step ended gets no graph, and the host
	// built for it is closed at once.
	if late, err := second.LoadGraph(); err == nil || late != nil || closed != 2 {
		t.Fatalf("member built after close = %v %v, closed hosts = %d", late, err, closed)
	}
}
