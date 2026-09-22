package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/labstack/echo/v4"
)

func dispatchInputRegistrationFixture(session, task, previous string) dispatchInputRegistration {
	seq := int64(0)
	return dispatchInputRegistration{
		RegistrationID: uuid.NewString(), WorkbenchSessionID: session, ExpectedRevisionID: previous,
		SourceMessages: []dispatchInputSourceMessage{{MessageID: "user-" + uuid.NewString(), EventSeq: &seq, SHA256: dispatchInputDigest([]byte(task))}},
		Task:           task, TeamID: "team",
	}
}

func dispatchInputTestContext(body []byte, path, workspaceID, userID string) (echo.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: workspaceID, UserID: userID}))
	c := echo.New().NewContext(request, recorder)
	c.Set("tenant", workspaceID)
	c.Set("user_id", userID)
	if strings.HasSuffix(path, "/dispatch") {
		c.SetPath("/v1/teams/:id/dispatch")
		c.SetParamNames("id")
		c.SetParamValues("team")
	}
	return c, recorder
}

func registerInputForTest(server *Server, request dispatchInputRegistration) (*httptest.ResponseRecorder, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	c, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", "user")
	return recorder, server.handleRegisterDispatchInput(c)
}

func TestDispatchInputFreezesAndRefreshesEmployeeForgeDelegationRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("11", 32))
	dependencies := []frozen.FrozenDependencyRef{}
	manifestHash, err := frozen.ComputeManifestHash(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	bundle := frozen.FrozenExecutionBundle{
		SchemaVersion: 1,
		FactoryKey:    frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "1", CompilerABI: "weave-graph-abi-v1"},
		Agent: frozen.FrozenAgentRecord{
			SchemaVersion: 1, WorkspaceID: "ws", AgentID: "worker", AgentVersion: 1, Name: "worker", Role: "worker", Engine: "loom", Model: "model",
			GraphType: "standard", FactoryInput: json.RawMessage(`{}`), Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}, OutputSchema: json.RawMessage(`{"type":"object"}`),
			BusinessCapabilityIDs: []string{"forge:action:sales_contract.ContractSubmit"},
		},
		PrimaryModel:   frozen.FrozenModelBinding{SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "provider", ProviderRevision: 1, ModelID: "model", BaseURL: "https://provider.example", CredentialRef: frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}},
		FallbackModels: []frozen.FrozenModelBinding{}, Credentials: []frozen.CredentialReference{}, MCPBindings: []frozen.FrozenMCPBinding{}, Skills: []frozen.FrozenSkill{},
		Dependencies: frozen.FrozenDependencyManifest{SchemaVersion: 1, Dependencies: dependencies, ManifestHash: manifestHash},
		Capability:   frozen.CapabilityManifest{SchemaVersion: 2, Role: "worker", AgentContentHash: strings.Repeat("b", 64)},
	}
	server, pool := newTeamDispatchTestServerWithGraph(t, json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`), bundle)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		VALUES('https://forge.example.test','forge-user','ws','user')`); err != nil {
		t.Fatal(err)
	}
	server.ExternalIdentity = externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
		if token != "first-token" && token != "refreshed-token" {
			return ExternalIdentity{}, errors.New("unexpected token")
		}
		return ExternalIdentity{Issuer: "https://forge.example.test", Subject: "forge-user", Organization: "ws"}, nil
	})
	version := 1
	registration := dispatchInputRegistrationFixture("forge-session", "提交这份固定合同", "")
	registration.WorkflowID, registration.WorkflowVersion = "flow", &version
	register := func(token string) (*httptest.ResponseRecorder, dispatchInputReceipt) {
		t.Helper()
		body, _ := json.Marshal(registration)
		c, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", "user")
		if token != "" {
			c.Request().Header.Set(forgeDelegationHeader, "Bearer "+token)
		}
		if err := server.handleRegisterDispatchInput(c); err != nil {
			t.Fatal(err)
		}
		var receipt dispatchInputReceipt
		if recorder.Code < 300 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
		}
		return recorder, receipt
	}
	created, receipt := register("first-token")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var ciphertext string
	var actions []byte
	var generation int
	if err := pool.QueryRow(t.Context(), `SELECT credential_ciphertext,allowed_actions,refresh_generation
		FROM weave_task_business_delegations WHERE workspace_id='ws' AND input_revision_id=$1`, receipt.InputRevisionID).Scan(&ciphertext, &actions, &generation); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "first-token" || strings.Contains(ciphertext, "first-token") || generation != 1 || !strings.Contains(string(actions), "ContractSubmit") {
		t.Fatalf("delegation was not frozen safely: ciphertext=%q actions=%s generation=%d", ciphertext, actions, generation)
	}
	replayed, replayReceipt := register("refreshed-token")
	if replayed.Code != http.StatusOK || replayReceipt != receipt {
		t.Fatalf("replay status=%d receipt=%+v want=%+v", replayed.Code, replayReceipt, receipt)
	}
	if err := pool.QueryRow(t.Context(), `SELECT refresh_generation FROM weave_task_business_delegations
		WHERE workspace_id='ws' AND input_revision_id=$1`, receipt.InputRevisionID).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("refresh generation=%d err=%v", generation, err)
	}
	missing := dispatchInputRegistrationFixture("forge-session-missing", "提交另一份合同", "")
	missing.WorkflowID, missing.WorkflowVersion = "flow", &version
	body, _ := json.Marshal(missing)
	c, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", "user")
	if err := server.handleRegisterDispatchInput(c); err != nil || recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "business_delegation_required") {
		t.Fatalf("missing delegation status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
}

func boundDispatchForTest(server *Server, body map[string]any, userID string) (*httptest.ResponseRecorder, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	c, recorder := dispatchInputTestContext(encoded, "/v1/teams/team/dispatch", "ws", userID)
	return recorder, server.handleDispatchTeam(c)
}

func reconcileInputForTest(server *Server, revisionID string) (*httptest.ResponseRecorder, error) {
	c, recorder := dispatchInputTestContext([]byte(`{}`), "/v1/workbench/dispatch-inputs/"+revisionID+"/reconcile", "ws", "user")
	c.SetParamNames("input_revision_id")
	c.SetParamValues(revisionID)
	return recorder, server.handleReconcileDispatchInput(c)
}

func TestBoundDispatchInputProvenanceAndAtomicAdmissionRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	ctx := context.Background()
	register := func(request dispatchInputRegistration, wantStatus int, wantCode string) dispatchInputReceipt {
		t.Helper()
		recorder, err := registerInputForTest(server, request)
		if err != nil || recorder.Code != wantStatus || wantCode != "" && !strings.Contains(recorder.Body.String(), wantCode) {
			t.Fatalf("register status=%d want=%d body=%s err=%v", recorder.Code, wantStatus, recorder.Body.String(), err)
		}
		var receipt dispatchInputReceipt
		if wantStatus < 300 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
		}
		return receipt
	}
	dispatch := func(body map[string]any, userID string, wantStatus int, wantCode string) workflowManualRunResponse {
		t.Helper()
		recorder, err := boundDispatchForTest(server, body, userID)
		if err != nil || recorder.Code != wantStatus || wantCode != "" && !strings.Contains(recorder.Body.String(), wantCode) {
			t.Fatalf("dispatch status=%d want=%d body=%s err=%v", recorder.Code, wantStatus, recorder.Body.String(), err)
		}
		var receipt workflowManualRunResponse
		if wantStatus < 300 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
		}
		return receipt
	}
	assertCounts := func(tasks, snapshots int) {
		t.Helper()
		var gotTasks, gotSnapshots int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM weave_task_queue), (SELECT count(*) FROM weave_team_run_snapshots)`).Scan(&gotTasks, &gotSnapshots); err != nil {
			t.Fatal(err)
		}
		if gotTasks != tasks || gotSnapshots != snapshots {
			t.Fatalf("task/snapshot counts=%d/%d want=%d/%d", gotTasks, gotSnapshots, tasks, snapshots)
		}
	}

	firstRequest := dispatchInputRegistrationFixture("session", "VBR-52 original task", "")
	first := register(firstRequest, http.StatusCreated, "")
	if replay := register(firstRequest, http.StatusOK, ""); replay != first {
		t.Fatalf("registration replay changed identity: %+v != %+v", replay, first)
	}
	changed := firstRequest
	changed.Task = "different"
	register(changed, http.StatusConflict, "input_registration_conflict")
	changed = firstRequest
	changed.WorkbenchSessionID = "different-session"
	register(changed, http.StatusConflict, "input_registration_conflict")
	invalid := dispatchInputRegistrationFixture("invalid", "task", "")
	invalid.SourceMessages = nil
	register(invalid, http.StatusBadRequest, "dispatch_input_request_invalid")
	invalid = dispatchInputRegistrationFixture("invalid", "task", "")
	invalid.Mode = "free_collab"
	register(invalid, http.StatusBadRequest, "dispatch_input_mode_unsupported")

	// Freeze this resolved request before another registration wins. Calling the
	// admission function later simulates a head replacement after route resolution.
	resolvedFirst, err := server.loadDispatchInput(ctx, "ws", "user", first.InputRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	secondText := " \n我是 Nora 的助理，为 INV-440 准备状态。\n保留“引号”与最后换行。\n"
	secondRequest := dispatchInputRegistrationFixture("session", secondText, first.InputRevisionID)
	second := register(secondRequest, http.StatusCreated, "")
	if second.TaskSHA256 != dispatchInputDigest([]byte(secondText)) || second.ClientRequestID == first.ClientRequestID {
		t.Fatal("new input did not freeze exact text with an independent request key")
	}
	staleContext, staleRecorder := dispatchInputTestContext([]byte(`{}`), "/v1/teams/team/dispatch", "ws", "user")
	staleRequest := teamDispatchRequest{
		InputRevisionID: first.InputRevisionID, Task: resolvedFirst.Task, Mode: "workflow", WorkflowID: "flow",
		WorkflowVersion: &resolvedFirst.WorkflowVersion, ClientRequestID: first.ClientRequestID, inputBinding: &resolvedFirst,
	}
	if err := server.admitTeamWorkflowDispatch(staleContext, "flow", staleRequest); err != nil || staleRecorder.Code != http.StatusConflict ||
		!strings.Contains(staleRecorder.Body.String(), "dispatch_input_superseded") {
		t.Fatalf("in-transaction stale revision admitted: status=%d body=%s err=%v", staleRecorder.Code, staleRecorder.Body.String(), err)
	}
	dispatch(map[string]any{"input_revision_id": first.InputRevisionID}, "user", http.StatusConflict, "dispatch_input_superseded")
	dispatch(map[string]any{"input_revision_id": second.InputRevisionID}, "another-user", http.StatusNotFound, "dispatch_input_not_found")
	for _, assertion := range []map[string]any{
		{"task": firstRequest.Task}, {"task": strings.TrimSpace(secondText)}, {"task": ""},
		{"client_request_id": uuid.NewString()}, {"mode": "free_collab"}, {"workflow_id": "wrong"},
		{"workflow_version": 2}, {"project_id": "wrong"}, {"conversation_id": "old-conversation"},
	} {
		assertion["input_revision_id"] = second.InputRevisionID
		dispatch(assertion, "user", http.StatusConflict, "dispatch_input_mismatch")
	}
	dispatch(map[string]any{"input_revision_id": second.InputRevisionID, "task": nil}, "user", http.StatusBadRequest, "team_dispatch_request_invalid")
	assertCounts(0, 0)

	// The default may change after registration; the saved workflow selection
	// and its original registration replay must remain fixed.
	if _, err := pool.Exec(ctx, `UPDATE weave_teams SET default_workflow_id=NULL WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	if replay := register(secondRequest, http.StatusOK, ""); replay != second {
		t.Fatal("default change altered the registered input")
	}
	secondRun := dispatch(map[string]any{"input_revision_id": second.InputRevisionID}, "user", http.StatusCreated, "")
	if secondRun.ClientRequestID != second.ClientRequestID || secondRun.InputRevisionID != second.InputRevisionID {
		t.Fatalf("dispatch receipt lost input identity: %+v", secondRun)
	}
	task, err := server.Tasks.Get(ctx, "ws", secondRun.TaskID)
	var storedTask string
	if err != nil || json.Unmarshal(task.Payload, &storedTask) != nil || storedTask != secondText {
		t.Fatalf("source text changed between registration and queue: %+v err=%v", task, err)
	}
	storedInput, err := server.loadDispatchInput(ctx, "ws", "user", second.InputRevisionID)
	if err != nil || storedInput.ConsumedRunID != secondRun.RunID || storedInput.ConsumedTaskID != secondRun.TaskID {
		t.Fatalf("input consumption receipt missing: %+v err=%v", storedInput, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_teams SET default_workflow_id='flow' WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	thirdRequest := dispatchInputRegistrationFixture("session", "third task", second.InputRevisionID)
	wrongCAS := thirdRequest
	wrongCAS.ExpectedRevisionID = first.InputRevisionID
	register(wrongCAS, http.StatusConflict, "input_revision_conflict")
	third := register(thirdRequest, http.StatusCreated, "")
	// Replaying an old registration must not reactivate it or select the latest
	// body. A fresh Server also demonstrates this behavior needs no process state.
	if replay := register(secondRequest, http.StatusOK, ""); replay != second {
		t.Fatal("old registration receipt changed")
	}
	server = &Server{
		Store:                server.Store,
		KernelPublication:    server.KernelPublication,
		OrgStore:             server.OrgStore,
		Registry:             server.Registry,
		Workflow:             server.Workflow,
		WorkflowArtifacts:    server.WorkflowArtifacts,
		Deliverables:         server.Deliverables,
		ScheduleTransactions: server.ScheduleTransactions,
		Snapshots:            server.Snapshots,
		Tasks:                server.Tasks,
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_teams SET status='building' WHERE workspace_id='ws'; UPDATE weave_workflow_version_admission_statuses SET blocked=true WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	// A retained receipt cannot bypass the current product authorization gate.
	dispatch(map[string]any{"input_revision_id": second.InputRevisionID, "client_request_id": second.ClientRequestID}, "user", http.StatusUnprocessableEntity, "workflow_not_runnable")
	dispatch(map[string]any{"input_revision_id": second.InputRevisionID, "client_request_id": uuid.NewString()}, "user", http.StatusConflict, "dispatch_input_mismatch")
	if _, err := pool.Exec(ctx, `UPDATE weave_teams SET status='active' WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	if replay := dispatch(map[string]any{"input_revision_id": second.InputRevisionID, "client_request_id": second.ClientRequestID}, "user", http.StatusOK, ""); replay != secondRun {
		t.Fatalf("restored authorization changed receipt: %+v", replay)
	}
	dispatch(map[string]any{"input_revision_id": third.InputRevisionID}, "user", http.StatusConflict, "")
	assertCounts(1, 1)
	if _, err := pool.Exec(ctx, `UPDATE weave_workflow_version_admission_statuses SET blocked=false WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}

	// Fail the product association after Kernel accepted. The execution remains
	// durable while consumption rolls back; recovery must reuse its same receipt.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_bound_input_consume() RETURNS trigger LANGUAGE plpgsql AS $$
	 BEGIN IF NEW.consumed_run_id IS NOT NULL THEN RAISE EXCEPTION 'injected input consume failure'; END IF; RETURN NEW; END $$;
	 CREATE TRIGGER fail_bound_input_consume BEFORE UPDATE ON weave_dispatch_input_revisions
	 FOR EACH ROW EXECUTE FUNCTION fail_bound_input_consume()`); err != nil {
		t.Fatal(err)
	}
	dispatch(map[string]any{"input_revision_id": third.InputRevisionID}, "user", http.StatusInternalServerError, "")
	assertCounts(2, 2)
	thirdStored, err := server.loadDispatchInput(ctx, "ws", "user", third.InputRevisionID)
	if err != nil || thirdStored.ConsumedRunID != "" || !thirdStored.IsCurrent {
		t.Fatalf("failed transaction consumed the input: %+v err=%v", thirdStored, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_bound_input_consume ON weave_dispatch_input_revisions; DROP FUNCTION fail_bound_input_consume()`); err != nil {
		t.Fatal(err)
	}

	const concurrentRequests = 6
	results := make(chan *httptest.ResponseRecorder, concurrentRequests)
	errors := make(chan error, concurrentRequests)
	var wait sync.WaitGroup
	for range concurrentRequests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			recorder, err := boundDispatchForTest(server, map[string]any{"input_revision_id": third.InputRevisionID}, "user")
			results <- recorder
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	created, runID := 0, ""
	for recorder := range results {
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			t.Fatalf("concurrent dispatch status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if recorder.Code == http.StatusCreated {
			created++
		}
		var receipt workflowManualRunResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if runID != "" && runID != receipt.RunID {
			t.Fatal("concurrent identical bound requests created different runs")
		}
		runID = receipt.RunID
	}
	if created != 0 {
		t.Fatalf("recovery reported %d new admissions", created)
	}
	assertCounts(2, 2)
}

func TestDispatchInputRegistrationCASConcurrentRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	for _, shareRegistration := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_registration_%v", shareRegistration), func(t *testing.T) {
			first := dispatchInputRegistrationFixture(uuid.NewString(), "task A", "")
			second := first
			if !shareRegistration {
				second.RegistrationID = uuid.NewString()
				second.Task = "task B"
			}
			results := make(chan *httptest.ResponseRecorder, 2)
			errors := make(chan error, 2)
			var wait sync.WaitGroup
			for _, request := range []dispatchInputRegistration{first, second} {
				wait.Add(1)
				go func() {
					defer wait.Done()
					recorder, err := registerInputForTest(server, request)
					results <- recorder
					errors <- err
				}()
			}
			wait.Wait()
			close(results)
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			counts := make(map[int]int)
			for recorder := range results {
				counts[recorder.Code]++
			}
			otherStatus := http.StatusConflict
			if shareRegistration {
				otherStatus = http.StatusOK
			}
			if counts[http.StatusCreated] != 1 || counts[otherStatus] != 1 {
				t.Fatalf("concurrent registration statuses=%v", counts)
			}
			var heads int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_dispatch_input_revisions
			 WHERE workspace_id='ws' AND user_id='user' AND workbench_session_id=$1 AND is_current`, first.WorkbenchSessionID).Scan(&heads); err != nil || heads != 1 {
				t.Fatalf("head count=%d err=%v", heads, err)
			}
		})
	}
}

func TestDispatchInputReconcileFencesDelayedAdmissionRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	ctx := context.Background()
	register := func(request dispatchInputRegistration) dispatchInputReceipt {
		t.Helper()
		recorder, err := registerInputForTest(server, request)
		if err != nil || (recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated) {
			t.Fatalf("registration status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
		var receipt dispatchInputReceipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	reconcile := func(revisionID string) dispatchInputReconciliation {
		t.Helper()
		recorder, err := reconcileInputForTest(server, revisionID)
		if err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("reconciliation status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
		var result dispatchInputReconciliation
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	request := dispatchInputRegistrationFixture("uncertain-response", "old task", "")
	input := register(request)
	resolved, err := server.loadDispatchInput(ctx, "ws", "user", input.InputRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		closed := reconcile(input.InputRevisionID)
		if closed.State != "closed" || closed.Receipt != input || closed.Result != nil {
			t.Fatalf("unconsumed reconciliation=%+v", closed)
		}
	}
	// Lost registration responses can be recovered without a dispatch. Neither
	// that replay nor a late dispatch may reopen the closed revision.
	if replay := register(request); replay != input {
		t.Fatal("recovered registration changed the fixed receipt")
	}
	recorder, err := boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID}, "user")
	if err != nil || recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "dispatch_input_closed") {
		t.Fatalf("late dispatch escaped fence: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	staleContext, staleRecorder := dispatchInputTestContext([]byte(`{}`), "/v1/teams/team/dispatch", "ws", "user")
	if err := server.admitTeamWorkflowDispatch(staleContext, "flow", resolved.dispatchRequest()); err != nil ||
		staleRecorder.Code != http.StatusConflict || !strings.Contains(staleRecorder.Body.String(), "dispatch_input_closed") {
		t.Fatalf("in-transaction admission escaped fence: status=%d body=%s err=%v", staleRecorder.Code, staleRecorder.Body.String(), err)
	}
	// Closing retains the CAS head so the subsequent, separately selected input
	// can replace it with its known expected_revision_id.
	next := register(dispatchInputRegistrationFixture("uncertain-response", "new task", input.InputRevisionID))
	recorder, err = boundDispatchForTest(server, map[string]any{"input_revision_id": next.InputRevisionID}, "user")
	if err != nil || recorder.Code != http.StatusCreated {
		t.Fatalf("new input blocked after closure: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	var acceptedRun workflowManualRunResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &acceptedRun); err != nil {
		t.Fatal(err)
	}
	accepted := reconcile(next.InputRevisionID)
	if accepted.State != "accepted" || accepted.Receipt != next || accepted.Result == nil || *accepted.Result != acceptedRun {
		t.Fatalf("accepted reconciliation lost original run: %+v", accepted)
	}
	stored, err := server.loadDispatchInput(ctx, "ws", "user", next.InputRevisionID)
	if err != nil || stored.IsClosed || stored.ConsumedRunID != acceptedRun.RunID {
		t.Fatalf("reconciliation altered accepted work: %+v err=%v", stored, err)
	}

	expectedTasks := 1
	for attempt := range 4 {
		input := register(dispatchInputRegistrationFixture(fmt.Sprintf("reconcile-race-%d", attempt), "race task", ""))
		var dispatchRecorder, reconcileRecorder *httptest.ResponseRecorder
		var dispatchErr, reconcileErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			dispatchRecorder, dispatchErr = boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID}, "user")
		}()
		go func() {
			defer wait.Done()
			reconcileRecorder, reconcileErr = reconcileInputForTest(server, input.InputRevisionID)
		}()
		wait.Wait()
		if dispatchErr != nil || reconcileErr != nil || reconcileRecorder.Code != http.StatusOK {
			t.Fatalf("reconcile race errors=%v/%v status=%d body=%s", dispatchErr, reconcileErr, reconcileRecorder.Code, reconcileRecorder.Body.String())
		}
		var outcome dispatchInputReconciliation
		if err := json.Unmarshal(reconcileRecorder.Body.Bytes(), &outcome); err != nil {
			t.Fatal(err)
		}
		switch outcome.State {
		case "accepted":
			expectedTasks++
			var run workflowManualRunResponse
			if err := json.Unmarshal(dispatchRecorder.Body.Bytes(), &run); err != nil || dispatchRecorder.Code != http.StatusCreated ||
				outcome.Result == nil || run.RunID != outcome.Result.RunID {
				t.Fatalf("accepted race lacks matching run: status=%d body=%s result=%+v", dispatchRecorder.Code, dispatchRecorder.Body.String(), outcome)
			}
		case "closed":
			if dispatchRecorder.Code != http.StatusConflict || !strings.Contains(dispatchRecorder.Body.String(), "dispatch_input_closed") {
				t.Fatalf("closed race still dispatched: status=%d body=%s", dispatchRecorder.Code, dispatchRecorder.Body.String())
			}
		default:
			t.Fatalf("unknown reconciliation state: %+v", outcome)
		}
		var taskCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&taskCount); err != nil || taskCount != expectedTasks {
			t.Fatalf("reconcile/dispatch race task count=%d want=%d err=%v", taskCount, expectedTasks, err)
		}
	}
}
