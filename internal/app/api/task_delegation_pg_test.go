package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

type taskGrantHTTPFixture struct {
	server      *httptest.Server
	grants      map[string]businessaction.TaskDelegationGrant
	files       map[string][]byte
	rejectFiles atomic.Bool
	lastIfMatch atomic.Value
	calls       int
}

func newTaskGrantHTTPFixture(t *testing.T) *taskGrantHTTPFixture {
	t.Helper()
	fixture := &taskGrantHTTPFixture{grants: map[string]businessaction.TaskDelegationGrant{}, files: map[string][]byte{}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == businessaction.TaskDelegationPath+"/current" {
			fixture.calls++
			grant, ok := fixture.grants[token]
			if !ok {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"code":"task_token_required"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(grant)
			return
		}
		if strings.HasPrefix(r.URL.Path, businessaction.TaskDelegationPath+"/files/") && strings.HasSuffix(r.URL.Path, "/original") {
			fixture.lastIfMatch.Store(r.Header.Get("If-Match"))
			grant, ok := fixture.grants[token]
			if !ok {
				w.WriteHeader(401)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, businessaction.TaskDelegationPath+"/files/"), "/original")
			var selected *businessaction.TaskDelegationResource
			for i := range grant.Scope.Resources {
				resource := &grant.Scope.Resources[i]
				if resource.Type == "forge-file" && resource.ID == id {
					selected = resource
					break
				}
			}
			content, exists := fixture.files[id]
			if selected == nil || !exists || r.Header.Get("If-Match") != `"`+selected.SHA256+`"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if fixture.rejectFiles.Load() {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"code":"MATERIAL_CHANGED"}`))
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(content)))
			_, _ = w.Write(content)
			return
		}
		t.Errorf("unexpected task authority path %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}
func (f *taskGrantHTTPFixture) add(t *testing.T, token, subject, org string, generation int64, scope businessaction.TaskDelegationScope) {
	f.grants[token] = testTaskGrant(t, f.server.URL, subject, org, generation, scope)
}

func testTaskGrant(t *testing.T, issuer, subject, org string, generation int64, scope businessaction.TaskDelegationScope) businessaction.TaskDelegationGrant {
	t.Helper()
	raw, _ := json.Marshal(scope)
	digest, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	grant := businessaction.TaskDelegationGrant{Version: "1", Active: true, TokenType: "forge_task", Issuer: issuer, IdentityIssuer: "forge:task-delegation-test", GrantID: "task-grant-stable", Generation: generation, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(20 * time.Minute), ScopeSHA256: digest, Scope: scope}
	grant.Subject.ID = subject
	grant.Subject.OrganizationID = org
	return grant
}
func nativeTaskRegistrationFixture(t *testing.T) (*Server, *pgxpool.Pool, dispatchInputRegistration, *taskGrantHTTPFixture) {
	t.Helper()
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	dependencies := []frozen.FrozenDependencyRef{}
	manifestHash, err := frozen.ComputeManifestHash(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	bundle := frozen.FrozenExecutionBundle{
		SchemaVersion: 1, FactoryKey: compiler.StandardFrozenToolsKey(),
		Agent:          frozen.FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "worker", AgentVersion: 1, Name: "worker", Role: "worker", Engine: "loom", Model: "model", GraphType: "standard", FactoryInput: json.RawMessage(`{}`), Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}, OutputSchema: json.RawMessage(`{"type":"object"}`), BusinessCapabilityIDs: []string{"forge:action:sales_contract.ContractSubmit"}},
		PrimaryModel:   frozen.FrozenModelBinding{SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "provider", ProviderRevision: 1, ModelID: "model", BaseURL: "https://provider.example", CredentialRef: frozen.CredentialReference{SchemaVersion: 1, Scope: frozen.CredentialScopeUser, UserID: "user", WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}},
		FallbackModels: []frozen.FrozenModelBinding{}, Credentials: []frozen.CredentialReference{}, MCPBindings: []frozen.FrozenMCPBinding{}, Skills: []frozen.FrozenSkill{}, Dependencies: frozen.FrozenDependencyManifest{SchemaVersion: 1, Dependencies: dependencies, ManifestHash: manifestHash}, Capability: frozen.CapabilityManifest{SchemaVersion: 2, Role: "worker", AgentContentHash: strings.Repeat("b", 64)},
	}

	server, pool := newTeamDispatchTestServerWithGraph(t, json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`), bundle)
	authority := newTaskGrantHTTPFixture(t)
	if server.Config == nil {
		server.Config = &config.Config{}
	}
	server.Config.ForgeSessionURL = authority.server.URL + "/api/v1/auth/me"
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization)VALUES($1,'native-user','ws','user','native-org')`, "forge:task-delegation-test"); err != nil {
		t.Fatal(err)
	}
	server.ExternalIdentity = externalIdentityVerifierFunc(func(context.Context, string) (ExternalIdentity, error) {
		t.Fatal("task registration called general employee session verifier")
		return ExternalIdentity{}, errors.New("not reachable")
	})
	request := dispatchInputRegistrationFixture("native-session", "提交固定合同", "")
	version := 1
	request.WorkflowID, request.WorkflowVersion = "flow", &version
	actions := []string{"forge:action:sales_contract.ContractSubmit"}
	request.AuthorizedBusinessCapabilityIDs = &actions
	request.BusinessRecord = &dispatchBusinessRecord{ObjectName: "sales_contract", RecordID: "record-a"}
	return server, pool, request, authority
}
func scopeForRegistration(request dispatchInputRegistration) businessaction.TaskDelegationScope {
	actions := []string{}
	if request.AuthorizedBusinessCapabilityIDs != nil {
		actions = append(actions, (*request.AuthorizedBusinessCapabilityIDs)...)
	}
	inputRevisionID := request.InputRevisionID
	if inputRevisionID == "" {
		inputRevisionID = stableDispatchInputID("ws", "user", request.RegistrationID)
	}
	scope := businessaction.TaskDelegationScope{InputRevisionID: inputRevisionID, RegistrationID: request.RegistrationID, TaskSHA256: dispatchInputDigest([]byte(request.Task)), WorkflowID: request.WorkflowID, WorkflowVersion: *request.WorkflowVersion, AllowedActions: actions, Resources: taskScopeResources(request.Resources)}
	if request.BusinessRecord != nil {
		scope.BusinessRecord = &businessaction.TaskBusinessRecord{ObjectName: request.BusinessRecord.ObjectName, RecordID: request.BusinessRecord.RecordID}
	}
	return scope
}
func registerTaskInput(t *testing.T, server *Server, request dispatchInputRegistration, token string) (*httptest.ResponseRecorder, dispatchInputReceipt) {
	t.Helper()
	body, _ := json.Marshal(request)
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
func TestPrepareAndRegisterNativeTaskTokenScopeRealPG(t *testing.T) {
	server, pool, request, authority := nativeTaskRegistrationFixture(t)
	scope := scopeForRegistration(request)
	body, _ := json.Marshal(request)
	c, prepared := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs/prepare", "ws", "user")
	if err := server.handlePrepareDispatchInput(c); err != nil || prepared.Code != 200 {
		t.Fatalf("prepare status=%d err=%v", prepared.Code, err)
	}
	var preparation struct {
		InputRevisionID string `json:"input_revision_id"`
	}
	_ = json.Unmarshal(prepared.Body.Bytes(), &preparation)
	if preparation.InputRevisionID != scope.InputRevisionID || authority.calls != 0 {
		t.Fatal("prepare was not read-only or stable")
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("prepare wrote an input")
	}
	request.InputRevisionID = preparation.InputRevisionID
	authority.add(t, "native-task-token", "native-user", "native-org", 3, scope)
	created, receipt := registerTaskInput(t, server, request, "native-task-token")
	if created.Code != 201 || receipt.InputRevisionID != preparation.InputRevisionID {
		t.Fatalf("register status=%d body=%s", created.Code, created.Body.String())
	}
	var ciphertext, grantID, org string
	var generation int64
	if err := pool.QueryRow(t.Context(), `SELECT credential_ciphertext,grant_id,external_organization,refresh_generation FROM weave_task_business_delegations WHERE input_revision_id=$1`, receipt.InputRevisionID).Scan(&ciphertext, &grantID, &org, &generation); err != nil {
		t.Fatal(err)
	}
	key, _ := secret.KeyFromEnv()
	plain, err := secret.Open(key, ciphertext)
	clear(key)
	defer clear(plain)
	if err != nil || string(plain) != "native-task-token" || generation != 3 || org != "native-org" || grantID == "" {
		t.Fatal("stored task grant did not preserve verified native authority")
	}
	replayed, replayedReceipt := registerTaskInput(t, server, request, "native-task-token")
	if replayed.Code != 200 || replayedReceipt != receipt {
		t.Fatal("same input replay changed receipt")
	}
	if err := pool.QueryRow(t.Context(), `SELECT refresh_generation FROM weave_task_business_delegations WHERE input_revision_id=$1`, receipt.InputRevisionID).Scan(&generation); err != nil || generation != 3 {
		t.Fatal("registration replay invented a local authorization generation")
	}
}
func TestTaskRegistrationRejectsEmployeeTokenScopeAndOrganizationRealPG(t *testing.T) {
	server, pool, request, authority := nativeTaskRegistrationFixture(t)
	scope := scopeForRegistration(request)
	request.InputRevisionID = scope.InputRevisionID
	for _, test := range []struct {
		name, token string
		mutate      func(*businessaction.TaskDelegationGrant)
		status      int
		code        string
	}{
		{"employee bearer", "employee-session", nil, 401, "business_delegation_invalid"},
		{"native organization", "wrong-org", func(g *businessaction.TaskDelegationGrant) { g.Subject.OrganizationID = "other-native-org" }, 403, "business_delegation_identity_mismatch"},
		{"native employee", "wrong-user", func(g *businessaction.TaskDelegationGrant) { g.Subject.ID = "other-native-user" }, 403, "business_delegation_identity_mismatch"},
		{"record scope", "wrong-scope", func(g *businessaction.TaskDelegationGrant) {
			g.Scope.BusinessRecord.RecordID = "record-other"
			raw, _ := json.Marshal(g.Scope)
			g.ScopeSHA256, _ = frozen.HashCanonicalJSON(raw)
		}, 403, "business_delegation_scope_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.mutate != nil {
				authority.add(t, test.token, "native-user", "native-org", 1, scope)
				g := authority.grants[test.token]
				copyRecord := *g.Scope.BusinessRecord
				g.Scope.BusinessRecord = &copyRecord
				test.mutate(&g)
				authority.grants[test.token] = g
			}
			response, _ := registerTaskInput(t, server, request, test.token)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("wrong rejection: %d %s", response.Code, response.Body.String())
			}
		})
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_dispatch_input_revisions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected task grant created frozen input")
	}
	_ = execution.AuthorizationExpiredBeforeDispatch
}

func TestExpiredParkedTaskAuthorizationRenewsAfterMaintenanceRealPG(t *testing.T) {
	server, pool, request, authority := nativeTaskRegistrationFixture(t)
	request.ProjectID = workbenchProjectID("user")
	scope := scopeForRegistration(request)
	authority.add(t, "initial-task-token", "native-user", "native-org", 1, scope)
	created, input := registerTaskInput(t, server, request, "initial-task-token")
	if created.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", created.Code, created.Body.String())
	}
	dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID}, "user")
	if err != nil || dispatched.Code != http.StatusCreated {
		t.Fatalf("dispatch: %v %d %s", err, dispatched.Code, dispatched.Body.String())
	}
	var receipt workflowManualRunResponse
	if err := json.Unmarshal(dispatched.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	claimed, err := server.Tasks.Claim(t.Context(), "expiry-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: "ws", IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	consumer := &teamrun.Consumer{Transactions: pool, Snapshots: server.Snapshots, Runs: teamrun.NewPGStore(), Tasks: server.Tasks}
	if _, err := consumer.ConsumeClaimed(t.Context(), claimed, "expiry-worker"); err != nil {
		t.Fatal(err)
	}
	proof, _ := execution.AuthorizationRefusalFromError(execution.NewAuthorizationRefusal(input.InputRevisionID, 2, errors.New("expired before dispatch")))
	wait, _ := json.Marshal(teamrun.RuntimeWaitDetailV1{SchemaVersion: 1, WaitType: "runtime", NodeID: "member", AuthorizationRequired: &proof})
	if _, err := pool.Exec(t.Context(), `
		UPDATE weave_task_business_delegations SET expires_at=issued_at+interval '1 millisecond',refresh_generation=2
		  WHERE workspace_id='ws' AND input_revision_id=$1;
		UPDATE weave_team_runs SET status='parked',wait_kind='runtime',wait_detail=$3::jsonb,
		  resume_token_hash='\x01'::bytea,checkpoint_ref=$4
		  WHERE workspace_id='ws' AND run_id=$2`, input.InputRevisionID, receipt.RunID, string(wait), teamrun.CheckpointRef("ws", receipt.RunID)); err != nil {
		t.Fatal(err)
	}
	if _, err := newTaskDelegationRevoker(pool).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := readRevokedDelegation(t, pool, input.InputRevisionID)
	if row.revoked {
		t.Fatal("expiry maintenance permanently revoked a live input waiting on a renewable no-effect proof")
	}
	state, err := server.readWorkbenchAuthorization(t.Context(), "ws", "user", input.InputRevisionID)
	if err != nil || !state.CanRenew || state.Status != "renewal_required" || state.Generation != 2 {
		t.Fatalf("expired input lost same-input renewal: %+v %v", state, err)
	}
	authority.add(t, "renewed-task-token", "native-user", "native-org", 3, scope)
	c, response := dispatchInputTestContext([]byte(`{"expected_generation":2}`), "/v1/workbench/dispatch-inputs/"+input.InputRevisionID+"/authorization", "ws", "user")
	c.SetParamNames("input_revision_id")
	c.SetParamValues(input.InputRevisionID)
	c.Request().Header.Set(forgeDelegationHeader, "Bearer renewed-task-token")
	if err := server.handleRenewDispatchAuthorization(c); err != nil || response.Code != http.StatusOK {
		t.Fatalf("same-input renewal failed after maintenance: %v %d %s", err, response.Code, response.Body.String())
	}
	var generation int64
	var tokenCiphertext string
	if err := pool.QueryRow(t.Context(), `SELECT refresh_generation,credential_ciphertext FROM weave_task_business_delegations
		WHERE workspace_id='ws' AND input_revision_id=$1 AND revoked_at IS NULL`, input.InputRevisionID).Scan(&generation, &tokenCiphertext); err != nil || generation != 3 {
		t.Fatalf("new generation not persisted for original input: %d %v", generation, err)
	}
	key, _ := secret.KeyFromEnv()
	plain, err := secret.Open(key, tokenCiphertext)
	clear(key)
	defer clear(plain)
	if err != nil || string(plain) != "renewed-task-token" {
		t.Fatal("renewal did not preserve the new task credential")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_task_business_delegations SET revoked_at=now(),revocation_reason='employee_cancel'
		WHERE workspace_id='ws' AND input_revision_id=$1`, input.InputRevisionID); err != nil {
		t.Fatal(err)
	}
	if state, err := server.readWorkbenchAuthorization(t.Context(), "ws", "user", input.InputRevisionID); err != nil || state.CanRenew {
		t.Fatalf("explicitly cancelled input was offered renewal: %+v %v", state, err)
	}
}
