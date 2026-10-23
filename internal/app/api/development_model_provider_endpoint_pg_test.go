package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

func TestDevelopmentModelProviderChatPathRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','ws','developer','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "provider-chat-path-test"}
	s.Credentials = credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	s.Models = llmrouter.NewResolver(llmrouter.New("glm"))
	s.Models.SetSource(s.Credentials)
	s.registerRoutes()
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path != "/api/paas/v4/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: "developer", TenantID: "ws", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString([]byte(s.Config.JWTSecret))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Echo.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// The preview resolves an address without storing anything, and an
	// ambiguous one comes back with the split to confirm.
	status, preview := call(http.MethodPost, "/v1/development/model-providers/endpoint", map[string]any{"base_url": upstream.URL + "/api/paas/v4", "chat_path": "/chat/completions"})
	if status != http.StatusOK || preview["chat_url"] != upstream.URL+"/api/paas/v4/chat/completions" {
		t.Fatalf("preview = %d %v", status, preview)
	}
	status, refused := call(http.MethodPost, "/v1/development/model-providers/endpoint", map[string]any{"base_url": upstream.URL + "/v1", "chat_path": "/v1/chat/completions"})
	suggestion, _ := refused["suggestion"].(map[string]any)
	if status != http.StatusUnprocessableEntity || refused["reason"] != "duplicate_path_segment" || suggestion["chat_url"] != upstream.URL+"/v1/chat/completions" {
		t.Fatalf("ambiguous preview = %d %v", status, refused)
	}

	// A provider saved with an explicit path stores it and its probe reaches it.
	status, saved := call(http.MethodPut, "/v1/development/model-providers/glm", map[string]any{"name": "GLM", "base_url": upstream.URL + "/api/paas/v4", "chat_path": "/chat/completions", "models": []string{"glm"}})
	if status != http.StatusOK || saved["chat_url"] != upstream.URL+"/api/paas/v4/chat/completions" {
		t.Fatalf("save = %d %v", status, saved)
	}
	status, probe := call(http.MethodPost, "/v1/development/model-providers/glm/probe", map[string]any{"model": "glm"})
	if status != http.StatusOK || probe["ok"] != true || probe["chat_url"] != upstream.URL+"/api/paas/v4/chat/completions" {
		t.Fatalf("probe = %d %v (paths %v)", status, probe, seen)
	}
	// Re-saving without a path keeps the recorded one.
	if status, kept := call(http.MethodPut, "/v1/development/model-providers/glm", map[string]any{"name": "GLM renamed", "base_url": upstream.URL + "/api/paas/v4", "models": []string{"glm"}}); status != http.StatusOK || kept["chat_path"] != "/chat/completions" {
		t.Fatalf("re-save = %d %v", status, kept)
	}

	// A new provider without a path records the address its client always used.
	status, fresh := call(http.MethodPut, "/v1/development/model-providers/sdk-style", map[string]any{"name": "SDK style", "base_url": "https://api.example.test/v1", "models": []string{"sdk-model"}})
	if status != http.StatusOK || fresh["base_url"] != "https://api.example.test" || fresh["chat_path"] != "/v1/chat/completions" {
		t.Fatalf("new provider without path = %d %v", status, fresh)
	}

	// A provider saved before paths were recorded keeps that form when re-saved
	// without one, so its revision and address do not change.
	service := "workspace-provider:legacy"
	legacyCtx := credentials.WithServiceReferenceAuthorization(execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "developer"}),
		func(context.Context, execution.Subject, frozen.CredentialReference) error { return nil })
	before, err := s.Credentials.UpsertRevision(legacyCtx, "ws", llmrouter.ProviderConfig{CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: service,
		ID: "legacy", Name: "Legacy", BaseURL: "https://legacy.example.test/v1", Protocol: llmrouter.ProtocolOpenAI, Models: []string{"legacy-model"}})
	if err != nil || before.Head.ChatPath != "" {
		t.Fatalf("legacy provider = %+v %v", before.Head, err)
	}
	status, legacy := call(http.MethodPut, "/v1/development/model-providers/legacy", map[string]any{"name": "Legacy", "base_url": "https://legacy.example.test/v1", "models": []string{"legacy-model"}})
	if status != http.StatusOK || legacy["chat_path"] != "" || legacy["chat_url"] != "https://legacy.example.test/v1/chat/completions" {
		t.Fatalf("legacy re-save = %d %v", status, legacy)
	}
	var revisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_provider_revisions WHERE workspace_id='ws' AND provider_id='legacy'`).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("legacy re-save made a new revision: %d %v", revisions, err)
	}
	status, list := call(http.MethodGet, "/v1/development/model-providers", nil)
	providers, _ := list["providers"].([]any)
	if status != http.StatusOK || len(providers) != 3 {
		t.Fatalf("list = %d %v", status, list)
	}
	for _, raw := range providers {
		provider := raw.(map[string]any)
		if provider["chat_url"] == "" || provider["chat_url"] == nil {
			t.Fatalf("listed provider without its address: %v", provider)
		}
	}
}
