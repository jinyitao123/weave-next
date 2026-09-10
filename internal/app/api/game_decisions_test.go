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

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/labstack/echo/v4"
)

func TestGameWorkflowRejectsToolMemoryAndGraphExpansion(t *testing.T) {
	valid := frozen.ArtifactPayloadV1{GraphDefinition: json.RawMessage(`{"nodes":[{"type":"worker"},{"type":"deliver"}]}`), Bundles: []frozen.FrozenExecutionBundle{{PrimaryModel: frozen.FrozenModelBinding{ProviderID: "provider", ModelID: "frozen-model"}, Agent: frozen.FrozenAgentRecord{Role: "worker", Engine: "loom", Model: "frozen-model", Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}, OutputSchema: json.RawMessage(`{"type":"object"}`)}}}}
	if err := validateGameWorkflow(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*frozen.ArtifactPayloadV1){
		func(p *frozen.ArtifactPayloadV1) {
			p.GraphDefinition = json.RawMessage(`{"nodes":[{"type":"worker"},{"type":"worker"},{"type":"deliver"}]}`)
		},
		func(p *frozen.ArtifactPayloadV1) { p.Bundles[0].Agent.Permissions.Deny = nil },
		func(p *frozen.ArtifactPayloadV1) {
			p.Bundles[0].Agent.MemoryConfig = &frozen.FrozenMemoryConfig{Enabled: true}
		},
		func(p *frozen.ArtifactPayloadV1) { p.Bundles[0].Agent.Engine = "codex" },
		func(p *frozen.ArtifactPayloadV1) { p.Bundles[0].Agent.Fallback.Models = []string{"other"} },
	} {
		raw, _ := json.Marshal(valid)
		var candidate frozen.ArtifactPayloadV1
		_ = json.Unmarshal(raw, &candidate)
		mutate(&candidate)
		if validateGameWorkflow(candidate) == nil {
			t.Fatal("expanded decision authority accepted")
		}
	}
}

func gameTestInput(room string) map[string]any {
	return map[string]any{"schema_version": "guandan-decision-v1", "room_id": room, "round_id": "round-private", "seat": "south", "sequence": 0, "policy_revision": "rule-1", "published_version": "ontology-v1", "state_hash": "state", "candidate_set_hash": "candidates", "own_hand": []any{}, "public_history": []any{}, "remaining_counts": map[string]int{"south": 1, "east": 2, "north": 3, "west": 4}, "teammate_seat": "north", "strategy": "balanced", "legal_candidates": []any{map[string]any{"candidate_id": "candidate-a"}}}
}

func TestGameInputAndChoiceRejectInjectionAndInvalidOutput(t *testing.T) {
	input := gameTestInput("room-1")
	raw, _ := json.Marshal(input)
	if _, err := validateGameInput(raw); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(map[string]any){func(v map[string]any) { v["other_hands"] = map[string]any{} }, func(v map[string]any) { v["room_id"] = "room-7" }, func(v map[string]any) { v["sequence"] = -1 }, func(v map[string]any) {
		v["legal_candidates"] = []any{map[string]any{"candidate_id": "same"}, map[string]any{"candidate_id": "same"}}
	}} {
		v := gameTestInput("room-1")
		mutation(v)
		b, _ := json.Marshal(v)
		if _, err := validateGameInput(b); err == nil {
			t.Fatal("invalid private input accepted")
		}
	}
	for _, output := range []string{`{"candidate_id":"outside","rationale":"x","confidence":0.5}`, `{"candidate_id":"candidate-a","rationale":"x","confidence":1.1}`, `{"candidate_id":"candidate-a","rationale":"x"}`, `{"candidate_id":"candidate-a","rationale":"x","confidence":0.5,"state_patch":{}}`, `{"candidate_id":"candidate-a","rationale":"x","confidence":0.5} {}`} {
		if validateGameChoice(json.RawMessage(output), raw) == nil {
			t.Fatalf("invalid output accepted %s", output)
		}
	}
	if err := validateGameChoice(json.RawMessage(`{"candidate_id":"candidate-a","rationale":"x","confidence":0.5}`), raw); err != nil {
		t.Fatal(err)
	}
}

func TestGameServiceAdmissionIdempotencyRoomIsolationAndCancellationRealPG(t *testing.T) {
	dependencies := []frozen.FrozenDependencyRef{}
	digest, _ := frozen.ComputeManifestHash(dependencies)
	bundle := frozen.FrozenExecutionBundle{SchemaVersion: 1, FactoryKey: frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "1", CompilerABI: "weave-graph-abi-v1"}, Agent: frozen.FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "worker", AgentVersion: 1, Name: "worker", Role: "worker", Engine: "loom", Model: "model", GraphType: "standard", FactoryInput: json.RawMessage(`{}`), Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}, OutputSchema: json.RawMessage(`{"type":"object"}`)}, PrimaryModel: frozen.FrozenModelBinding{SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "provider", ProviderRevision: 1, ModelID: "model", BaseURL: "https://provider.example", CredentialRef: frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}}, Dependencies: frozen.FrozenDependencyManifest{SchemaVersion: 1, Dependencies: dependencies, ManifestHash: digest}, Capability: frozen.CapabilityManifest{SchemaVersion: 2, Role: "worker", AgentContentHash: strings.Repeat("b", 64)}}
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"worker","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"worker","type":"worker","inputs":{"task":{"expected_type":"text","value":{"source":"run_input","path":""}}},"output":{"type":"text"},"config":{"agent_id":"worker","agent_version":1,"kind":"consult","result_requirement":"choose a candidate"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"worker","path":""}}}],"edges":[{"id":"delivery","from_node_id":"worker","to_node_id":"deliver","route":"success"}]}`)
	s, pool := newTeamDispatchTestServerWithGraph(t, graph, bundle)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('worker','ws','worker','worker','{}'); INSERT INTO weave_team_workers(workspace_id,team_id,worker_agent_id,allowed_kinds,default_kind) VALUES('ws','team','worker',ARRAY['consult'],'consult')`); err != nil {
		t.Fatal(err)
	}
	s.teamRunCancel = &teamrun.CancelService{Transactions: pool, Runs: teamrun.NewPGStore(), Tasks: s.Tasks}
	_, err := pool.Exec(context.Background(), `INSERT INTO weave_api_keys(id,tenant_id,name,key_hash,role,scopes) VALUES('game-key','ws','game','game-hash','service',ARRAY['game_decisions']),('other-key','ws','other','other-hash','service',ARRAY['game_decisions']); INSERT INTO weave_game_decision_bindings(workspace_id,api_key_id,team_id,workflow_id,workflow_version,created_by) VALUES('ws','game-key','team','flow',1,'user'),('ws','other-key','team','flow',1,'user');`)
	if err != nil {
		t.Fatal(err)
	}
	call := func(handler echo.HandlerFunc, key, path string, body any, params ...string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)), rec)
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		c.Set(authSourceContextKey, authSourceAPIKey)
		c.Set(apiKeyIDContextKey, key)
		if len(params) > 0 {
			c.SetParamNames("decision_id")
			c.SetParamValues(params...)
		}
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	request := func(id, room string) map[string]any {
		return map[string]any{"client_request_id": id, "input": gameTestInput(room)}
	}
	const firstID = "00000000-0000-0000-0000-000000000011"
	const secondID = "00000000-0000-0000-0000-000000000012"
	const cancelledID = "00000000-0000-0000-0000-000000000013"
	first := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(firstID, "room-1"))
	if first.Code != 201 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	replay := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(firstID, "room-1"))
	if replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay %d %s", replay.Code, replay.Body)
	}
	changed := request(firstID, "room-1")
	changed["input"].(map[string]any)["sequence"] = 1
	if result := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", changed); result.Code != 409 {
		t.Fatalf("changed input %d", result.Code)
	}
	if result := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(secondID, "room-1")); result.Code != 409 || result.Header().Get("X-Game-Active-Request-ID") != firstID {
		t.Fatalf("overlapping room accepted %d %s", result.Code, result.Body)
	}
	if result := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(secondID, "room-2")); result.Code != 201 {
		t.Fatalf("independent room %d %s", result.Code, result.Body)
	}
	if result := call(s.handleReadGameDecision, "other-key", "/v1/game-decisions/id", nil, first.Header().Get("X-Game-Decision-ID")); result.Code != 404 {
		t.Fatal("another key read private decision")
	}
	if result := call(s.handleCancelGameDecision, "game-key", "/v1/game-decisions:cancel", map[string]string{"client_request_id": firstID, "room_id": "room-2"}); result.Code != 409 {
		t.Fatal("wrong-room cancellation accepted")
	}
	if result := call(s.handleCancelGameDecision, "game-key", "/v1/game-decisions:cancel", map[string]string{"client_request_id": cancelledID, "room_id": "room-3"}); result.Code != 200 {
		t.Fatalf("early cancel %d %s", result.Code, result.Body)
	}
	if result := call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(cancelledID, "room-3")); result.Code != 409 {
		t.Fatal("cancelled request admitted after delayed arrival")
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM weave_game_decision_admissions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unexpected admissions %d %v", count, err)
	}
	// Concurrent replicas race in the database lock, not just this process.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, id := range []string{"00000000-0000-0000-0000-000000000021", "00000000-0000-0000-0000-000000000022"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			codes <- call(s.handleAdmitGameDecision, "game-key", "/v1/game-decisions", request(id, "room-4")).Code
		}(id)
	}
	wg.Wait()
	close(codes)
	created, conflicts := 0, 0
	for code := range codes {
		if code == 201 {
			created++
		}
		if code == 409 {
			conflicts++
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("room race created=%d conflicts=%d", created, conflicts)
	}
}
