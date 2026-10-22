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

func TestDevelopmentModelProvidersIsolationAndSecretsRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
	 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','ws','developer','unused','developer');
	 INSERT INTO weave_workspaces(id,slug,name) VALUES('other','other','other');
	 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('other-admin','other','other-admin','unused','admin');`); err != nil {
		t.Fatal(err)
	}
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "organization-model-test"}
	s.Credentials = credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	s.Models = llmrouter.NewResolver(llmrouter.New("test-model"))
	s.Models.SetSource(s.Credentials)
	s.registerRoutes()
	keys := []string{"model-test-key-one", "model-test-key-two"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model               string `json:"model"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			MaxTokens           int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if r.URL.Path != "/v1/chat/completions" || request.Model != "test-model" || request.MaxCompletionTokens != 31 || request.MaxTokens != 0 || r.Header.Get("Authorization") != "Bearer "+keys[1] {
			t.Error("model runtime did not use the configured endpoint, model and replacement key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"model reached"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer provider.Close()
	call := func(user, workspace, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		if user != "" {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: user, TenantID: workspace, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).SignedString([]byte(s.Config.JWTSecret))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		s.Echo.ServeHTTP(rec, req)
		for _, key := range keys {
			if strings.Contains(rec.Body.String(), key) {
				t.Fatal("provider response disclosed a secret")
			}
		}
		return rec
	}
	body := map[string]any{"output_limit_field": "max_completion_tokens", "protocol": "openai-completions", "name": "Organization model", "base_url": provider.URL, "models": []string{"test-model"}, "api_key": keys[0]}
	path := "/v1/development/model-providers/local"
	for _, user := range []string{"", "user-other"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			url := path
			if method == http.MethodGet {
				url = "/v1/development/model-providers"
			}
			rec := call(user, "ws", method, url, body)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("unprivileged request returned %d", rec.Code)
			}
		}
	}
	if rec := call("developer", "ws", http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
	}
	var encrypted string
	if err := pool.QueryRow(ctx, `SELECT api_key_cipher FROM weave_provider_credentials WHERE workspace_id='ws' AND id='local'`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if encrypted == "" || strings.Contains(encrypted, keys[0]) {
		t.Fatal("key not sealed at rest")
	}
	delete(body, "api_key")
	body["name"] = "Renamed model"
	if rec := call("user", "ws", http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("retain key returned %d: %s", rec.Code, rec.Body.String())
	}
	var retained string
	if err := pool.QueryRow(ctx, `SELECT api_key_cipher FROM weave_provider_credentials WHERE workspace_id='ws' AND id='local'`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != encrypted {
		t.Fatal("omitting key changed the stored credential")
	}
	body["api_key"] = keys[1]
	if rec := call("developer", "ws", http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("replace returned %d", rec.Code)
	}
	if rec := call("developer", "ws", http.MethodGet, "/v1/development/model-providers", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Renamed model") || !strings.Contains(rec.Body.String(), "max_completion_tokens") || strings.Contains(rec.Body.String(), "api_key") {
		t.Fatal("list missing provider or exposing a key field")
	}
	if rec := call("other-admin", "other", http.MethodGet, "/v1/development/model-providers", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Renamed model") {
		t.Fatal("list crossed organization")
	}
	if rec := call("developer", "ws", http.MethodPut, "/v1/development/model-providers/duplicate", body); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate model returned %d", rec.Code)
	}
	body["models"] = []string{"other-model"}
	if rec := call("other-admin", "other", http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Fatalf("another organization's provider returned %d", rec.Code)
	}
	for _, invalid := range []string{"https://user:pass@example.test/v1", "https://example.test/v1?key=x", "file:///tmp/models"} {
		body["base_url"] = invalid
		if rec := call("developer", "ws", http.MethodPut, "/v1/development/model-providers/invalid", body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid address returned %d", rec.Code)
		}
	}
	body["base_url"], body["api_key"] = provider.URL+"/v1", credentials.MaskedKey
	if rec := call("developer", "ws", http.MethodPut, "/v1/development/model-providers/masked", body); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("masked placeholder accepted as new key: %d", rec.Code)
	}
	actor := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "developer"})
	granted := credentials.WithServiceReferenceAuthorization(actor, func(_ context.Context, _ execution.Subject, ref frozen.CredentialReference) error {
		if ref.WorkspaceID == "ws" && ref.ResourceID == "local" && ref.ServiceID == "workspace-provider:local" {
			return nil
		}
		return execution.ErrSubjectMismatch
	})
	llm, err := s.Models.ForWorkspace(granted, "ws")
	if err != nil {
		t.Fatal(err)
	}
	response, err := llm.Chat(granted, contract.ChatRequest{Model: "test-model", MaxTokens: 31})
	if err != nil || response.Content != "model reached" {
		t.Fatalf("configured model call failed: %v", err)
	}
}

func TestDevelopmentModelProvidersConcurrentModelClaimRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Credentials = credentials.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	// A slow storage write exposes overlapping model checks without replacing the database.
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION slow_provider_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.1); RETURN NEW; END $$;
	 CREATE TRIGGER slow_provider_insert BEFORE INSERT ON weave_provider_credentials FOR EACH ROW EXECUTE FUNCTION slow_provider_insert();`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan int, 6)
	var workers sync.WaitGroup
	for _, id := range []string{"one", "two", "three", "four", "five", "six"} {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			<-start
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"protocol":"openai-completions","name":"Concurrent model","base_url":"https://models.example.test","models":["claimed-model"],"api_key":"fake-test-key"}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)
			c.Set("tenant", "ws")
			c.Set("user_id", "user")
			c.SetParamNames("id")
			c.SetParamValues(id)
			if err := s.handlePutDevelopmentModelProvider(c); err != nil {
				results <- 0
				return
			}
			results <- rec.Code
		}(id)
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded := 0
	for status := range results {
		if status == http.StatusOK {
			succeeded++
		} else if status != http.StatusConflict {
			t.Fatalf("concurrent configuration returned %d", status)
		}
	}
	if succeeded != 1 {
		t.Fatalf("one model acquired by %d providers", succeeded)
	}
}
