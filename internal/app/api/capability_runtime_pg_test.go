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
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
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
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model string
			Tools []any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "test-model" || len(req.Tools) != 0 {
			t.Errorf("unexpected provider request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"{\"value\":7}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer provider.Close()
	router := llmrouter.New("test-model")
	router.RegisterProvider(llmrouter.ProviderConfig{ID: "test", BaseURL: provider.URL, APIKey: "test", Models: []string{"test-model"}})
	store := capabilities.NewPGStore(persisted.Pool())
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "integration-secret"}, Pool: persisted.Pool(), Store: persisted, Models: llmrouter.NewResolver(router), Capabilities: capabilities.NewService(store, store), UserStore: users.NewStore(persisted.Pool())}
	s.StoreExt = storeext.New(persisted.Pool())
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
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "arithmetic", Name: "Arithmetic", InputSchema: object, OutputSchema: object, Runtime: capability.RuntimeRequirement{Engine: "loom", Model: "test-model"},
		Roles: []capability.Role{{ID: "r", Name: "Calculator"}},
		Steps: []capability.Step{
			{ID: "a", Name: "A", RoleID: "r", Kind: capability.StepWorker, Instruction: "Calculate value 7", OutputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]}`)},
			{ID: "b", Name: "B", RoleID: "r", Kind: capability.StepWorker, Instruction: "Calculate value 7"},
			{ID: "merge", Name: "Merge", RoleID: "r", Kind: capability.StepTransform, InputBindings: map[string]capability.ValueRef{"left": {Source: "step_output", StepID: "a", Path: "/value"}, "right": {Source: "step_output", StepID: "b", Path: "/value"}}},
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
					Left  int
					Right int
				}
			}
			if err := json.Unmarshal(record["result"], &result); err != nil || result.Merge.Left != 7 || result.Merge.Right != 7 {
				t.Fatalf("result=%s err=%v", record["result"], err)
			}
			break
		}
		if string(record["status"]) == `"failed"` || time.Now().After(deadline) {
			t.Fatalf("run did not complete: %s", mustCapabilityJSON(record))
		}
		time.Sleep(20 * time.Millisecond)
	}
	replay := call("POST", "/v1/capabilities/arithmetic/versions/1/invocations", request, 202)
	if string(replay["invocation_id"]) != string(accepted["invocation_id"]) || calls.Load() != 2 {
		t.Fatal("replay executed again")
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
	}
}

func mustCapabilityJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprint(err))
	}
	return raw
}
