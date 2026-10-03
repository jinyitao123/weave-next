package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

// The integration test routes a support ticket: nothing in the endpoint knows
// about tickets, games or any other consumer domain.
var ticketContract = map[string]any{"input_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ticket", "queues"}, "properties": map[string]any{"ticket": map[string]any{"type": "string"}, "attempt": map[string]any{"type": "integer"}, "queues": map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []string{"queue_id"}}}}}, "output_schema": map[string]any{"type": "object", "required": []string{"queue_id"}, "properties": map[string]any{"queue_id": map[string]any{"type": "string"}}}, "options_pointer": "/queues", "option_id_field": "queue_id", "choice_pointer": "/queue_id", "instruction": "Pick the queue best placed to resolve the ticket."}

func ticketInput(attempt int) map[string]any {
	return map[string]any{"ticket": "Invoice shows the wrong VAT", "attempt": attempt, "queues": []any{map[string]any{"queue_id": "billing"}, map[string]any{"queue_id": "tech"}}}
}

func TestBoundedDecisionBindingAdmissionLanesAndCancellationRealPG(t *testing.T) {
	dependencies := []frozen.FrozenDependencyRef{}
	digest, _ := frozen.ComputeManifestHash(dependencies)
	bundle := frozen.FrozenExecutionBundle{SchemaVersion: 1, FactoryKey: frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "1", CompilerABI: "weave-graph-abi-v1"}, Agent: frozen.FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "worker", AgentVersion: 1, Name: "worker", Role: "worker", Engine: "loom", Model: "model", GraphType: "standard", FactoryInput: json.RawMessage(`{}`), Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}, MemoryConfig: &frozen.FrozenMemoryConfig{Enabled: false, AutoRemember: false}, OutputSchema: json.RawMessage(`{"type":"object"}`)}, PrimaryModel: frozen.FrozenModelBinding{SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "provider", ProviderRevision: 1, ModelID: "model", BaseURL: "https://provider.example", CredentialRef: frozen.CredentialReference{SchemaVersion: 1, Scope: frozen.CredentialScopeUser, UserID: "user", WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}}, Dependencies: frozen.FrozenDependencyManifest{SchemaVersion: 1, Dependencies: dependencies, ManifestHash: digest}, Capability: frozen.CapabilityManifest{SchemaVersion: 2, Role: "worker", AgentContentHash: strings.Repeat("b", 64)}}
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"worker","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"worker","type":"worker","inputs":{"task":{"expected_type":"text","value":{"source":"run_input","path":""}}},"output":{"type":"text"},"config":{"agent_id":"worker","agent_version":1,"kind":"consult","result_requirement":"choose a queue"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"worker","path":""}}}],"edges":[{"id":"delivery","from_node_id":"worker","to_node_id":"deliver","route":"success"}]}`)
	s, pool := newTeamDispatchTestServerWithGraph(t, graph, bundle)
	s.KeyStore = apikeys.NewStore(pool)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('worker','ws','worker','worker','{}'); INSERT INTO weave_team_workers(workspace_id,team_id,worker_agent_id,allowed_kinds,default_kind) VALUES('ws','team','worker',ARRAY['consult'],'consult')`); err != nil {
		t.Fatal(err)
	}
	s.teamRunCancel = &teamrun.CancelService{Transactions: pool, Runs: &teamrun.PGStore{Transactions: pool}, Tasks: s.Tasks}
	s.teamRunActivities = &teamrun.PGActivityStore{Transactions: pool}
	if _, err := pool.Exec(context.Background(), `INSERT INTO weave_api_keys(id,tenant_id,name,key_hash,role,scopes) VALUES('decision-key','ws','d','d-hash','service',ARRAY['decisions']),('other-key','ws','o','o-hash','service',ARRAY['decisions']),('broad-key','ws','b','b-hash','service',ARRAY['decisions','runs']),('admin-decision-key','ws','a','a-hash','admin',ARRAY['decisions'])`); err != nil {
		t.Fatal(err)
	}
	call := func(handler echo.HandlerFunc, key, path string, body any, params ...string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		method := http.MethodPost
		if strings.HasPrefix(path, "/v1/auth/api-keys/") {
			method = http.MethodDelete
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Idempotency-Key", "bounded-decision-test:"+path)
		subject := execution.Subject{WorkspaceID: "ws", ServiceID: "api-key:" + key}
		if key == "" {
			subject = execution.Subject{WorkspaceID: "ws", UserID: "user"}
		}
		request = request.WithContext(execution.WithSubject(request.Context(), subject))
		c := echo.New().NewContext(request, rec)
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		if key != "" {
			c.Set(authSourceContextKey, authSourceAPIKey)
			c.Set(apiKeyIDContextKey, key)
			role := "service"
			if key == "admin-decision-key" {
				role = "admin"
			}
			c.Set("roles", []string{role})
		}
		if len(params) > 0 {
			c.SetParamNames(params[0])
			c.SetParamValues(params[1:]...)
		}
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	assertCode := func(result *httptest.ResponseRecorder, want int) {
		t.Helper()
		if result.Code != want {
			t.Fatalf("status %d, want %d", result.Code, want)
		}
	}
	bind := func(key string) *httptest.ResponseRecorder {
		return call(s.handleBindDecision, "", "/v1/decision-bindings/"+key, map[string]any{"team_id": "team", "workflow_id": "flow", "workflow_version": 1, "contract": ticketContract}, "key_id", key)
	}
	for key, want := range map[string]int{"decision-key": 200, "other-key": 200, "broad-key": 409, "admin-decision-key": 409} {
		if result := bind(key); result.Code != want {
			t.Fatalf("bind %s: %d %s", key, result.Code, result.Body)
		}
	}

	request := func(id, lane string, attempt int) map[string]any {
		return map[string]any{"client_request_id": id, "lane": lane, "input": ticketInput(attempt)}
	}
	const firstID, secondID, cancelledID = "abcdefab-cdef-4abc-8def-000000000011", "00000000-0000-0000-0000-000000000012", "abcdefab-cdef-4abc-8def-000000000013"
	assertCode(call(s.handleAdmitDecision, "admin-decision-key", "/v1/decisions", request(firstID, "customer:4711", 1)), 403)
	first := call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(firstID, "customer:4711", 1))
	if first.Code != 201 || first.Header().Get("X-Decision-ID") == "" || len(first.Header().Get("X-Decision-Input-Hash")) != 64 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	replay := call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(strings.ToUpper(firstID), "customer:4711", 1))
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay %d %s", replay.Code, replay.Body)
	}
	assertCode(call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(firstID, "customer:4711", 2)), 409)
	assertCode(call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(firstID, "customer:4712", 1)), 409)
	undeclared := request(secondID, "customer:9", 1)
	undeclared["input"].(map[string]any)["internal_notes"] = "private"
	assertCode(call(s.handleAdmitDecision, "decision-key", "/v1/decisions", undeclared), 400)
	if result := call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(secondID, "customer:4711", 1)); result.Code != 409 || result.Header().Get("X-Decision-Active-Request-ID") != firstID {
		t.Fatalf("overlapping lane accepted %d %s", result.Code, result.Body)
	}
	assertCode(call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(secondID, "customer:42", 1)), 201)
	assertCode(call(s.handleReadDecision, "other-key", "/v1/decisions/id", nil, "decision_id", first.Header().Get("X-Decision-ID")), 404)
	read := call(s.handleReadDecision, "decision-key", "/v1/decisions/id", nil, "decision_id", first.Header().Get("X-Decision-ID"))
	var body struct {
		Lane  string `json:"lane"`
		Trace struct {
			Nodes []decisionTraceNode `json:"nodes"`
		} `json:"trace"`
	}
	if read.Code/100 != 2 || json.Unmarshal(read.Body.Bytes(), &body) != nil || body.Lane != "customer:4711" {
		t.Fatalf("read %d %s", read.Code, read.Body)
	}
	if read.Code == 200 && (len(body.Trace.Nodes) != 1 || body.Trace.Nodes[0].NodeID != "worker") {
		t.Fatalf("trace missing the model node: %s", read.Body)
	}
	assertCode(call(s.handleCancelDecision, "decision-key", "/v1/decisions:cancel", map[string]string{"client_request_id": firstID, "lane": "customer:42"}), 409)
	assertCode(call(s.handleCancelDecision, "decision-key", "/v1/decisions:cancel", map[string]string{"client_request_id": strings.ToUpper(cancelledID), "lane": "customer:3"}), 200)
	assertCode(call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(cancelledID, "customer:3", 1)), 409)
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_decision_admissions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unexpected admissions %d %v", count, err)
	}
	// Concurrent replicas race in the database lock, not just this process.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, id := range []string{"00000000-0000-0000-0000-000000000021", "00000000-0000-0000-0000-000000000022"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			codes <- call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(id, "customer:race", 1)).Code
		}(id)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("lane race outcomes %v", counts)
	}
	originalSchema := ticketContract["input_schema"]
	defer func() { ticketContract["input_schema"] = originalSchema }()
	ticketContract["input_schema"] = map[string]any{"type": "object", "maxProperties": 0}
	rebound, replay := bind("decision-key"), call(s.handleAdmitDecision, "decision-key", "/v1/decisions", request(firstID, "customer:4711", 1))
	if rebound.Code != 200 || replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("replay changed after rebinding")
	}
	assertCode(call(s.handleDeleteAPIKey, "", "/v1/auth/api-keys/decision-key", nil, "id", "decision-key"), http.StatusNoContent)
	var bindings, admissions, cancellations int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM weave_decision_bindings WHERE api_key_id='decision-key'),(SELECT count(*) FROM weave_decision_admissions WHERE api_key_id='decision-key'),(SELECT count(*) FROM weave_decision_cancellations WHERE api_key_id='decision-key')`).Scan(&bindings, &admissions, &cancellations); err != nil || bindings != 0 || admissions != 3 || cancellations != 1 {
		t.Fatalf("revocation removed frozen decision history or kept a live binding: %d %d %d %v", bindings, admissions, cancellations, err)
	}
}
