package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
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
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

const trialEvidenceCapability = "forge:action:test_object.RecordCheck"

type trialEvidenceModel struct{ large bool }

func (m trialEvidenceModel) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	for _, message := range request.Messages {
		if message.Role == "tool" {
			return &contract.ChatResponse{Content: "Finished this node", Usage: contract.Usage{InputTokens: 5, OutputTokens: 2}}, nil
		}
	}
	value := execution.NodeID(ctx)
	if m.large {
		value += strings.Repeat("测", loomruntime.MemberToolEvidenceMaxBytes)
	}
	args, _ := json.Marshal(map[string]any{"recordId": "record-" + execution.NodeID(ctx), "params": map[string]string{"value": value}})
	for _, tool := range request.Tools {
		if strings.HasPrefix(tool.Name, "forge_test_object_recordcheck_") {
			return &contract.ChatResponse{Content: "Private model rationale", ToolCalls: []contract.ToolCall{{ID: "same-call", Name: tool.Name, Args: string(args)}}, Usage: contract.Usage{InputTokens: 5, OutputTokens: 2}}, nil
		}
	}
	return nil, errors.New("trial business dispatcher did not advertise its frozen tool")
}

func (trialEvidenceModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

type trialEvidenceFixture struct {
	pool             *pgxpool.Pool
	server           *Server
	ctx              context.Context
	run              teamrun.TeamRun
	workspace, actor string
}

// The trial API, frozen loader, simulated business Dispatcher, MemberRunner,
// journal, PG activity store and activity API are real. Only inference is
// scripted. No activity events or journal receipts are injected for execution.
func runDevelopmentToolEvidence(t *testing.T, large bool) *trialEvidenceFixture {
	t.Helper()
	const workspace, actor = "tool-evidence-workspace", "developer"
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: workspace, UserID: actor})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Trial tools');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES($2,$1,$2,'unused','admin')`, workspace, actor); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "trial-lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Call the selected simulated action once, then finish"}}
	worker := registry.AgentRecord{Name: "trial-worker", DisplayName: "只读成员", Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Read only"}}
	for _, record := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, workspace, record); err != nil {
			t.Fatal(err)
		}
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, workspace, org.CreateActiveTeamInput{Name: "trial-tools", Objective: "Verify tool evidence", LeadAvatarID: lead.ID, Workers: []org.InitialTeamWorker{{WorkerAgentID: worker.ID, Duty: "Read only", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}}})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	providerActor := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: workspace, ServiceID: "provider:fixture"})
	if err := providers.Upsert(providerActor, workspace, llmrouter.ProviderConfig{CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "provider:fixture", ID: "fixture", Name: "Fixture", BaseURL: "http://127.0.0.1:1", APIKey: "test-only", Models: []string{"fixture-model"}}); err != nil {
		t.Fatal(err)
	}
	artifacts := workflow.NewArtifactStore(pool, nil)
	flows := workflowcatalog.New(pool, nil, artifacts)
	authority := teamconstruction.NewPublicationAuthority(pool, workflowcatalog.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), memberIntegrationDescriptors(t)))
	tasks := taskqueue.New(pool, nil, time.Minute)
	server := &Server{Pool: pool, Store: teamDeliveryPoolStore{teamDispatchPoolStore{pool: pool}}, StoreExt: storeext.New(pool), Registry: agents, OrgStore: orgstore.NewStore(pool), PublicationAuthority: authority, Workflow: flows, WorkflowArtifacts: artifacts, Tasks: tasks, Snapshots: snapshot.NewStore(pool), Deliverables: deliveryverify.NewStore(pool)}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, authority)
	call := func(method string, body any, handler echo.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		c := echo.New().NewContext(request, response)
		c.SetParamNames("id")
		c.SetParamValues(created.Team.ID)
		c.Set("tenant", workspace)
		c.Set("user_id", actor)
		c.Set("roles", []string{"developer"})
		if err := handler(c); err != nil {
			t.Fatalf("development request failed: %v", err)
		}
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("development request status=%d body=%s", response.Code, response.Body.String())
		}
		return response
	}
	var draft developmentDraft
	if err := json.Unmarshal(call(http.MethodGet, nil, server.handleGetTeamDevelopment).Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	for index := range draft.Document.Members {
		draft.Document.Members[index].Relationship.Duty = "Verify the assigned trial step"
		draft.Document.Members[index].Configuration.SystemPrompt = "Call only the selected simulated action and return its observed result"
		if draft.Document.Members[index].Configuration.Role == "avatar" {
			draft.Document.Members[index].Configuration.BusinessCapabilityIDs = []string{trialEvidenceCapability}
		}
	}
	flowID := uuid.NewString()
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"node-A","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"node-A","label":"First call","type":"lead","config":{"instruction":"Call for A"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"node-B","label":"Second call","type":"lead","config":{"instruction":"Call for B"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"node-B","path":""}}}],"edges":[{"id":"a","from_node_id":"node-A","to_node_id":"node-B","route":"success"},{"id":"b","from_node_id":"node-B","to_node_id":"deliver","route":"success"}]}`)
	draft.Document.Workflows = []developmentWorkflow{{ID: flowID, Name: "Tool evidence", Graph: graph, Trigger: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)}}
	call(http.MethodPut, map[string]any{"expected_revision": draft.Revision, "document": draft.Document}, server.handleSaveTeamDevelopment)
	var actions []businessaction.DevelopmentAction
	if err := json.Unmarshal([]byte(`[{"capability_id":"forge:action:test_object.RecordCheck","name":"RecordCheck","object_name":"test_object","requires_record":true,"params":[{"name":"value","type":"string","required":true}]}]`), &actions); err != nil {
		t.Fatal(err)
	}
	trial := developmentTrialRequest{Revision: draft.Revision + 1, WorkflowID: flowID, RequestID: uuid.NewString(), Input: "Check both steps with their exact values", BusinessActions: actions}
	var receipt struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(call(http.MethodPost, trial, server.handleTrialTeamDevelopment).Body.Bytes(), &receipt); err != nil {
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
	host := workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		return compiler.FrozenBuildOpts{LLM: trialEvidenceModel{large: large}, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
	})
	runtime := &teamrun.WorkflowSerialRuntime{OutputRecorder: &developmentTrialWorkflowOutputRecorder{pool: pool, fallback: server.Deliverables}, Members: members, Artifacts: artifacts, Loader: &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}, HostFactory: businessaction.Factory{Inner: host, Store: businessaction.NewStore(pool, tasks, key)}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: server.Snapshots, Activities: activities}
	executor := &teamrun.Executor{Tasks: tasks, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: server.Snapshots, Runs: runs, Tasks: tasks}}
	if worked, err := executor.ProcessNext(ctx, "trial-evidence-worker"); err != nil || !worked {
		t.Fatalf("execute=%v err=%v", worked, err)
	}
	run, err := runs.Get(ctx, workspace, receipt.RunID)
	if err != nil || run.Status != teamrun.StatusSucceeded {
		t.Fatalf("trial run=%+v err=%v", run, err)
	}
	server.teamRunCancel = &teamrun.CancelService{Transactions: pool, Runs: runs}
	server.teamRunActivities = activities
	return &trialEvidenceFixture{pool: pool, server: server, ctx: ctx, run: run, workspace: workspace, actor: actor}
}

func (fixture *trialEvidenceFixture) activity(t *testing.T, actor string, role ...string) (int, map[string]string, []runActivityMember, string) {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+fixture.run.RunID+"/activity", nil).WithContext(fixture.ctx)
	c := echo.New().NewContext(request, response)
	c.SetParamNames("id")
	c.SetParamValues(fixture.run.RunID)
	c.Set("tenant", fixture.workspace)
	c.Set("user_id", actor)
	if len(role) == 0 {
		role = []string{"developer"}
	}
	c.Set("roles", role)
	if err := fixture.server.handleGetRunActivity(c); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Completeness map[string]string   `json:"completeness"`
		Members      []runActivityMember `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return response.Code, result.Completeness, result.Members, response.Body.String()
}

func TestDevelopmentTrialToolJournalEvidenceRealPG(t *testing.T) {
	fixture := runDevelopmentToolEvidence(t, false)
	status, completeness, members, body := fixture.activity(t, fixture.actor)
	if status != http.StatusOK || completeness["member_tool_activity"] != "complete" || completeness["member_tool_payloads"] != "complete" {
		t.Fatalf("activity=%d completeness=%v body=%s", status, completeness, body)
	}
	found := 0
	for _, member := range members {
		for _, stage := range member.Stages {
			if stage.NodeID != "node-A" && stage.NodeID != "node-B" {
				continue
			}
			found++
			if len(stage.Tools) != 1 {
				t.Fatalf("node %s tools=%+v", stage.NodeID, stage.Tools)
			}
			tool := stage.Tools[0]
			var input struct {
				Params map[string]string `json:"params"`
			}
			var output struct {
				Params    map[string]string `json:"params"`
				Simulated bool              `json:"simulated"`
			}
			if json.Unmarshal([]byte(tool.Input), &input) != nil || json.Unmarshal([]byte(tool.Output), &output) != nil || input.Params["value"] != stage.NodeID || output.Params["value"] != stage.NodeID || !output.Simulated || tool.CallID != "same-call" || tool.Status != "ok" || tool.InputState != "recorded" || tool.OutputState != "recorded" {
				t.Fatalf("node %s evidence=%+v", stage.NodeID, tool)
			}
		}
	}
	if found != 2 || strings.Contains(body, "Private model rationale") {
		t.Fatal("node or tool-only evidence boundary failed")
	}
	var copied, formal int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FILTER(WHERE kind IN('tool_started','tool_completed') AND (detail ? 'input' OR detail ? 'output')),count(*) FILTER(WHERE kind IN('business_action_started','business_action_result')) FROM weave_team_run_activity_events WHERE workspace_id=$1 AND run_id=$2`, fixture.workspace, fixture.run.RunID).Scan(&copied, &formal); err != nil || copied != 0 || formal != 0 {
		t.Fatalf("trial leaked payload or formal business facts: copied=%d formal=%d err=%v", copied, formal, err)
	}
	var key string
	var savedReceipt []byte
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT j.key,j.value FROM weave_workflow_member_runs m
		JOIN loom_store j ON j.namespace='member-operation:' || m.workspace_id AND starts_with(j.key,m.member_run_id || '/')
		WHERE m.workspace_id=$1 AND m.parent_run_id=$2 AND m.node_id='node-A' AND convert_from(j.value,'UTF8')::jsonb->>'kind'='tool'`, fixture.workspace, fixture.run.RunID).Scan(&key, &savedReceipt); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE loom_store SET value=convert_to((convert_from(value,'UTF8')::jsonb-'response')::text,'UTF8') WHERE namespace='member-operation:' || $1 AND key=$2`, fixture.workspace, key); err != nil {
		t.Fatal(err)
	}
	status, completeness, members, _ = fixture.activity(t, fixture.actor)
	if status != http.StatusOK || completeness["member_tool_activity"] != "complete" || completeness["member_tool_payloads"] != "partial" {
		t.Fatal("a missing receipt changed lifecycle or became complete payload evidence")
	}
	for _, member := range members {
		for _, stage := range member.Stages {
			if stage.NodeID == "node-A" && (stage.Tools[0].InputState != "recorded" || stage.Tools[0].OutputState != "missing" || stage.Tools[0].Output != "") {
				t.Fatal("missing receipt was inferred from model text or lifecycle status")
			}
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE loom_store SET value=$3 WHERE namespace='member-operation:' || $1 AND key=$2`, fixture.workspace, key, savedReceipt); err != nil {
		t.Fatal(err)
	}
	// A legitimate empty ToolResult.Content must remain an explicitly recorded
	// output on the actual endpoint wire, not vanish through string omitempty.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE loom_store SET value=convert_to(jsonb_set(convert_from(value,'UTF8')::jsonb,'{response,content}','""'::jsonb)::text,'UTF8') WHERE namespace='member-operation:' || $1 AND key=$2`, fixture.workspace, key); err != nil {
		t.Fatal(err)
	}
	status, completeness, _, body = fixture.activity(t, fixture.actor)
	var wire struct {
		Members []struct {
			Stages []struct {
				NodeID string                       `json:"node_id"`
				Tools  []map[string]json.RawMessage `json:"tools"`
			} `json:"stages"`
		} `json:"members"`
	}
	if status != http.StatusOK || completeness["member_tool_payloads"] != "complete" || json.Unmarshal([]byte(body), &wire) != nil {
		t.Fatal("empty receipt endpoint could not be read")
	}
	emptyObserved := false
	for _, member := range wire.Members {
		for _, stage := range member.Stages {
			if stage.NodeID == "node-A" {
				output, present := stage.Tools[0]["output"]
				originalBytes, bytesPresent := stage.Tools[0]["output_bytes"]
				if !present || string(output) != `""` || !bytesPresent || string(originalBytes) != "0" || string(stage.Tools[0]["output_state"]) != `"recorded"` {
					t.Fatalf("recorded empty output was omitted: %s", body)
				}
				emptyObserved = true
			}
		}
	}
	if !emptyObserved {
		t.Fatal("empty receipt stage was not present")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE loom_store SET value=$3 WHERE namespace='member-operation:' || $1 AND key=$2`, fixture.workspace, key, savedReceipt); err != nil {
		t.Fatal(err)
	}
	if status, _, _, body := fixture.activity(t, "another-developer"); status != http.StatusNotFound || strings.Contains(body, "record-node") {
		t.Fatal("another developer received trial tool evidence")
	}
	if status, _, _, body := fixture.activity(t, fixture.actor, "member"); status != http.StatusForbidden || strings.Contains(body, "record-node") {
		t.Fatal("the trial actor retained raw evidence after losing developer access")
	}
	for _, role := range []string{"admin", "owner"} {
		if status, _, _, _ := fixture.activity(t, fixture.actor, role); status != http.StatusOK {
			t.Fatalf("authorized %s could not read owned trial evidence", role)
		}
	}
	// Removing the server-owned trial association must remove journal payload
	// projection, even though exactly the same private journal still exists.
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE weave_task_queue SET context_key='ordinary-run' WHERE workspace_id=$1 AND id=$2`, fixture.workspace, fixture.run.SourceTaskID); err != nil {
		t.Fatal(err)
	}
	status, completeness, members, _ = fixture.activity(t, fixture.actor, "member")
	if status != http.StatusOK || completeness["member_tool_payloads"] != "" {
		t.Fatal("ordinary activity invoked development journal projection")
	}
	for _, member := range members {
		for _, stage := range member.Stages {
			for _, tool := range stage.Tools {
				if tool.Input != "" || tool.Output != "" || tool.InputState != "" || tool.OutputState != "" {
					t.Fatal("ordinary activity exposed private journal payload")
				}
			}
		}
	}
}

func TestDevelopmentTrialToolJournalTruncationRealPG(t *testing.T) {
	fixture := runDevelopmentToolEvidence(t, true)
	status, completeness, members, _ := fixture.activity(t, fixture.actor)
	if status != http.StatusOK || completeness["member_tool_activity"] != "complete" || completeness["member_tool_payloads"] != "partial" {
		t.Fatalf("bounded trial activity=%d completeness=%v", status, completeness)
	}
	for _, member := range members {
		for _, stage := range member.Stages {
			for _, tool := range stage.Tools {
				if tool.InputState != "truncated" || tool.OutputState != "truncated" || len(tool.Input) > loomruntime.MemberToolEvidenceMaxBytes || len(tool.Output) > loomruntime.MemberToolEvidenceMaxBytes || tool.InputBytes <= len(tool.Input) || tool.OutputBytes <= len(tool.Output) {
					t.Fatalf("bounded tool=%+v", tool)
				}
			}
		}
	}
}

func TestDevelopmentTrialToolJournalDoesNotGuessLatestInvocationRealPG(t *testing.T) {
	fixture := runDevelopmentToolEvidence(t, false)
	var original string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT member_run_id FROM weave_workflow_member_runs WHERE workspace_id=$1 AND parent_run_id=$2 AND node_id='node-A'`, fixture.workspace, fixture.run.RunID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	newMember := uuid.NewString()
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO weave_workflow_member_runs(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state,checkpoint_seq,result)
		SELECT workspace_id,parent_run_id,$3,'later-invocation',run_snapshot_id,node_id,parent_generation,identity_hash,initial_state,checkpoint_seq,result FROM weave_workflow_member_runs WHERE workspace_id=$1 AND member_run_id=$2`, fixture.workspace, original, newMember); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO loom_store(namespace,key,value) SELECT namespace,$3 || substring(key FROM length($2)+1),value FROM loom_store WHERE namespace='member-operation:' || $1 AND starts_with(key,$2 || '/')`, fixture.workspace, original, newMember); err != nil {
		t.Fatal(err)
	}
	status, completeness, members, _ := fixture.activity(t, fixture.actor)
	if status != http.StatusOK || completeness["member_tool_payloads"] != "partial" {
		t.Fatal("multiple node invocations were called complete")
	}
	for _, member := range members {
		for _, stage := range member.Stages {
			if stage.NodeID == "node-A" && (stage.Tools[0].InputState != "missing" || stage.Tools[0].OutputState != "missing") {
				t.Fatal("reused call ID was guessed across node invocations")
			}
		}
	}
	// More than the run-wide receipt cap remains explicitly partial.
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO loom_store(namespace,key,value)
		SELECT j.namespace,$3 || '/limit/' || lpad(n::text,12,'0'),j.value FROM loom_store j CROSS JOIN generate_series(1,$4) n
		WHERE j.namespace='member-operation:' || $1 AND starts_with(j.key,$2 || '/') AND convert_from(j.value,'UTF8')::jsonb->>'kind'='tool'`, fixture.workspace, original, newMember, loomruntime.MemberToolEvidenceMaxCount); err != nil {
		t.Fatal(err)
	}
	report, err := loomruntime.ReadMemberToolEvidence(fixture.ctx, fixture.pool, fixture.workspace, fixture.run.RunID, fixture.run.RunSnapshotID)
	if err != nil || !report.Partial || len(report.Tools) != loomruntime.MemberToolEvidenceMaxCount {
		t.Fatalf("count cap report: count=%d partial=%v err=%v", len(report.Tools), report.Partial, err)
	}
}
