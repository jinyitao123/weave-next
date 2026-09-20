package api

import (
	"bytes"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCapabilityApplicationHTTPIdentityRotationAndScopesRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workspaces(id,slug,name) VALUES('apps-ws','apps-ws','apps-ws');
 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('author','apps-ws','author','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	store := capabilities.NewPGStore(pool)
	service := capabilities.NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "published", Name: "Published", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "r"}}, Steps: []capability.Step{{ID: "s", Name: "s", RoleID: "r", Kind: capability.StepWorker, Instruction: "run"}}}
	if err := service.SaveDraft(t.Context(), capabilities.DraftRequest{WorkspaceID: "apps-ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(t.Context(), "apps-ws", "published", 1); err != nil {
		t.Fatal(err)
	}
	server := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "test-apps"}, Pool: pool, Capabilities: service, CapabilityAccess: capabilities.NewAccessStore(pool), UserStore: users.NewStore(pool), ProductAuthorizer: productAuthorizerFunc(allowAllProductAuthorizer)}
	server.registerRoutes()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{TenantID: "apps-ws", UserID: "author", Roles: []string{"developer"}, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString([]byte("test-apps"))
	if err != nil {
		t.Fatal(err)
	}
	call := func(key, method, path string, body any, expected int) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Echo.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
		}
		if expected == 204 {
			return nil
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	field := func(result map[string]json.RawMessage, name string) string {
		t.Helper()
		var s string
		if err := json.Unmarshal(result[name], &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	app := call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "create", "name": "Integration"}, 201)
	appID := field(app, "id")
	issued := call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "issue", "app_id": appID, "name": "First", "scopes": []string{"invoke", "read", "cancel"}}, 201)
	key := field(issued, "key")
	var credential capabilities.ApplicationCredential
	if err := json.Unmarshal(issued["credential"], &credential); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"request_id": "same", "input": map[string]any{"x": 1}}
	invokePath := "/v1/capabilities/published/versions/1/invocations"
	call(key, "POST", invokePath, request, 403)
	call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "grant", "app_id": appID, "capability_id": "published", "revision": 1, "enabled": true}, 204)
	first := call(key, "POST", invokePath, request, 202)
	invocationID := field(first, "invocation_id")
	replacement := call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "issue", "app_id": appID, "name": "Replacement", "scopes": []string{"invoke", "read"}}, 201)
	nextKey := field(replacement, "key")
	replay := call(nextKey, "POST", invokePath, request, 202)
	if field(replay, "invocation_id") != invocationID || string(replay["replayed"]) != "true" {
		t.Fatal("rotation lost invocation identity")
	}
	call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "revoke", "app_id": appID, "credential_id": credential.ID}, 204)
	call(key, "GET", "/v1/invocations/"+invocationID, nil, 401)
	call(nextKey, "GET", "/v1/invocations/"+invocationID, nil, 200)
	call(nextKey, "POST", "/v1/invocations/"+invocationID+"/cancel", map[string]any{}, 403)
	call(nextKey, "POST", "/v1/capabilities/drafts", map[string]any{"definition": d}, 403)
	call(nextKey, "POST", "/v1/capabilities/published/debug", map[string]any{}, 403)
	call(nextKey, "GET", "/v1/capability-apps", nil, 403)
	call(nextKey, "GET", "/v1/users", nil, 401)
	otherApp := call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "create", "name": "Other"}, 201)
	other := call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "issue", "app_id": field(otherApp, "id"), "name": "Other key", "scopes": []string{"read"}}, 201)
	call(field(other, "key"), "GET", "/v1/invocations/"+invocationID, nil, 404)
	snapshot := call(token, "GET", "/v1/capability-apps", nil, 200)
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), key) || strings.Contains(string(encoded), nextKey) || strings.Contains(string(encoded), "key_hash") {
		t.Fatal("snapshot leaked key material")
	}
	call(token, "POST", "/v1/capability-apps/actions", map[string]any{"action": "enable", "app_id": appID, "enabled": false}, 204)
	call(nextKey, "GET", "/v1/invocations/"+invocationID, nil, 401)
}
