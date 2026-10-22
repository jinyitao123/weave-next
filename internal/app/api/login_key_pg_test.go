package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestLoginKeySignsInAsTheAdministratorRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "login-key-test", LoginKey: "admin", LoginKeyDefault: true}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()
	call := func(method, path, bearer, body string) (int, map[string]any, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest(method, host.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// The console only takes requests its own pages make.
		req.Header.Set(adminRequestHeader, "1")
		req.Header.Set("Origin", host.URL)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return resp.StatusCode, decoded, resp
	}

	// A deployment that set no key has the default one and says so.
	if status, body, _ := call("GET", "/v1/admin/config", "", ""); status != http.StatusOK || body["default_key"] != true {
		t.Fatalf("config: %d %v", status, body)
	}
	status, session, response := call("POST", "/v1/admin/session", "", `{"api_key":"admin"}`)
	if status != http.StatusOK || session["role"] != "admin" || session["source"] != "api_key" || session["default_key"] != true || len(response.Cookies()) == 0 {
		t.Fatalf("console sign-in with the default key: %d %v", status, session)
	}
	if status, body, _ := call("GET", "/v1/admin/session", "admin", ""); status != http.StatusOK || body["role"] != "admin" || body["default_key"] != true {
		t.Fatalf("session read with the key: %d %v", status, body)
	}
	// The key stands where a session would, on the MCP entry as elsewhere.
	status, listed, _ := call("POST", "/v1/mcp", "admin", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"weave_team_list","arguments":{"purpose":"develop"}}}`)
	result, _ := listed["result"].(map[string]any)
	if status != http.StatusOK || result == nil || result["isError"] == true {
		t.Fatalf("MCP call with the key: %d %v", status, listed)
	}
	for _, wrong := range []string{"admin2", "Admin", ""} {
		if status, _, _ := call("GET", "/v1/admin/session", wrong, ""); status != http.StatusUnauthorized {
			t.Fatalf("key %q: %d", wrong, status)
		}
	}
	if status, _, _ := call("POST", "/v1/admin/session", "", `{"api_key":"admin2"}`); status != http.StatusUnauthorized {
		t.Fatalf("console sign-in with a wrong key: %d", status)
	}

	// A key set at deployment replaces the default, and no page is told it is the default.
	s.Config.LoginKey, s.Config.LoginKeyDefault = "set-at-deployment", false
	if status, _, _ := call("POST", "/v1/admin/session", "", `{"api_key":"admin"}`); status != http.StatusUnauthorized {
		t.Fatalf("the default key after one was set: %d", status)
	}
	if status, body, _ := call("POST", "/v1/admin/session", "", `{"api_key":"set-at-deployment"}`); status != http.StatusOK || body["default_key"] != nil {
		t.Fatalf("console sign-in with the deployment key: %d %v", status, body)
	}
	if status, body, _ := call("GET", "/v1/admin/config", "", ""); status != http.StatusOK || body["default_key"] != nil {
		t.Fatalf("config after a key was set: %d %v", status, body)
	}

	// Without an administrator in the configured workspace there is nobody to sign in as.
	s.Config.LoginWorkspace = "no-such-workspace"
	if status, _, _ := call("GET", "/v1/admin/session", "set-at-deployment", ""); status != http.StatusUnauthorized {
		t.Fatalf("key without an administrator: %d", status)
	}
	if status, _, _ := call("POST", "/v1/admin/session", "", `{"api_key":"set-at-deployment"}`); status != http.StatusUnauthorized {
		t.Fatalf("console sign-in without an administrator: %d", status)
	}
}

// TestLoginKeyManagesCapabilityAppsRealPG checks that the deployment login key
// manages capability applications as it does every other administrator
// route, while an application credential still cannot reach manage.
func TestLoginKeyManagesCapabilityAppsRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "login-key-apps-test", LoginKey: "deployment-login-key"}
	s.CapabilityAccess = capabilities.NewAccessStore(pool)
	s.registerRoutes()
	call := func(method, path, bearer, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		s.Echo.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), s.Config.LoginKey) {
			t.Fatalf("%s %s disclosed the login key", method, path)
		}
		return rec.Code, rec.Body.String()
	}
	if status, body := call("GET", "/v1/capability-apps", "deployment-login-key", ""); status != http.StatusOK {
		t.Fatalf("list applications with the login key: %d %s", status, body)
	}
	status, body := call("POST", "/v1/capability-apps/actions", "deployment-login-key", `{"action":"create","name":"Operator app"}`)
	var app struct {
		ID string `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &app) != nil || app.ID == "" {
		t.Fatalf("create an application with the login key: %d %s", status, body)
	}
	status, body = call("POST", "/v1/capability-apps/actions", "deployment-login-key",
		`{"action":"issue","app_id":"`+app.ID+`","name":"Operator key","scopes":["invoke","read"]}`)
	var issued struct {
		Key string `json:"key"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &issued) != nil || !strings.HasPrefix(issued.Key, capabilities.CredentialPrefix) {
		t.Fatalf("issue a credential with the login key: %d", status)
	}
	if status, body := call("GET", "/v1/capability-apps", "deployment-login-key", ""); status != http.StatusOK || strings.Contains(body, issued.Key) {
		t.Fatalf("application listing: %d, or it disclosed the issued key", status)
	}
	for _, request := range []struct{ method, path, body string }{
		{"GET", "/v1/capability-apps", ""},
		{"POST", "/v1/capability-apps/actions", `{"action":"create","name":"Escalation"}`},
	} {
		if status, _ := call(request.method, request.path, issued.Key, request.body); status != http.StatusForbidden {
			t.Fatalf("application credential reached manage via %s %s: %d", request.method, request.path, status)
		}
	}
	if status, _ := call("GET", "/v1/capability-apps", "deployment-login-key-wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("a wrong login key managed applications: %d", status)
	}
}
