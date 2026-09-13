package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

func TestCapabilityHTTPToLoomRuntimeRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := uri.Query()
	q.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	uri.RawQuery = q.Encode()
	persisted, err := pgstore.New(uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer persisted.Close()
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workspaces(id,slug,name) VALUES('cap-ws','cap-ws','cap-ws');
 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('dev','cap-ws','dev','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var materialRoleCalls, ruleRoleCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model    string
			Tools    []any
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "test-model" || len(req.Tools) != 0 {
			t.Errorf("unexpected provider request: %+v", req)
		}
		generation := false
		materialRole, ruleRole := false, false
		for _, message := range req.Messages {
			generation = generation || strings.Contains(message.Content, "企业能力设计师")
			materialRole = materialRole || strings.Contains(message.Content, "资料核对员")
			ruleRole = ruleRole || strings.Contains(message.Content, "规则复核员")
		}
		content := `{"value":7}`
		if generation {
			content = `{"name":"资料核对","description":"由两类核对人员分别检查材料，再汇总形成核对结论。","input_fields":[{"key":"material","label":"待处理材料","description":"需要核对的业务资料","type":"string","required":true}],"output_fields":[{"key":"summary","label":"核对摘要","description":"汇总后的核对结论","type":"object","required":true}],"roles":[{"name":"资料核对员","responsibilities":"检查材料完整性"},{"name":"规则复核员","responsibilities":"检查材料是否符合规则"}],"steps":[{"name":"材料检查","role_index":0,"kind":"worker","instruction":"核对所给材料的完整性，并给出检查项数。","depends_on":[],"uses_input":true,"uses_steps":[]},{"name":"规则检查","role_index":1,"kind":"worker","instruction":"复核材料是否满足约定的规则，并给出检查项数。","depends_on":[],"uses_input":true,"uses_steps":[]},{"name":"核对摘要","role_index":0,"kind":"collect","instruction":"","depends_on":[0,1],"uses_input":false,"uses_steps":[0,1]}]}`
		} else {
			if materialRole == ruleRole {
				t.Error("model request did not preserve one declared role")
			}
			if materialRole {
				materialRoleCalls.Add(1)
			}
			if ruleRole {
				ruleRoleCalls.Add(1)
			}
		}
		payload, _ := json.Marshal(map[string]any{"id": "test", "model": "test-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer provider.Close()
	router := llmrouter.New("test-model")
	router.RegisterProvider(llmrouter.ProviderConfig{ID: "test", BaseURL: provider.URL, APIKey: "test", Models: []string{"test-model"}})
	store := capabilities.NewPGStore(persisted.Pool())
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "integration-secret"}, Pool: persisted.Pool(), Store: persisted, Models: llmrouter.NewResolver(router), Capabilities: capabilities.NewService(store, store), UserStore: users.NewStore(persisted.Pool())}
	s.StoreExt = storeext.New(persisted.Pool())
	s.CapabilityAccess = capabilities.NewAccessStore(persisted.Pool())
	s.registerRoutes()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{TenantID: "cap-ws", UserID: "dev", Roles: []string{"admin"}, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString([]byte(s.Config.JWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any, expected int) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.Echo.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	object := json.RawMessage(`{"type":"object"}`)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "arithmetic", Name: "资料核对示例", InputSchema: object, OutputSchema: object, Runtime: capability.RuntimeRequirement{Engine: "loom", Model: "test-model"},
		Roles: []capability.Role{{ID: "material-reviewer", Name: "资料核对员", Description: "核对材料的完整性"}, {ID: "rule-reviewer", Name: "规则复核员", Description: "复核材料是否满足规则"}},
		Steps: []capability.Step{
			{ID: "a", Name: "材料检查", RoleID: "material-reviewer", Kind: capability.StepWorker, Instruction: "核对所给材料的完整性，并给出检查项数。", OutputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]}`)},
			{ID: "b", Name: "规则检查", RoleID: "rule-reviewer", Kind: capability.StepWorker, Instruction: "复核材料是否满足约定的规则，并给出检查项数。"},
			{ID: "merge", Name: "核对摘要", RoleID: "material-reviewer", Kind: capability.StepTransform, InputBindings: map[string]capability.ValueRef{"材料检查项数": {Source: "step_output", StepID: "a", Path: "/value"}, "规则检查项数": {Source: "step_output", StepID: "b", Path: "/value"}}},
		}, Relations: []capability.Relation{{From: "a", To: "merge", Kind: capability.RelationJoin}, {From: "b", To: "merge", Kind: capability.RelationJoin}}}
	call("POST", "/v1/capabilities/drafts", map[string]any{"definition": d}, 202)
	call("POST", "/v1/capabilities/arithmetic/versions/1/publish", map[string]any{}, 201)
	request := map[string]any{"request_id": "one", "input": map[string]any{"material": "input"}}
	accepted := call("POST", "/v1/capabilities/arithmetic/versions/1/invocations", request, 202)
	var id string
	_ = json.Unmarshal(accepted["invocation_id"], &id)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.serveCapabilityTasks(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		record := call("GET", "/v1/invocations/"+id, nil, 200)
		if string(record["status"]) == `"completed"` {
			var result struct {
				Merge struct {
					Left  int `json:"材料检查项数"`
					Right int `json:"规则检查项数"`
				}
			}
			if err := json.Unmarshal(record["result"], &result); err != nil || result.Merge.Left != 7 || result.Merge.Right != 7 {
				t.Fatalf("result=%s err=%v", record["result"], err)
			}
			break
		}
		if string(record["status"]) == `"failed"` || time.Now().After(deadline) {
			var executionError string
			_ = pool.QueryRow(t.Context(), `SELECT COALESCE(error,'') FROM weave_capability_invocations WHERE invocation_id=$1`, id).Scan(&executionError)
			t.Fatalf("run did not complete: %s; execution error: %s", mustCapabilityJSON(record), executionError)
		}
		time.Sleep(20 * time.Millisecond)
	}
	replay := call("POST", "/v1/capabilities/arithmetic/versions/1/invocations", request, 202)
	if string(replay["invocation_id"]) != string(accepted["invocation_id"]) || calls.Load() != 2 {
		t.Fatal("replay executed again")
	}
	if materialRoleCalls.Load() != 1 || ruleRoleCalls.Load() != 1 {
		t.Fatal("declared roles were not executed independently")
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_step_runs WHERE workspace_id='cap-ws' AND invocation_id=$1`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("Loom run bindings=%d err=%v", count, err)
	}
	var markers int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_run_attempt_leases WHERE workspace_id='cap-ws'`).Scan(&markers); err != nil || markers != 2 {
		t.Fatalf("Loom attempt leases=%d err=%v", markers, err)
	}
	if strings.Contains(string(mustCapabilityJSON(replay)), "test-model") {
		t.Fatal("internal runtime details leaked")
	}
	// Optional browser acceptance attaches a real Workbench Host to this isolated
	// server and database. The token is written only to a private temporary file.
	if directory := os.Getenv("WEAVE_CAPABILITY_BROWSER_DIR"); directory != "" {
		backend := httptest.NewServer(s.Echo)
		defer backend.Close()
		raw, _ := json.Marshal(map[string]string{"url": backend.URL, "token": token})
		if err := os.WriteFile(filepath.Join(directory, "connection.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(120 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(directory, "done")); err == nil {
				break
			}
			if time.Now().After(expires) {
				t.Fatal("browser acceptance did not finish")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if calls.Load() != 4 {
			t.Fatalf("browser model calls=%d expected=4", calls.Load())
		}
		var debugCount int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_debug_snapshots WHERE workspace_id='cap-ws'`).Scan(&debugCount); err != nil || debugCount != 0 {
			t.Fatalf("browser path created debug snapshots=%d err=%v", debugCount, err)
		}
	}
}

func mustCapabilityJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprint(err))
	}
	return raw
}
