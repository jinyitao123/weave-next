package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

const publishedBusinessCapability = "forge:action:sales_quote.AdjustPrice"

type publishedBusinessModel struct{ calls int }

func (m *publishedBusinessModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls++
	for _, message := range request.Messages {
		if message.Role == "tool" {
			if !strings.Contains(message.Content, "business_receipt") {
				return nil, errors.New("business tool did not return its real receipt")
			}
			return &contract.ChatResponse{Content: "Business receipt verified."}, nil
		}
	}
	for _, tool := range request.Tools {
		if strings.HasPrefix(tool.Name, "forge_sales_quote_adjustprice_") {
			return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "model-call", Name: tool.Name, Args: `{"params":{"line_id":"line-a"}}`}}}, nil
		}
	}
	return nil, errors.New("published business capability was not offered")
}
func (*publishedBusinessModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

// The publication selector, frozen loader and workflow MemberRunner are real.
// Only inference and the external Forge endpoint are fixtures; no test injects
// an operation ID or installs a journal by hand.
func TestBusinessOnlyPublicationDispatchUsesDurableProvenanceRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
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
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','unused','admin'); INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES('forge:task-delegation-test','native-user','ws','user','native-org')`); err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	var grant businessaction.TaskDelegationGrant
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
			t.Error("Forge did not receive the scoped task token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == businessaction.TaskDelegationPath+"/current" {
			_ = json.NewEncoder(w).Encode(grant)
			return
		}
		if r.URL.Path == businessaction.TaskDelegationPath+"/objects/sales_quote" {
			_, _ = io.WriteString(w, `{"type":"object","name":"sales_quote","item":{"name":"sales_quote","actions":[{"name":"AdjustPrice","params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}}`)
			return
		}
		if r.URL.Path != businessaction.TaskDelegationPath+"/mcp" {
			t.Errorf("unexpected Forge path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture-forge", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "list_actions", "inputSchema": json.RawMessage(`{"type":"object"}`)}, {"name": "run_action", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		case "tools/call":
			content := `{"actions":[{"name":"AdjustPrice","objectName":"sales_quote","label":"Adjust price","requiresRecord":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}`
			if request.Params.Name == "run_action" {
				var action struct {
					Action, Object, Record string
					Params                 map[string]string
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(request.Params.Arguments, &raw); err != nil {
					t.Error(err)
					return
				}
				_ = json.Unmarshal(raw["actionName"], &action.Action)
				_ = json.Unmarshal(raw["objectName"], &action.Object)
				_ = json.Unmarshal(raw["recordId"], &action.Record)
				_ = json.Unmarshal(raw["params"], &action.Params)
				if action.Action != "AdjustPrice" || action.Object != "sales_quote" || action.Record != "record-a" || action.Params["line_id"] != "line-a" || !strings.HasPrefix(action.Params["idempotency_key"], "weave-op-") {
					t.Error("unfrozen action or missing operation key")
					return
				}
				var journalCount, reservedCount int
				if err := pool.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM loom_store WHERE namespace='member-operation:ws' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'), (SELECT count(*) FROM weave_team_run_activity_events WHERE kind='business_action_started' AND detail->>'operation_id'=$1)`, action.Params["idempotency_key"]).Scan(&journalCount, &reservedCount); err != nil || journalCount != 1 || reservedCount != 1 {
					t.Errorf("side effect preceded durable intent and action reservation: journal=%d reserved=%d err=%v", journalCount, reservedCount, err)
					return
				}
				effects.Add(1)
				content = `{"ok":true,"data":{"business_receipt":"receipt-one"}}`
			} else if request.Params.Name != "list_actions" {
				t.Errorf("unexpected tool %s", request.Params.Name)
				return
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": content}}}
		default:
			t.Errorf("unexpected method %s", request.Method)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(forge.Close)
	key := []byte(strings.Repeat("1", 32))
	bundle, _, saved := publishConfiguredMemberIntegrationSample(t, pool, key, "ws", "user", forge.URL, "", 0, "", "Execute the authorized adjustment.", func(record *registry.AgentRecord) {
		record.MCPServers = nil
		record.BusinessCapabilityIDs = []string{publishedBusinessCapability}
	})
	if bundle.FactoryKey != compiler.StandardFrozenToolsKey() || len(bundle.MCPBindings) != 0 || bundle.Agent.Limits.ToolLoopControl != nil {
		t.Fatal("business-only publication did not select the existing durable contract")
	}
	artifacts := workflow.NewArtifactStore(pool, nil)
	tasks := taskqueue.New(pool, nil, time.Minute)
	snapshots := snapshot.NewStore(pool)
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, OrgStore: orgstore.NewStore(pool), Registry: agentcatalog.New(pool), Workflow: workflowcatalog.New(pool, nil, artifacts), WorkflowArtifacts: artifacts, Deliverables: deliveryverify.NewStore(pool), ScheduleTransactions: pool, Snapshots: snapshots, Tasks: tasks, Config: &config.Config{ForgeSessionURL: forge.URL + "/api/v1/auth/me"}}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, teamconstruction.NewPublicationAuthority(pool, nil))
	request := dispatchInputRegistrationFixture("business-session", "Adjust the authorized record", "")
	version := 1
	request.WorkflowID, request.WorkflowVersion = "flow", &version
	request.AuthorizedBusinessCapabilityIDs = &[]string{publishedBusinessCapability}
	request.BusinessRecord = &dispatchBusinessRecord{ObjectName: "sales_quote", RecordID: "record-a"}
	grant = testTaskGrant(t, forge.URL, "native-user", "native-org", 1, scopeForRegistration(request))
	registered, receipt := registerTaskInput(t, server, request, "fixture-task-token")
	if registered.Code != http.StatusCreated {
		t.Fatalf("registration=%d %s", registered.Code, registered.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID})
	c, response := dispatchInputTestContext(body, "/v1/teams/team/dispatch", "ws", "user")
	if err := server.handleDispatchTeam(c); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("dispatch=%d %s err=%v", response.Code, response.Body.String(), err)
	}
	var dispatched workflowManualRunResponse
	if err := json.Unmarshal(response.Body.Bytes(), &dispatched); err != nil {
		t.Fatal(err)
	}
	members, err := loomruntime.NewMemberRunner(storeext.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	checkpoints := teamrun.NewPGCheckpointStore()
	activities := &teamrun.PGActivityStore{Transactions: pool}
	model := &publishedBusinessModel{}
	host := workflow.RuntimeHostFactoryFunc(func(_ context.Context, candidate frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		var llm contract.LLM = &memberIntegrationModel{}
		if candidate.Agent.AgentID == bundle.Agent.AgentID {
			llm = model
		}
		return compiler.FrozenBuildOpts{LLM: llm, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
	})
	runtime := &teamrun.WorkflowSerialRuntime{OutputRecorder: deliveryverify.NewStore(pool), Members: members, Artifacts: artifacts, Loader: &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}, HostFactory: businessaction.Factory{Inner: host, Store: businessaction.NewStore(pool, tasks, key)}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: snapshots, Activities: activities}
	executor := &teamrun.Executor{Tasks: tasks, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: snapshots, Runs: runs, Tasks: tasks}}
	if ok, err := executor.ProcessNext(ctx, "business-worker"); err != nil || !ok {
		t.Fatalf("execute=%v err=%v", ok, err)
	}
	finished, err := runs.Get(ctx, "ws", dispatched.RunID)
	if err != nil || finished.Status != teamrun.StatusSucceeded || effects.Load() != 1 || model.calls != 2 {
		t.Fatalf("run=%+v effects=%d model=%d err=%v", finished, effects.Load(), model.calls, err)
	}
	events, err := activities.ListBusinessActionEvents(ctx, "ws", dispatched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := teamrun.ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != 1 || outcomes[0].Status != "succeeded" {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
	var memberCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_workflow_member_runs WHERE workspace_id='ws' AND parent_run_id=$1`, dispatched.RunID).Scan(&memberCount); err != nil || memberCount != 1 {
		t.Fatalf("member count=%d err=%v", memberCount, err)
	}
	assertUnsupportedBusinessArtifactsRejectedBeforeHosts(t, saved)
	flows := workflowcatalog.New(pool, nil, artifacts)
	draft, err := flows.CreateDraft(ctx, "ws", "flow", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = flows.UpdateDraft(ctx, "ws", "flow", draft.Version, draft.UpdatedAt, workflow.DraftInput{CreatedBy: "user", TriggerConfig: draft.TriggerConfig, GraphDefinition: businessParallelGraph(bundle.Agent.AgentID, bundle.Agent.AgentVersion)})
	if err != nil {
		t.Fatal(err)
	}
	builder := workflowcatalog.NewCandidateBuilder(flows, agentcatalog.New(pool), delivery.New(pool, key), skills.New(pool), credentials.New(pool, key), schedule.New(pool, nil), memberIntegrationDescriptors(t))
	authority := teamconstruction.NewPublicationAuthority(pool, builder)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: draft.Version}); !errors.Is(err, compiler.ErrFactoryCompileFailed) {
		t.Fatalf("parallel business publication was not refused: %v", err)
	}
}

func businessParallelGraph(agentID string, version int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"fanout","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"fanout","type":"parallel","config":{"join_node_id":"join"}},{"id":"first","type":"worker","config":{"agent_id":%q,"agent_version":%d,"kind":"dispatch","result_requirement":"Execute the authorized action"},"output":{"type":"text"}},{"id":"second","type":"worker","config":{"agent_id":%q,"agent_version":%d,"kind":"dispatch","result_requirement":"Execute the authorized action"},"output":{"type":"text"}},{"id":"join","type":"join","config":{"policy":"all_success"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[{"id":"a","from_node_id":"fanout","to_node_id":"first","route":"branch"},{"id":"b","from_node_id":"fanout","to_node_id":"second","route":"branch"},{"id":"c","from_node_id":"first","to_node_id":"join","route":"join"},{"id":"d","from_node_id":"second","to_node_id":"join","route":"join"},{"id":"e","from_node_id":"join","to_node_id":"deliver","route":"success"}]}`, agentID, version, agentID, version))
}

func assertUnsupportedBusinessArtifactsRejectedBeforeHosts(t *testing.T, saved *workflow.PublishedArtifactContent) {
	t.Helper()
	for _, parallel := range []bool{false, true} {
		envelope := frozen.ArtifactEnvelopeV1{WorkspaceID: saved.WorkspaceID, WorkflowID: saved.WorkflowID, WorkflowVersion: saved.WorkflowVersion, ArtifactSchemaVersion: saved.ArtifactSchemaVersion, CanonicalizationAlgorithm: saved.CanonicalizationAlgorithm, CanonicalizationVersion: saved.CanonicalizationVersion, HashAlgorithm: saved.HashAlgorithm, ContentHash: saved.ContentHash, Payload: saved.Payload}
		payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		for index := range payload.Bundles {
			if len(payload.Bundles[index].Agent.BusinessCapabilityIDs) > 0 {
				if parallel {
					payload.GraphDefinition = businessParallelGraph(payload.Bundles[index].Agent.AgentID, payload.Bundles[index].Agent.AgentVersion)
					continue
				}
				payload.Bundles[index].FactoryKey = compiler.NewStandardFrozenDescriptor().Key()
				payload.Bundles[index].Agent.FactoryInput = json.RawMessage(`{}`)
			}
		}
		envelope.Payload, err = frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
		if err != nil {
			t.Fatal(err)
		}
		envelope.ContentHash, err = frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{WorkspaceID: envelope.WorkspaceID, WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion, ArtifactSchemaVersion: envelope.ArtifactSchemaVersion, CanonicalizationAlgorithm: envelope.CanonicalizationAlgorithm, CanonicalizationVersion: envelope.CanonicalizationVersion, HashAlgorithm: envelope.HashAlgorithm, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		before := string(envelope.Payload)
		loader := &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}
		_, err = loader.Load(t.Context(), envelope, workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
			t.Fatal("legacy business artifact reached host construction")
			return compiler.FrozenBuildOpts{}, nil, nil
		}), memberIntegrationSecrets{})
		expected := error(compiler.ErrFactoryCompileFailed)
		if parallel {
			expected = workflow.ErrRuntimeHostUnsupported
		}
		if !errors.Is(err, expected) || string(envelope.Payload) != before {
			t.Fatalf("legacy artifact was upgraded or not rejected: %v", err)
		}
	}
}
