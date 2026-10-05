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
	"github.com/jinyitao123/weave/internal/base/deliverable"
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
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const publishedBusinessCapability = "forge:action:sales_quote.AdjustPrice"
const publishedLeadConversionCapability = "forge:action:forge_sales_lead.sales_lead_convert_to_opportunity"

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

type leadBusinessActionOnceModel struct {
	calls                 int
	called                bool
	retried               bool
	firstArgs             string
	retryArgs             string
	amountRequiredVisible bool
}

func (m *leadBusinessActionOnceModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls++
	sawToolResult := false
	for _, message := range request.Messages {
		if message.Role == "tool" {
			sawToolResult = true
			if strings.Contains(message.Content, "business_receipt") {
				return &contract.ChatResponse{Content: "The authorized business receipt was verified."}, nil
			}
		}
	}
	if sawToolResult && m.retryArgs != "" && !m.retried {
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "forge_forge_sales_lead_sales_lead_convert_to_opportunity_") {
				m.retried = true
				return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "abcdef0123456789abcdef0123456789", Name: tool.Name, Args: m.retryArgs}}}, nil
			}
		}
		return nil, errors.New("published lead business capability was not offered for correction")
	}
	if !m.called {
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "forge_forge_sales_lead_sales_lead_convert_to_opportunity_") {
				var schema map[string]any
				if json.Unmarshal(tool.InputSchema, &schema) == nil {
					properties, _ := schema["properties"].(map[string]any)
					params, _ := properties["params"].(map[string]any)
					required, _ := params["required"].([]any)
					for _, name := range required {
						if name == "amount" {
							m.amountRequiredVisible = true
						}
					}
				}
				m.called = true
				return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "0123456789abcdef0123456789abcdef", Name: tool.Name, Args: m.firstArgs}}}, nil
			}
		}
		return nil, errors.New("published lead business capability was not offered")
	}
	return &contract.ChatResponse{Content: `{"disposition":"complete","summary":"The final lead summary is ready.","missing_items":[]}`}, nil
}

func (*leadBusinessActionOnceModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

type leadDispatchScenario struct {
	name                   string
	firstArgs              string
	retryArgs              string
	amountRequired         bool
	wantLeadCalls          int
	wantForgeCalls         int32
	wantEffects            int32
	wantJournalToolRecords int
	wantOutcomeStatus      string
	wantRunStatus          teamrun.Status
	reservedCallID         string
}

// The publication selector, frozen loader and workflow MemberRunner are real.
// The first lead calls Forge before a v1 read-only worker runs, matching the
// production bundle order. Only inference and Forge HTTP are fixtures; no test
// injects an operation ID or installs a journal by hand.
func TestBusinessLeadDispatchUsesDurableProvenanceBesideLegacyWorkerRealPG(t *testing.T) {
	for _, scenario := range []leadDispatchScenario{
		// The declared receipt check turns a failed authorized action into a failed
		// run instead of a successful summary; the action is still never replayed.
		{name: "empty args become an object before Forge MCP validation", firstArgs: `{}`, wantLeadCalls: 3, wantForgeCalls: 1, wantOutcomeStatus: "failed", wantRunStatus: teamrun.StatusFailed, wantJournalToolRecords: 1, reservedCallID: "0123456789abcdef0123456789abcdef"},
		{name: "missing required amount is journaled then corrected before one Forge dispatch", firstArgs: `{}`, retryArgs: `{"params":{"amount":"2300"}}`, amountRequired: true, wantLeadCalls: 4, wantForgeCalls: 1, wantEffects: 1, wantOutcomeStatus: "succeeded", wantRunStatus: teamrun.StatusSucceeded, wantJournalToolRecords: 2, reservedCallID: "abcdef0123456789abcdef0123456789"},
	} {
		t.Run(scenario.name, func(t *testing.T) { runBusinessLeadDispatchScenario(t, scenario) })
	}
}

func runBusinessLeadDispatchScenario(t *testing.T, scenario leadDispatchScenario) {
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
	var forgeRunActionCalls atomic.Int32
	var grant businessaction.TaskDelegationGrant
	var runID string
	amountRequired := scenario.amountRequired
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
		if r.URL.Path == businessaction.TaskDelegationPath+"/objects/forge_sales_lead" {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"type":"object","name":"forge_sales_lead","item":{"name":"forge_sales_lead","actions":[{"name":"sales_lead_convert_to_opportunity","params":[{"name":"amount","type":"string","required":%t},{"name":"expected_close_on","type":"string","required":false}]}]}}`, amountRequired))
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
			content := fmt.Sprintf(`{"actions":[{"name":"sales_lead_convert_to_opportunity","objectName":"forge_sales_lead","label":"Convert lead to opportunity","requiresRecord":true,"params":[{"name":"amount","type":"string","required":%t},{"name":"expected_close_on","type":"string","required":false}]}]}`, amountRequired)
			if request.Params.Name == "run_action" {
				forgeRunActionCalls.Add(1)
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
				wantAmount := ""
				if scenario.wantEffects == 1 {
					wantAmount = "2300"
				}
				if action.Action != "sales_lead_convert_to_opportunity" || action.Object != "forge_sales_lead" || action.Record != "lead-1" || action.Params["amount"] != wantAmount {
					t.Error("lead conversion request differed from the frozen tool-call fixture")
					return
				}
				var journalCount, completedTools, reservedCount int
				if err := pool.QueryRow(r.Context(), `SELECT count(*),count(*) FILTER (WHERE convert_from(value,'UTF8')::jsonb ? 'response'),(SELECT count(*) FROM weave_team_run_activity_events WHERE workspace_id='ws' AND run_id=$1 AND kind='business_action_started' AND detail->>'tool_call_id'=$2 AND COALESCE(detail->>'operation_id','')<>'') FROM loom_store WHERE namespace='member-operation:ws' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'`, runID, scenario.reservedCallID).Scan(&journalCount, &completedTools, &reservedCount); err != nil || journalCount != scenario.wantJournalToolRecords || completedTools != scenario.wantJournalToolRecords-1 || reservedCount != 1 {
					t.Errorf("Forge dispatch lacked journal intent and action reservation: journal=%d completed=%d reserved=%d err=%v", journalCount, completedTools, reservedCount, err)
					return
				}
				if wantAmount == "" {
					content = `{"ok":false,"error":"amount is required"}`
				} else {
					effects.Add(1)
					content = `{"ok":true,"data":{"business_receipt":"receipt-one"}}`
				}
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
	leadBundle, workerBundle, _, saved := publishConfiguredMemberIntegrationSampleForGraph(t, pool, key, "ws", "user", forge.URL, "", 0, "", "Read the request and report the result.",
		func(record *registry.AgentRecord) {
			record.BusinessCapabilityIDs = []string{publishedLeadConversionCapability}
		},
		func(record *registry.AgentRecord) { record.MCPServers = nil },
		func(lead, worker *registry.AgentRecord) json.RawMessage {
			return withLeadConversionReceiptCheck(t, json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"understand","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"understand","type":"lead","config":{"instruction":"Understand and handle the authorized action."},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"work","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Perform read-only middle-step analysis"},"inputs":{"task":{"value":{"source":"node_output","node_id":"understand","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"summarize","type":"lead","config":{"instruction":"Summarize the completed work."},"inputs":{"task":{"value":{"source":"node_output","node_id":"work","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"summarize","path":""}}}],"edges":[{"id":"a","from_node_id":"understand","to_node_id":"work","route":"success"},{"id":"b","from_node_id":"work","to_node_id":"summarize","route":"success"},{"id":"c","from_node_id":"summarize","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version)))
		},
	)
	if leadBundle.FactoryKey != compiler.StandardFrozenToolsKey() || len(leadBundle.Agent.BusinessCapabilityIDs) != 1 || workerBundle.FactoryKey != compiler.NewStandardFrozenDescriptor().Key() || len(workerBundle.Agent.BusinessCapabilityIDs) != 0 {
		t.Fatalf("mixed business/read-only publication contract changed: lead=%+v worker=%+v", leadBundle.FactoryKey, workerBundle.FactoryKey)
	}
	artifacts := workflow.NewArtifactStore(pool, nil)
	tasks := taskqueue.New(pool, nil, time.Minute)
	snapshots := snapshot.NewStore(pool)
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, OrgStore: orgstore.NewStore(pool), Registry: agentcatalog.New(pool), Workflow: workflowcatalog.New(pool, nil, artifacts), WorkflowArtifacts: artifacts, Deliverables: deliveryverify.NewStore(pool), ScheduleTransactions: pool, Snapshots: snapshots, Tasks: tasks, Config: &config.Config{ForgeSessionURL: forge.URL + "/api/v1/auth/me"}}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, teamconstruction.NewPublicationAuthority(pool, nil))
	request := dispatchInputRegistrationFixture("business-session", "Convert the authorized lead", "")
	version := 1
	request.WorkflowID, request.WorkflowVersion = "flow", &version
	request.AuthorizedBusinessCapabilityIDs = &[]string{publishedLeadConversionCapability}
	request.BusinessRecord = &dispatchBusinessRecord{ObjectName: "forge_sales_lead", RecordID: "lead-1"}
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
	runID = dispatched.RunID
	members, err := loomruntime.NewMemberRunner(storeext.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	checkpoints := teamrun.NewPGCheckpointStore()
	activities := &teamrun.PGActivityStore{Transactions: pool}
	leadModel, workerModel := &leadBusinessActionOnceModel{firstArgs: scenario.firstArgs, retryArgs: scenario.retryArgs}, &memberIntegrationModel{}
	host := workflow.RuntimeHostFactoryFunc(func(_ context.Context, candidate frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		if candidate.Agent.AgentID == leadBundle.Agent.AgentID {
			return compiler.FrozenBuildOpts{LLM: leadModel, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
		}
		return compiler.FrozenBuildOpts{LLM: workerModel, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
	})
	runtime := &teamrun.WorkflowSerialRuntime{BusinessReceiptReader: deliveryverify.BusinessReceiptReader(pool), OutputRecorder: deliveryverify.NewStore(pool), Members: members, Artifacts: artifacts, Loader: &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}, HostFactory: businessaction.Factory{Inner: host, Store: businessaction.NewStore(pool, tasks, key)}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: snapshots, Activities: activities}
	executor := &teamrun.Executor{Tasks: tasks, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: snapshots, Runs: runs, Tasks: tasks}}
	if ok, err := executor.ProcessNext(ctx, "business-worker"); err != nil || !ok {
		t.Fatalf("execute=%v err=%v", ok, err)
	}
	finished, err := runs.Get(ctx, "ws", dispatched.RunID)
	if err != nil || finished.Status != scenario.wantRunStatus || effects.Load() != scenario.wantEffects || forgeRunActionCalls.Load() != scenario.wantForgeCalls || leadModel.calls != scenario.wantLeadCalls || leadModel.amountRequiredVisible != scenario.amountRequired || workerModel.calls != 1 {
		t.Fatalf("run status=%s effects=%d Forge calls=%d lead model=%d worker model=%d amount-required-visible=%v err=%v", finished.Status, effects.Load(), forgeRunActionCalls.Load(), leadModel.calls, workerModel.calls, leadModel.amountRequiredVisible, err)
	}
	events, err := activities.ListBusinessActionEvents(ctx, "ws", dispatched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "business_action_started" || events[1].Kind != "business_action_result" {
		t.Fatalf("lead action did not persist a start/result pair: events=%+v", events)
	}
	var started, result businessaction.ActionOutcomeEvent
	if json.Unmarshal(events[0].Detail, &started) != nil || json.Unmarshal(events[1].Detail, &result) != nil ||
		started.Phase != "started" || result.Phase != "result" ||
		started.InvocationID == "" || started.OperationSlot == "" || started.OperationID == "" ||
		started.InvocationID != result.InvocationID || started.OperationSlot != result.OperationSlot || started.OperationID != result.OperationID {
		t.Fatalf("lead action lost invocation or durable-slot provenance: started=%+v result=%+v", started, result)
	}
	outcomes, err := teamrun.ProjectBusinessActionOutcomes(events)
	if err != nil || len(outcomes) != 1 || outcomes[0].Status != scenario.wantOutcomeStatus {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
	var memberCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_workflow_member_runs WHERE workspace_id='ws' AND parent_run_id=$1`, dispatched.RunID).Scan(&memberCount); err != nil || memberCount != 2 {
		t.Fatalf("member count=%d err=%v", memberCount, err)
	}
	var toolOperations, completedToolOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE convert_from(value,'UTF8')::jsonb ? 'response') FROM loom_store WHERE namespace='member-operation:ws' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'`).Scan(&toolOperations, &completedToolOperations); err != nil || toolOperations != scenario.wantJournalToolRecords || completedToolOperations != scenario.wantJournalToolRecords {
		t.Fatalf("member journal did not preserve every tool response: tool operations=%d completed=%d err=%v", toolOperations, completedToolOperations, err)
	}
	assertUnsupportedBusinessArtifactsRejectedBeforeHosts(t, saved)
	flows := workflowcatalog.New(pool, nil, artifacts)
	draft, err := flows.CreateDraft(ctx, "ws", "flow", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = flows.UpdateDraft(ctx, "ws", "flow", draft.Version, draft.UpdatedAt, workflow.DraftInput{CreatedBy: "user", TriggerConfig: draft.TriggerConfig, GraphDefinition: businessParallelGraph(leadBundle.Agent.AgentID, leadBundle.Agent.AgentVersion)})
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

// withLeadConversionReceiptCheck makes the final summary a workbench result and
// declares the authorized conversion as a required receipt, which authorized
// dispatch now demands of any graph whose members can execute the action.
func withLeadConversionReceiptCheck(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var graph map[string]json.RawMessage
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()})
	graph["output_contract"] = output
	graph["result_protocol"], _ = json.Marshal(machine.ResultProtocolWorkbenchV1)
	var nodes []map[string]json.RawMessage
	if err := json.Unmarshal(graph["nodes"], &nodes); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if string(node["id"]) == `"summarize"` {
			node["output"] = output
		}
	}
	graph["nodes"], _ = json.Marshal(nodes)
	params, _ := json.Marshal(deliverycheck.BusinessReceiptParameters{RequiredCapabilityIDs: []string{publishedLeadConversionCapability}, WhenAuthorized: true, AllowNeedsInput: true})
	graph["delivery_contract"], _ = json.Marshal(deliverable.DeliveryContract{Version: 1, Coverage: deliverable.CoverageExplicit, Output: deliverable.OutputRequirement{Type: "json", Schema: machine.WorkbenchResultSchemaV1()},
		RequiredChecks:  []deliverable.CheckSpec{{ID: "business-effect", Title: "Required business receipt", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: deliverycheck.BusinessReceiptsVersion, Parameters: params}},
		ExternalEffects: deliverable.ExternalEffectsRequired, ExternalEffectsCheckID: "business-effect"})
	encoded, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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
