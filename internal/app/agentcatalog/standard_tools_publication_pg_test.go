package agentcatalog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/workflow"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
)

type publicationTestSecrets struct{}

func (publicationTestSecrets) Validate(context.Context, frozen.CredentialReference) error { return nil }
func (publicationTestSecrets) Resolve(context.Context, credentials.ResolveRequest) (credentials.SecretMaterial, error) {
	return credentials.NewSecretMaterial(nil, nil, nil), nil
}

func TestStandardToolsPublicationReachesActualMCPAndPreservesFrozenContractRealPG(t *testing.T) {
	ctx := t.Context()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Exercise the production pgx parameter encoding after schema setup.
	config := pool.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	productionPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer productionPool.Close()
	pool = productionPool
	key := []byte(strings.Repeat("k", 32))
	workspace := "tools-publication"
	ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspace, UserID: "test"})
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	file := filepath.Join(t.TempDir(), "result.txt")
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "actual-test-mcp", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "calculate", "description": "Compute and write a result", "inputSchema": schema, "annotations": map[string]any{"readOnlyHint": false}}}}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Name != "calculate" {
				http.Error(w, "unknown tool", 400)
				return
			}
			if err := os.WriteFile(file, []byte(fmt.Sprint(21*2)), 0o600); err != nil {
				http.Error(w, "write failed", 500)
				return
			}
			effects.Add(1)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "42"}}, "isError": false}
		default:
			http.Error(w, "unknown method", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspace); err != nil {
		t.Fatal(err)
	}
	mcp := mcpregistry.New(pool, key)
	registered, err := mcp.Create(ctx, workspace, "test", mcpregistry.UpsertServerRequest{Slug: "calculation", DisplayName: "Calculation", Transport: mcpregistry.TransportStreamableHTTP, URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	catalog := []mcpregistry.Tool{{Name: "calculate", Description: "Compute and write a result", InputSchema: schema}}
	if _, err := mcp.RecordProbeSuccess(ctx, workspace, registered.ID, "2025-03-26", json.RawMessage(`{}`), catalog); err != nil {
		t.Fatal(err)
	}
	bundle, resolver, saved := publishStandardToolSample(t, pool, key, workspace, server.URL, registered.ID, registered.FunctionalRevision, "calculate", "Use the configured calculation tool.")
	descriptors := publicationDescriptors(t)
	seen := 0
	model := testutil.NewScriptedLLM(nil, testutil.ScriptedResponse{Respond: func(request contract.ChatRequest) *contract.ChatResponse {
		if len(request.Tools) != 1 || request.Tools[0].Name != "calculate" {
			t.Errorf("model did not receive configured tool: %#v", request.Tools)
		}
		seen++
		for _, message := range request.Messages {
			if message.Role == "tool" && strings.Contains(message.Content, "42") {
				return &contract.ChatResponse{Content: "42"}
			}
		}
		return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "calculate-1", Name: "calculate", Args: `{}`}}}
	}})
	opts, closer, err := workflow.NewRuntimeHostFactory().Build(ctx, bundle, publicationTestSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	opts.LLM = model
	opts = loomruntime.InstallFrozenMemberJournal(loomruntime.InstallFrozenUsageTracking(opts))
	compiled, err := compiler.CompileFrozenWithRegistry(ctx, descriptors, bundle, resolver, opts)
	if err != nil {
		t.Fatal(err)
	}
	team, snapshot, parent, seq := "team", "publication-snapshot", "publication-parent", int64(1)
	attribution, err := loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{
		Scope: loomruntime.TerminalAttributionFixedWorkflow, WorkspaceID: workspace,
		TeamID: &team, WorkflowID: &saved.WorkflowID, WorkflowVersion: &saved.WorkflowVersion, RunSnapshotID: &snapshot,
		ParentRunID: &parent, ParentSeq: &seq, AggregationParentRunID: &parent,
	}, &loomruntime.TerminalSnapshotEvidence{WorkspaceID: workspace, RunID: snapshot, TeamID: team,
		Mode: "fixed_workflow", WorkflowID: &saved.WorkflowID, WorkflowVersion: &saved.WorkflowVersion})
	if err != nil {
		t.Fatal(err)
	}
	records := storeext.New(pool)
	runner, err := loomruntime.NewMemberRunner(records)
	if err != nil {
		t.Fatal(err)
	}
	request := loomruntime.MemberRequest{WorkspaceID: workspace, ParentRunID: parent, RunSnapshotID: snapshot, NodeID: "compute", CallID: "first",
		ParentGeneration: 1, Bundle: bundle, ArtifactHash: saved.ContentHash, Graph: compiled, Attribution: attribution,
		ParentGuard: func(ctx context.Context, tx pgx.Tx) error {
			return records.LockValueTx(ctx, tx, "test-parent:"+workspace, parent)
		},
		Input: loom.State{"last_user_message": "Calculate", "messages": []contract.Message{{Role: "user", Content: "Calculate"}}},
	}
	result, err := runner.Run(ctx, request)
	if err != nil || result == nil {
		t.Fatalf("execute published graph: %v", err)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "42" || effects.Load() != 1 || seen != 2 {
		t.Fatalf("actual effect=%s calls=%d model turns=%d err=%v", content, effects.Load(), seen, err)
	}
	if cached, err := runner.Run(ctx, request); err != nil || cached.RunID != result.RunID || effects.Load() != 1 || seen != 2 {
		t.Fatalf("published member result reuse: %v", err)
	}
	// A changed registry catalog does not rewrite the already-published bundle.
	if _, err := mcp.RecordProbeSuccess(ctx, workspace, registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "different", InputSchema: schema}}); err != nil {
		t.Fatal(err)
	}
	fresh, err := workflow.New(pool, nil).GetArtifact(ctx, workspace, "flow", saved.WorkflowVersion)
	if err != nil || fresh.ContentHash != saved.ContentHash || string(fresh.Payload) != string(saved.Payload) {
		t.Fatal("published artifact changed with live catalog")
	}
	checkTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer checkTx.Rollback(context.Background())
	if _, err := mcpregistry.ResolveCurrentMCPToolsTx(ctx, checkTx, workspace, registered.ID, mcpregistry.MCPAgentPolicy{Filter: []string{"calculate"}}); err == nil {
		t.Fatal("publication accepted missing required tool")
	}
}

func publishStandardToolSample(t *testing.T, pool *pgxpool.Pool, key []byte, workspace, serverURL, serverID string, revision int64, toolName, prompt string) (frozen.FrozenExecutionBundle, compiler.FrozenResolver, *workflow.PublishedArtifactContent) {
	t.Helper()
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: workspace, UserID: "test"})
	providers := credentials.New(pool, key)
	if err := providers.Upsert(ctx, workspace, llmrouter.ProviderConfig{ID: "fixture", Name: "Fixture", CredentialScope: frozen.CredentialScopeUser, CredentialUserID: "test", BaseURL: serverURL, APIKey: "test-provider-secret", Models: []string{"fixture-model"}}); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Return the brief."}}
	worker := &registry.AgentRecord{Name: "worker", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: prompt}, MCPServers: []registry.MCPServerConfig{{ServerID: serverID, Filter: []string{toolName}}}}
	for _, record := range []*registry.AgentRecord{lead, worker} {
		if err := agents.Put(ctx, workspace, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team',$1,'Tools',$2,'active')`, workspace, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := agentcatalog.NewTeamWorkerRepository(pool).Create(ctx, workspace, registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID, Duty: "Compute", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: prompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"brief","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"brief","type":"lead","config":{"instruction":"Brief"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"compute","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Execute the configured tool task"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"compute","path":""}}}],"edges":[{"id":"a","from_node_id":"brief","to_node_id":"compute","route":"success"},{"id":"b","from_node_id":"compute","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version))
	store := workflow.New(pool, nil)
	draft, err := store.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: workspace, TeamID: "team", Name: "Tool flow"}, workflow.DraftInput{CreatedBy: "test", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := publicationDescriptors(t)
	builder := workflow.NewCandidateBuilder(store, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), descriptors)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	candidate, report, err := builder.BuildTx(ctx, tx, workflow.CandidateInput{WorkspaceID: workspace, WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		t.Fatalf("build publication: err=%+v report=%+v", err, report)
	}
	publication, err := workflow.PublicationFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPublicationTx(ctx, tx, publication); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetArtifact(ctx, workspace, "flow", draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved.Payload), "test-provider-secret") {
		t.Fatal("credential value leaked into frozen publication")
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID: saved.WorkspaceID, WorkflowID: saved.WorkflowID, WorkflowVersion: saved.WorkflowVersion,
		ArtifactSchemaVersion: saved.ArtifactSchemaVersion, CanonicalizationAlgorithm: saved.CanonicalizationAlgorithm,
		CanonicalizationVersion: saved.CanonicalizationVersion, HashAlgorithm: saved.HashAlgorithm, ContentHash: saved.ContentHash, Payload: saved.Payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	var bundle frozen.FrozenExecutionBundle
	for _, entry := range payload.Bundles {
		if entry.Agent.AgentID == worker.ID {
			bundle = entry
		} else if entry.FactoryKey.FactoryVersion != "1" {
			t.Fatal("unchanged lead unexpectedly upgraded")
		}
	}
	if bundle.FactoryKey != compiler.StandardFrozenToolsKey() || len(bundle.MCPBindings) != 1 || len(bundle.MCPBindings[0].Tools) != 1 {
		t.Fatalf("tools missing after publication: %#v", bundle.FactoryKey)
	}
	if bundle.MCPBindings[0].ServerRevision != revision {
		t.Fatal("MCP revision not pinned")
	}
	resolver, err := freezer.RebuildArtifactResolver(bundle)
	if err != nil {
		agentHash, _ := frozen.HashDTO(bundle.Agent, frozen.PreorderFrozenAgentRecord)
		mcpHash, _ := frozen.HashDTO(bundle.MCPBindings[0], frozen.PreorderFrozenMCPBinding)
		modelHash, _ := frozen.HashDTO(bundle.PrimaryModel, frozen.PreorderFrozenModelBinding)
		t.Logf("agent hash=%s; mcp hash=%s stored=%s; model hash=%s stored=%s; dependencies=%+v", agentHash, mcpHash, bundle.MCPBindings[0].ContentHash, modelHash, bundle.PrimaryModel.ContentHash, bundle.Dependencies)
		t.Fatal(err)
	}
	return bundle, resolver, saved
}

func publicationDescriptors(t *testing.T) *compiler.DescriptorRegistry {
	t.Helper()
	descriptors := compiler.NewDescriptorRegistry()
	for _, descriptor := range []compiler.GraphFactoryDescriptor{compiler.NewStandardFrozenDescriptor(), compiler.NewStandardFrozenToolsDescriptor()} {
		if err := descriptors.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return descriptors
}
