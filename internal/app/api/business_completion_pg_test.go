package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type receiptCompletionModel struct {
	calls            int
	outcome          string
	feedbackObserved bool
}

func (m *receiptCompletionModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls++
	complete := `{"disposition":"complete","summary":"The requested action was submitted.","missing_items":[]}`
	if m.outcome == "not-authorized" || m.outcome == "trial" {
		return &contract.ChatResponse{Content: `{"disposition":"complete","summary":"Read-only review finished.","missing_items":[]}`}, nil
	}
	if m.outcome == "needs-input" {
		return &contract.ChatResponse{Content: `{"disposition":"needs_input","summary":"Required source is missing.","missing_items":["source data"]}`}, nil
	}
	if m.calls == 1 {
		return &contract.ChatResponse{Content: complete}, nil
	}
	if m.calls == 2 {
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "user" || !strings.Contains(last.Content, "No matching call is recorded") {
			return nil, errors.New("missing completion feedback did not reach the model")
		}
		m.feedbackObserved = true
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "forge_sales_quote_adjustprice_") {
				calls := []contract.ToolCall{{ID: "model-call", Name: tool.Name, Args: `{"params":{"line_id":"line-a"}}`}}
				if strings.HasSuffix(m.outcome, "-batch") {
					calls = append(calls, contract.ToolCall{ID: "second-model-call", Name: tool.Name, Args: `{"params":{"line_id":"line-a"}}`})
				}
				return &contract.ChatResponse{Content: complete, ToolCalls: calls}, nil
			}
		}
		return nil, errors.New("authorized business tool was not advertised")
	}
	if m.calls == 3 && m.outcome == "failed-retry" {
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "forge_sales_quote_adjustprice_") {
				return &contract.ChatResponse{Content: complete, ToolCalls: []contract.ToolCall{{ID: "retry-model-call", Name: tool.Name, Args: `{"params":{"line_id":"line-a"}}`}}}, nil
			}
		}
		return nil, errors.New("retry test lost the advertised tool")
	}
	if m.calls == 3 || m.calls == 4 && m.outcome == "failed-retry" {
		return &contract.ChatResponse{Content: complete}, nil
	}
	return nil, errors.New("completion re-prompted after an authoritative action result")
}
func (*receiptCompletionModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

func TestBusinessReceiptCompletionPublishedRuntimeRealPG(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "unknown", "failed-retry", "failed-batch", "unknown-batch", "not-authorized", "needs-input", "trial"} {
		t.Run(outcome, func(t *testing.T) { runBusinessReceiptCompletion(t, outcome) })
	}
}

// The publication selector, frozen loader and workflow MemberRunner are real.
// Only inference and the external Forge endpoint are fixtures; no test injects
// an operation ID or installs a journal by hand.
func runBusinessReceiptCompletion(t *testing.T, outcome string) {
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
	resultStatus := strings.SplitN(outcome, "-", 2)[0]
	var effects, forgeCalls, forgeRequests atomic.Int32
	var grant businessaction.TaskDelegationGrant
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forgeRequests.Add(1)
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
				if err := pool.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM loom_store WHERE namespace='member-operation:ws' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'), (SELECT count(*) FROM weave_team_run_activity_events WHERE kind='business_action_started' AND detail->>'operation_id'=$1)`, action.Params["idempotency_key"]).Scan(&journalCount, &reservedCount); err != nil || journalCount != int(forgeCalls.Load())+1 || reservedCount != 1 {
					t.Errorf("side effect preceded durable intent and action reservation: journal=%d reserved=%d err=%v", journalCount, reservedCount, err)
					return
				}
				forgeCalls.Add(1)
				if resultStatus != "failed" {
					effects.Add(1)
				}
				content = `{"ok":true,"data":{"business_receipt":"receipt-one"}}`
				if resultStatus == "failed" {
					content = `{"ok":false,"error":"fixture rejected the operation"}`
				}
				if resultStatus == "unknown" {
					content = `{"ok":null,"error":false}`
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
	bundle, saved, envelope, authority := publishBusinessCompletionSample(t, pool, key, forge.URL)
	if bundle.FactoryKey != compiler.StandardFrozenToolsKey() || len(bundle.MCPBindings) != 0 || bundle.Agent.Limits.ToolLoopControl != nil {
		t.Fatal("business-only publication did not select the existing durable contract")
	}
	artifacts := workflow.NewArtifactStore(pool, nil)
	tasks := taskqueue.New(pool, nil, time.Minute)
	snapshots := snapshot.NewStore(pool)
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, OrgStore: orgstore.NewStore(pool), Registry: agentcatalog.New(pool), Workflow: workflowcatalog.New(pool, nil, artifacts), WorkflowArtifacts: artifacts, Deliverables: deliveryverify.NewStore(pool), ScheduleTransactions: pool, Snapshots: snapshots, Tasks: tasks, Config: &config.Config{ForgeSessionURL: forge.URL + "/api/v1/auth/me"}}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, authority)
	var dispatched workflowManualRunResponse
	var receipt dispatchInputReceipt
	var trial publication.CandidateRunRequest
	var trialAdmission publication.AdmissionReceipt
	var trialID string
	if outcome == "trial" {
		trialID = uuid.NewString()
		input, _ := json.Marshal("Read-only developer trial with quoted \"source\" and newline\ncontent")
		hash := sha256.Sum256(input)
		trial = publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "development:" + trialID, Candidate: envelope, Input: input, InputVersion: hex.EncodeToString(hash[:]), SourceRef: "team-development:team", Purpose: "developer-trial"}
		digest, err := trial.Fingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		actions := []businessaction.DevelopmentAction{{CapabilityID: publishedBusinessCapability, Name: "AdjustPrice", ObjectName: "sales_quote", Label: "Adjust price", RequiresRecord: true}}
		source, _ := json.Marshal(struct {
			RequestDigest string                             `json:"request_digest"`
			Actions       []businessaction.DevelopmentAction `json:"actions"`
		}{digest, actions})
		bindingDigest := sha256.Sum256(source)
		digest = hex.EncodeToString(bindingDigest[:])
		if _, err := pool.Exec(ctx, `INSERT INTO weave_team_development_trials(workspace_id,team_id,request_id,revision,workflow_id,actor_id,request_digest,request,business_actions) VALUES('ws','team',$1,1,'flow','user',$2,$3,$4)`, trialID, digest, encodeDevelopment(trial), encodeDevelopment(actions)); err != nil {
			t.Fatal(err)
		}
		admission, err := server.KernelPublication.AdmitCandidate(ctx, trial)
		if err != nil {
			t.Fatal(err)
		}
		if err := admission.Verify(ctx, trial); err != nil {
			t.Fatal(err)
		}
		trialAdmission = admission
		dispatched.RunID = admission.RunID
		// Deliberately leave the product receipt NULL: the worker may start
		// after atomic admission and before the API writes its display receipt.
	} else {
		request := dispatchInputRegistrationFixture("business-session", "Adjust the authorized record", "")
		version := saved.WorkflowVersion
		request.WorkflowID, request.WorkflowVersion = "flow", &version
		request.AuthorizedBusinessCapabilityIDs = &[]string{publishedBusinessCapability}
		if outcome == "not-authorized" {
			request.AuthorizedBusinessCapabilityIDs = &[]string{}
		}
		request.BusinessRecord = &dispatchBusinessRecord{ObjectName: "sales_quote", RecordID: "record-a"}
		grant = testTaskGrant(t, forge.URL, "native-user", "native-org", 1, scopeForRegistration(request))
		registered, inputReceipt := registerTaskInput(t, server, request, "fixture-task-token")
		receipt = inputReceipt
		if registered.Code != http.StatusCreated {
			t.Fatalf("registration=%d %s", registered.Code, registered.Body.String())
		}
		body, _ := json.Marshal(map[string]string{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID})
		c, response := dispatchInputTestContext(body, "/v1/teams/team/dispatch", "ws", "user")
		if err := server.handleDispatchTeam(c); err != nil || response.Code != http.StatusCreated {
			t.Fatalf("dispatch=%d %s err=%v", response.Code, response.Body.String(), err)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &dispatched); err != nil {
			t.Fatal(err)
		}

	}
	members, err := loomruntime.NewMemberRunner(storeext.New(pool))
	if err != nil {
		t.Fatal(err)
	}
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	checkpoints := teamrun.NewPGCheckpointStore()
	activities := &teamrun.PGActivityStore{Transactions: pool}
	model := &receiptCompletionModel{outcome: outcome}
	host := workflow.RuntimeHostFactoryFunc(func(_ context.Context, candidate frozen.FrozenExecutionBundle, _ workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		var llm contract.LLM = &memberIntegrationModel{}
		if candidate.Agent.AgentID == bundle.Agent.AgentID {
			llm = model
		}
		return compiler.FrozenBuildOpts{LLM: llm, Tools: &memberIntegrationExport{}}, io.NopCloser(strings.NewReader("")), nil
	})
	runtime := &teamrun.WorkflowSerialRuntime{BusinessReceiptReader: deliveryverify.BusinessReceiptReader(pool), OutputRecorder: deliveryverify.NewStore(pool), Members: members, Artifacts: artifacts, Loader: &workflow.RuntimeLoader{Registry: memberIntegrationDescriptors(t)}, HostFactory: businessaction.Factory{Inner: host, Store: businessaction.NewStore(pool, tasks, key)}, CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return memberIntegrationSecrets{}, nil }, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: snapshots, Activities: activities}
	executor := &teamrun.Executor{Tasks: tasks, Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime, Consumer: &teamrun.Consumer{Transactions: pool, Snapshots: snapshots, Runs: runs, Tasks: tasks}}
	if ok, err := executor.ProcessNext(ctx, "business-worker"); err != nil || !ok {
		t.Fatalf("execute=%v err=%v", ok, err)
	}
	finished, err := runs.Get(ctx, "ws", dispatched.RunID)
	wantStatus := teamrun.StatusSucceeded
	if resultStatus == "failed" || resultStatus == "unknown" {
		wantStatus = teamrun.StatusFailed
	}
	cause := ""
	if finished.CauseSummary != nil {
		cause = *finished.CauseSummary
	}
	if err != nil || finished.Status != wantStatus {
		t.Fatalf("status=%s want=%s cause=%v err=%v", finished.Status, wantStatus, cause, err)
	}
	if outcome == "trial" {
		candidate := deliverable.Candidate{WorkspaceID: "ws", RunID: trialAdmission.RunID, RunSnapshotID: trialAdmission.RunSnapshotID}
		read := deliveryverify.BusinessReceiptReader(pool)
		assertTrial := func() {
			t.Helper()
			frame, err := read(ctx, candidate)
			if err != nil || frame.Scope.InputRevisionID != trial.InputVersion || frame.Scope.SubjectID != "user" || frame.Scope.AllowedCapabilityIDs == nil || len(frame.Scope.AllowedCapabilityIDs) != 0 {
				t.Fatalf("trial frozen scope=%+v err=%v", frame.Scope, err)
			}
			check, err := deliverycheck.BusinessReceiptCheck(frame.Contract)
			if err != nil || check == nil {
				t.Fatalf("trial lost frozen delivery check: %v", err)
			}
		}
		assertTrial()
		if _, err := pool.Exec(ctx, `UPDATE weave_team_development_trials SET actor_id='another-actor' WHERE workspace_id='ws' AND request_id=$1`, trialID); err != nil {
			t.Fatal(err)
		}
		if _, err := read(ctx, candidate); err == nil {
			t.Fatal("trial borrowed another actor")
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_team_development_trials SET actor_id='user',request=jsonb_set(request,'{request_id}','"development:wrong-request"') WHERE workspace_id='ws' AND request_id=$1`, trialID); err != nil {
			t.Fatal(err)
		}
		if _, err := read(ctx, candidate); err == nil {
			t.Fatal("trial borrowed another request")
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_team_development_trials SET request=$2,receipt=$3 WHERE workspace_id='ws' AND request_id=$1`, trialID, encodeDevelopment(trial), encodeDevelopment(trialAdmission)); err != nil {
			t.Fatal(err)
		}
		assertTrial()
	}
	wantCalls, wantEffects, wantModels := int32(1), int32(1), 3
	if resultStatus == "unknown" || outcome == "failed-batch" {
		wantModels = 2
	}
	if resultStatus == "failed" {
		wantEffects = 0
	}
	if outcome == "not-authorized" || outcome == "needs-input" || outcome == "trial" {
		wantCalls, wantEffects, wantModels = 0, 0, 1
	}
	if forgeCalls.Load() != wantCalls || effects.Load() != wantEffects || model.calls != wantModels {
		t.Fatalf("Forge calls=%d effects=%d model calls=%d cause=%v", forgeCalls.Load(), effects.Load(), model.calls, cause)
	}
	if wantCalls == 1 && !model.feedbackObserved {
		t.Fatal("fake completion was not corrected through the existing model loop")
	}
	events, err := activities.ListBusinessActionEvents(ctx, "ws", dispatched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := teamrun.ProjectBusinessCompletionReceipts(events)
	if err != nil || len(receipts) != int(wantCalls) {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	if wantCalls == 1 && (receipts[0].Status != resultStatus || receipts[0].InputRevisionID != receipt.InputRevisionID || receipts[0].RecordID != "record-a" || receipts[0].CapabilityID != publishedBusinessCapability || receipts[0].OperationID == "") {
		t.Fatalf("receipt not bound to this operation: %+v", receipts)
	}
	state, err := deliveryverify.NewStore(pool).GetDeliveryState(ctx, "ws", dispatched.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if wantStatus == teamrun.StatusSucceeded {
		wantVerification := deliverable.VerificationPassed
		if outcome == "needs-input" {
			wantVerification = deliverable.VerificationUnknown
		}
		if state.Report == nil || state.Report.Status != wantVerification {
			t.Fatalf("verification=%+v want=%s", state.Report, wantVerification)
		}
		found := false
		for _, check := range state.Report.Checks {
			if check.VerifierID == deliverycheck.BusinessReceiptsID {
				found = true
				if check.Status != wantVerification {
					t.Fatalf("business check=%+v", check)
				}
			}
		}
		if !found {
			t.Fatal("final receipt verification was not persisted")
		}
	} else if state.Report != nil && state.Report.Status == deliverable.VerificationPassed {
		t.Fatal("failed or unknown action became verified completion")
	}
	if ok, err := executor.ProcessNext(ctx, "no-replay"); err != nil || ok {
		t.Fatalf("terminal result queued another execution: %v %v", ok, err)
	}
	if outcome == "trial" && forgeRequests.Load() != 0 {
		t.Fatal("developer trial contacted real Forge HTTP fixture")
	}
	if forgeCalls.Load() != wantCalls {
		t.Fatal("failure or unknown result was replayed")
	}
}

func publishBusinessCompletionSample(t *testing.T, pool *pgxpool.Pool, key []byte, origin string) (frozen.FrozenExecutionBundle, *workflow.PublishedArtifactContent, frozen.ArtifactEnvelopeV1, *teamconstruction.PublicationAuthority) {
	t.Helper()
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	bundle, _, _ := publishConfiguredMemberIntegrationSample(t, pool, key, "ws", "user", origin, "", 0, "", "Perform the declared authorized action and then report the actual outcome.", func(record *registry.AgentRecord) {
		record.MCPServers = nil
		record.BusinessCapabilityIDs = []string{publishedBusinessCapability}
	})
	artifacts := workflow.NewArtifactStore(pool, nil)
	flows := workflowcatalog.New(pool, nil, artifacts)
	draft, err := flows.CreateDraft(ctx, "ws", "flow", "user")
	if err != nil {
		t.Fatal(err)
	}
	var graph map[string]json.RawMessage
	if err := json.Unmarshal(draft.GraphDefinition, &graph); err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()})
	if err != nil {
		t.Fatal(err)
	}
	graph["output_contract"] = output
	graph["result_protocol"], _ = json.Marshal(machine.ResultProtocolWorkbenchV1)
	var nodes []map[string]json.RawMessage
	if err := json.Unmarshal(graph["nodes"], &nodes); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if string(node["id"]) == `"compute"` {
			node["output"] = output
		}
	}
	graph["nodes"], _ = json.Marshal(nodes)
	params, _ := json.Marshal(deliverycheck.BusinessReceiptParameters{RequiredCapabilityIDs: []string{publishedBusinessCapability}, WhenAuthorized: true, AllowNeedsInput: true})
	contract := deliverable.DeliveryContract{Version: 1, Coverage: deliverable.CoverageExplicit, Output: deliverable.OutputRequirement{Type: "json", Schema: machine.WorkbenchResultSchemaV1()}, RequiredChecks: []deliverable.CheckSpec{{ID: "business-effect", Title: "Required business receipt", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: deliverycheck.BusinessReceiptsVersion, Parameters: params}}, ExternalEffects: deliverable.ExternalEffectsRequired, ExternalEffectsCheckID: "business-effect"}
	graph["delivery_contract"], _ = json.Marshal(contract)
	raw, _ := json.Marshal(graph)
	_, err = flows.UpdateDraft(ctx, "ws", "flow", draft.Version, draft.UpdatedAt, workflow.DraftInput{CreatedBy: "user", TriggerConfig: draft.TriggerConfig, GraphDefinition: raw})
	if err != nil {
		t.Fatal(err)
	}
	builder := workflowcatalog.NewCandidateBuilder(flows, agentcatalog.New(pool), delivery.New(pool, key), skills.New(pool), credentials.New(pool, key), schedule.New(pool, nil), memberIntegrationDescriptors(t))
	authority, publications := openAPIProductPublication(t, ctx, pool, builder)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		t.Fatalf("candidate err=%v report=%+v", err, report)
	}
	command, err := teamconstruction.PublicationCommandForCandidate("business-completion-"+draft.WorkflowID, candidate, teamconstruction.PublicationTarget{TeamID: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := publications.Publish(ctx, command); err != nil {
		t.Fatal(err)
	}
	saved, err := artifacts.GetArtifact(ctx, "ws", "flow", draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := workflow.CandidateEnvelope(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, saved, envelope, authority
}
