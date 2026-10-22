package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/labstack/echo/v4"
)

// TestDevelopmentModelProviderOmittedSettingsKeepCurrentRealPG checks that
// json_object_mode and protocol behave like api_key on a shared provider: an
// omitted field keeps the stored value, an explicit one replaces it, and a new
// provider starts from OpenAI chat with strict JSON Schema output. The model
// endpoint records the response_format each structured call actually sends.
func TestDevelopmentModelProviderOmittedSettingsKeepCurrentRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','ws','developer','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "provider-settings-test"}
	s.Credentials = credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	s.Models = llmrouter.NewResolver(llmrouter.New("settings-model"))
	s.Models.SetSource(s.Credentials)
	s.registerRoutes()
	const key = "provider-settings-key"
	var mu sync.Mutex
	var formats []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		formats = append(formats, request.ResponseFormat.Type)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer endpoint.Close()
	put := func(id string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPut, "/v1/development/model-providers/"+id, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: "developer", TenantID: "ws", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString([]byte(s.Config.JWTSecret))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Echo.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), key) {
			t.Fatalf("PUT %s returned %d: %s", id, rec.Code, rec.Body.String())
		}
	}
	stored := func(id string) (string, bool) {
		t.Helper()
		var protocol string
		var jsonObjectMode bool
		if err := pool.QueryRow(ctx, `SELECT protocol,json_object_mode FROM weave_provider_credentials WHERE workspace_id='ws' AND id=$1`, id).Scan(&protocol, &jsonObjectMode); err != nil {
			t.Fatal(err)
		}
		return protocol, jsonObjectMode
	}
	sentFormat := func() string {
		t.Helper()
		actor := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "developer"})
		granted := credentials.WithServiceReferenceAuthorization(actor, func(_ context.Context, _ execution.Subject, ref frozen.CredentialReference) error {
			if ref.WorkspaceID == "ws" && ref.ResourceID == "shared" && ref.ServiceID == "workspace-provider:shared" {
				return nil
			}
			return execution.ErrSubjectMismatch
		})
		llm, err := s.Models.ForWorkspace(granted, "ws")
		if err != nil {
			t.Fatal(err)
		}
		schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
		if _, err := llm.Chat(granted, contract.ChatRequest{Model: "settings-model", Schema: &schema,
			Messages: []contract.Message{{Role: "user", Content: "Reply in JSON with the field ok."}}}); err != nil {
			t.Fatalf("structured call failed: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return formats[len(formats)-1]
	}

	body := map[string]any{"name": "Shared", "base_url": endpoint.URL, "models": []string{"settings-model"}, "api_key": key}
	put("shared", body)
	if protocol, jsonMode := stored("shared"); protocol != llmrouter.ProtocolOpenAI || jsonMode {
		t.Fatalf("new provider stored protocol %q, json_object_mode %v", protocol, jsonMode)
	}
	if got := sentFormat(); got != "json_schema" {
		t.Fatalf("new provider sent response_format %q", got)
	}
	delete(body, "api_key")
	body["json_object_mode"] = true
	put("shared", body)
	if got := sentFormat(); got != "json_object" {
		t.Fatalf("explicit json_object_mode sent response_format %q", got)
	}
	delete(body, "json_object_mode")
	body["name"] = "Shared renamed"
	put("shared", body)
	if _, jsonMode := stored("shared"); !jsonMode {
		t.Fatal("omitting json_object_mode turned it off")
	}
	if got := sentFormat(); got != "json_object" {
		t.Fatalf("kept json_object_mode sent response_format %q", got)
	}
	body["json_object_mode"] = false
	put("shared", body)
	if got := sentFormat(); got != "json_schema" {
		t.Fatalf("explicit false sent response_format %q", got)
	}

	decision := map[string]any{"name": "Decision", "base_url": endpoint.URL, "models": []string{"decision-model"}, "protocol": llmrouter.ProtocolSystemOne}
	put("decision", decision)
	delete(decision, "protocol")
	decision["name"] = "Decision renamed"
	put("decision", decision)
	if protocol, _ := stored("decision"); protocol != llmrouter.ProtocolSystemOne {
		t.Fatalf("omitting protocol changed a System One provider to %q", protocol)
	}
	decision["protocol"] = llmrouter.ProtocolOpenAI
	put("decision", decision)
	if protocol, _ := stored("decision"); protocol != llmrouter.ProtocolOpenAI {
		t.Fatalf("explicit protocol stored %q", protocol)
	}
}
