package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memberIntegrationModel struct {
	fail   bool
	calls  int
	export bool
}

func (m *memberIntegrationModel) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls++
	if m.fail {
		return nil, errors.New("connection reset")
	}
	if m.export {
		found := false
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				found = true
			}
		}
		if !found {
			return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "export-once", Name: "compute", Args: `{}`}}, Usage: contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: .01}}, nil
		}
	}
	return &contract.ChatResponse{Content: "finished", Usage: contract.Usage{InputTokens: 5, OutputTokens: 2, CostUSD: .01}}, nil
}
func (*memberIntegrationModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

type memberIntegrationHosts struct {
	workerID     string
	lead, worker *memberIntegrationModel
}

func (h memberIntegrationHosts) Build(_ context.Context, b frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	m := h.lead
	if b.Agent.AgentID == h.workerID {
		m = h.worker
	}
	return compiler.FrozenBuildOpts{LLM: m, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
}

type memberIntegrationExport struct{}

func (*memberIntegrationExport) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "compute", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (*memberIntegrationExport) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{CallID: call.ID, Content: `{"weave_member_artifacts_v1":[{"path":"model/result.json","content_type":"application/json","content":"{\"value\":42}"}]}`}, nil
}

type memberIntegrationSecrets struct{}

func (memberIntegrationSecrets) Validate(context.Context, frozen.CredentialReference) error {
	return nil
}
func (memberIntegrationSecrets) Resolve(context.Context, credentials.ResolveRequest) (credentials.SecretMaterial, error) {
	return credentials.NewSecretMaterial(nil, nil, nil), nil
}

func TestWorkflowPublishedMemberParkKeepsOriginalRunAndAccountingRealPG(t *testing.T) {
	runPublishedMemberRecovery(t, 0)
}
func TestWorkflowPublishedMemberTotalBudgetRealPG(t *testing.T)    { runPublishedMemberRecovery(t, 1) }
func TestWorkflowPublishedMemberAutomaticSliceRealPG(t *testing.T) { runPublishedMemberRecovery(t, 2) }

func runPublishedMemberRecovery(t *testing.T, totalRounds uint64) {
	ctx := t.Context()
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
	bundle, _, saved := publishMemberIntegrationSample(t, pool, key, "ws", "http://127.0.0.1:1", registered.ID, registered.FunctionalRevision, "compute", "Finish the assigned calculation.", totalRounds)
	tasks := taskqueue.New(pool, nil, time.Minute)
	snapshots := snapshot.NewStore(pool)
	flows := workflow.New(pool, nil)
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, OrgStore: org.NewStore(pool), Registry: registry.New(pool), Workflow: flows, Deliverables: deliveryverify.NewStore(pool), ScheduleTransactions: pool, Snapshots: snapshots, Tasks: tasks}
	request, _ := json.Marshal(teamDispatchRequest{Task: "calculate", ClientRequestID: "00000000-0000-4000-8000-000000000055"})
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/v1/teams/team/dispatch", bytes.NewReader(request)), recorder)
	c.SetPath("/v1/teams/:id/dispatch")
	c.SetParamNames("id")
	c.SetParamValues("team")
	c.Set("tenant", "ws")
	c.Set("user_id", "test")
	if err := server.handleDispatchTeam(c); err != nil || recorder.Code != http.StatusCreated {
		t.Fatalf("dispatch=%d %s %v", recorder.Code, recorder.Body.String(), err)
	}
	var dispatched workflowManualRunResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	checkpoints := teamrun.NewPGCheckpointStore()
	members, err := loomruntime.NewMemberRunner(storeext.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	lead, worker := &memberIntegrationModel{}, &memberIntegrationModel{fail: totalRounds == 0, export: true}
	runtime := &teamrun.WorkflowSerialRuntime{OutputRecorder: deliveryverify.NewStore(pool), Members: members, Artifacts: flows, Loader: &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}, HostFactory: memberIntegrationHosts{bundle.Agent.AgentID, lead, worker}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: snapshots}
	executor := &teamrun.Executor{MemberBudgets: loomruntime.MemberBudgetCoordinator{}, Tasks: tasks, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: snapshots, Runs: runs, Tasks: tasks}}
	if ok, err := executor.ProcessNext(ctx, "first"); err != nil || !ok {
		t.Fatalf("first=%v %v", ok, err)
	}
	parked, err := runs.Get(ctx, "ws", dispatched.RunID)
	wantStatus := teamrun.StatusParked
	if totalRounds == 2 {
		wantStatus = teamrun.StatusRunning
	}
	if err != nil || parked.Status != wantStatus {
		if parked.CauseSummary != nil {
			t.Log(*parked.CauseSummary)
		}
		t.Fatalf("parked=%+v err=%v", parked, err)
	}
	getCheckpoint := func() teamrun.WorkflowCheckpointV1 {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		cp, err := checkpoints.GetTx(ctx, tx, "ws", dispatched.RunID)
		if err != nil {
			t.Fatal(err)
		}
		return cp
	}
	if totalRounds == 1 {
		projected := []runActivityMember{{Stages: []runActivityMemberStage{{NodeID: "compute", Status: "running"}}}}
		server.StoreExt = storeext.New(pool)
		if node := server.reconcileRunActivityRecovery(ctx, parked, projected); node != "compute" {
			t.Fatal("budget wait node missing")
		}
		stage := projected[0].Stages[0]
		if stage.Status != "waiting" || stage.FailureClass != "" || !stage.Retryable || stage.BudgetPause == nil || stage.BudgetPause.RoundsUsed != 1 {
			t.Fatalf("budget projected as failure or completion: %+v", stage)
		}
		raw, _ := json.Marshal(stage)
		if strings.Contains(string(raw), "yield_token") {
			t.Fatal("private resume token projected")
		}
	}
	before := getCheckpoint()
	if before.ActiveMember == nil {
		t.Fatal("lost member pointer")
	}
	memberID := loomruntime.MemberRunID("ws", dispatched.RunID, dispatched.RunID, "compute", before.ActiveMember.CallID)
	retry := &teamrun.StageRetryService{MemberBudgets: loomruntime.MemberBudgetCoordinator{}, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks}
	command := teamrun.StageRetryRequest{WorkspaceID: "ws", RunID: dispatched.RunID, NodeID: "compute", IdempotencyKey: "continue-once"}
	if totalRounds != 2 {
		if totalRounds == 1 {
			if _, err := retry.Retry(ctx, command); err == nil {
				t.Fatal("total budget renewed without an explicit increase")
			}
			command.AuthorizedTotalRounds = 2
		}
		if _, err := retry.Retry(ctx, command); err != nil {
			t.Fatal(err)
		}
		if _, err := retry.Retry(ctx, command); err != nil {
			t.Fatal(err)
		}
		if totalRounds == 1 {
			changed := command
			changed.AuthorizedTotalRounds = 3
			if _, err := retry.Retry(ctx, changed); err == nil {
				t.Fatal("replayed request changed its allowance")
			}
		}
	}
	if cp := getCheckpoint(); cp.ActiveMember.CallID != before.ActiveMember.CallID {
		t.Fatal("continue allocated another logical call")
	}
	worker.fail = false
	if ok, err := executor.ProcessNext(ctx, "second"); err != nil || !ok {
		t.Fatalf("second=%v %v", ok, err)
	}
	finished, err := runs.Get(ctx, "ws", dispatched.RunID)
	if err != nil || finished.Status != teamrun.StatusSucceeded {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
	wantWorkerCalls := 3
	if totalRounds > 0 {
		wantWorkerCalls = 2
	}
	if lead.calls != 1 || worker.calls != wantWorkerCalls {
		t.Fatalf("lead=%d worker=%d", lead.calls, worker.calls)
	}
	var count, attempt int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_workflow_member_runs WHERE workspace_id='ws' AND parent_run_id=$1`, dispatched.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempt_generation FROM weave_run_attempt_leases WHERE workspace_id='ws' AND run_id=$1`, memberID).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || attempt != 2 {
		t.Fatalf("count=%d attempt=%d", count, attempt)
	}
	raw, _, err := storeext.New(pool).ReadValue(ctx, "audit:ws", dispatched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	inspected := loomruntime.InspectTerminalRecord(true, raw)
	if inspected.Err != nil || inspected.Entry == nil {
		t.Fatalf("terminal=%v", inspected.Err)
	}
	terminal := inspected.Entry
	if terminal.SelfExclusive.InputTokens != 5 || terminal.SubtreeTotal.InputTokens != 15 || len(terminal.ChildBreakdown) != 1 || terminal.ChildBreakdown[0].RunID != memberID {
		t.Fatalf("double accounting=%+v", terminal)
	}
	if cp := getCheckpoint(); cp.ActiveMember != nil || cp.NodeID != "deliver" || len(cp.MemberBreakdown) != 1 {
		t.Fatalf("result not accepted: %+v", cp)
	}
	var delivered string
	if err := pool.QueryRow(ctx, `SELECT content FROM weave_final_deliverables WHERE workspace_id='ws' AND run_id=$1 AND metadata->>'artifact_kind'='final' AND metadata->>'filename'='model/result.json'`, dispatched.RunID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != `{"value":42}` {
		t.Fatalf("final file=%q", delivered)
	}
	_ = saved
}
func publishMemberIntegrationSample(t *testing.T, pool *pgxpool.Pool, key []byte, workspace, serverURL, serverID string, revision int64, toolName, prompt string, budget ...uint64) (frozen.FrozenExecutionBundle, compiler.FrozenResolver, *workflow.PublishedArtifactContent) {
	t.Helper()
	ctx := t.Context()
	providers := credentials.New(pool, key)
	if err := providers.Upsert(ctx, workspace, llmrouter.ProviderConfig{ID: "fixture", Name: "Fixture", BaseURL: serverURL, APIKey: "test-provider-secret", Models: []string{"fixture-model"}}); err != nil {
		t.Fatal(err)
	}
	agents := registry.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Return the brief."}}
	worker := &registry.AgentRecord{Name: "worker", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: prompt}, MCPServers: []registry.MCPServerConfig{{ServerID: serverID, Filter: []string{toolName}}}}
	if len(budget) > 0 && budget[0] > 0 {
		worker.ToolLoopControl = &frozen.ToolLoopControl{SliceRounds: 1, InitialTotalRounds: budget[0]}
	}
	for _, record := range []*registry.AgentRecord{lead, worker} {
		if err := agents.Put(ctx, workspace, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team',$1,'Tools',$2,'active')`, workspace, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.NewTeamWorkerRepository(pool).Create(ctx, workspace, registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID, Duty: "Compute", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: prompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"brief","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"brief","type":"lead","config":{"instruction":"Brief"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"compute","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Execute the configured tool task"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"compute","path":""}}}],"edges":[{"id":"a","from_node_id":"brief","to_node_id":"compute","route":"success"},{"id":"b","from_node_id":"compute","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version))
	store := workflow.New(pool, nil)
	draft, err := store.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: workspace, TeamID: "team", Name: "Tool flow"}, workflow.DraftInput{CreatedBy: "test", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := memberIntegrationDescriptors(t)
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

func memberIntegrationDescriptors(t *testing.T) *compiler.DescriptorRegistry {
	t.Helper()
	descriptors := compiler.NewDescriptorRegistry()
	for _, descriptor := range []compiler.GraphFactoryDescriptor{compiler.NewStandardFrozenDescriptor(), compiler.NewStandardFrozenToolsDescriptor(), compiler.NewStandardFrozenCLIToolsDescriptor()} {
		if err := descriptors.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return descriptors
}
