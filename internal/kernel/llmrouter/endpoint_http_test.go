package llmrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// pathRecorder answers every chat request with a minimal completion and keeps
// the path each one was sent to.
func pathRecorder(t *testing.T, status int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, `{"error":{"message":"refused"}}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestProviderRequestsGoToTheRecordedAddress(t *testing.T) {
	server, paths := pathRecorder(t, http.StatusOK)
	for _, tc := range []struct {
		name, base, chatPath, want string
	}{
		{"recorded custom path", server.URL + "/api/paas/v4", "/chat/completions", "/api/paas/v4/chat/completions"},
		{"recorded gateway prefix", server.URL + "/openai/team-a", "/v1/chat/completions", "/openai/team-a/v1/chat/completions"},
		{"legacy base ending in v1", server.URL + "/v1", "", "/v1/chat/completions"},
		{"legacy origin", server.URL, "", "/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(paths())
			cfg := ProviderConfig{ID: "p", BaseURL: tc.base, ChatPath: tc.chatPath, Models: []string{"m"}, Protocol: ProtocolOpenAI}
			if cfg.ChatURL() != server.URL+tc.want {
				t.Fatalf("ChatURL = %s, want %s", cfg.ChatURL(), server.URL+tc.want)
			}
			if result := ProbeProvider(context.Background(), cfg, "m", 5*time.Second); result.Category != ProbeOK {
				t.Fatalf("probe = %+v", result)
			}
			got := paths()
			if len(got) != before+1 || got[len(got)-1] != tc.want {
				t.Fatalf("request paths = %v, want one more ending %s", got, tc.want)
			}
		})
	}
}

func TestOpenAISDKBaseReachesTheSameAddress(t *testing.T) {
	for _, tc := range []struct {
		base, chatPath, want string
		ok                   bool
	}{
		{"https://open.example.test", "/api/paas/v4/chat/completions", "https://open.example.test/api/paas/v4", true},
		{"https://api.example.test", "/v1/chat/completions", "https://api.example.test/v1", true},
		{"https://api.example.test/v1", "", "https://api.example.test/v1", true},
		{"https://api.example.test", "/v1/generate", "", false},
	} {
		got, ok := ProviderConfig{BaseURL: tc.base, ChatPath: tc.chatPath, Protocol: ProtocolOpenAI}.OpenAISDKBaseURL()
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s + %s: sdk base = %q %v, want %q %v", tc.base, tc.chatPath, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := (ProviderConfig{BaseURL: "https://d.example.test", Protocol: ProtocolSystemOne}).OpenAISDKBaseURL(); ok {
		t.Fatal("a systemone provider was given an OpenAI SDK base")
	}
}

func TestProbeNamesWhatToCheck(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, ProbeEndpointNotFound},
		{http.StatusUnauthorized, ProbeUnauthorized},
		{http.StatusTooManyRequests, ProbeRateLimited},
		{http.StatusBadRequest, ProbeRequestRejected},
	} {
		server, _ := pathRecorder(t, tc.status)
		cfg := ProviderConfig{ID: "p", BaseURL: server.URL, ChatPath: "/v1/chat/completions", Models: []string{"m"}, Protocol: ProtocolOpenAI}
		if result := ProbeProvider(context.Background(), cfg, "m", 5*time.Second); result.Category != tc.want || result.Status != tc.status {
			t.Errorf("status %d: probe = %+v, want %s", tc.status, result, tc.want)
		}
	}
	unreachable := ProviderConfig{ID: "p", BaseURL: "http://127.0.0.1:1", ChatPath: "/v1/chat/completions", Models: []string{"m"}, Protocol: ProtocolOpenAI}
	if result := ProbeProvider(context.Background(), unreachable, "m", 5*time.Second); result.Category != ProbeUnreachable {
		t.Errorf("closed port: probe = %+v", result)
	}
}

func TestDeclaredOutputLimitFieldReachesTheWire(t *testing.T) {
	var fields []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fields = append(fields, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	for _, declared := range []string{"", "max_completion_tokens"} {
		cfg := ProviderConfig{ID: "p", BaseURL: server.URL, ChatPath: "/v1/chat/completions", Models: []string{"m"}, Protocol: ProtocolOpenAI, OutputLimitField: declared}
		if _, err := NewProviderClient(cfg).Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "ping"}}, MaxTokens: 64}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fields) != 2 || fields[0]["max_tokens"] != float64(64) || fields[0]["max_completion_tokens"] != nil ||
		fields[1]["max_completion_tokens"] != float64(64) || fields[1]["max_tokens"] != nil {
		t.Fatalf("request bodies = %v", fields)
	}
}
