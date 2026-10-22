package llmrouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// recordingProvider records the path of every request and answers with an
// error, so each adapter's request reaches the wire and stops there.
type recordingProvider struct {
	mu    sync.Mutex
	paths []string
}

func (p *recordingProvider) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.mu.Unlock()
		http.Error(w, `{"error":{"message":"recorded"}}`, http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	return server
}

func (p *recordingProvider) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.paths) == 0 {
		return ""
	}
	return p.paths[len(p.paths)-1]
}

// Every protocol's client sends to exactly base_url + chat_path, for an
// origin, a gateway prefix, a custom path and a trailing slash alike.
func TestProviderClientSendsToTheConfiguredChatURL(t *testing.T) {
	recorder := &recordingProvider{}
	server := recorder.start(t)
	for _, tc := range []struct {
		name, protocol, prefix, path, want string
	}{
		{"completions at the origin", frozen.ProtocolOpenAICompletions, "", "", "/v1/chat/completions"},
		{"completions with trailing slash", frozen.ProtocolOpenAICompletions, "/", "", "/v1/chat/completions"},
		{"completions sdk style", frozen.ProtocolOpenAICompletions, "/v1", "/chat/completions", "/v1/chat/completions"},
		{"completions behind a gateway prefix", frozen.ProtocolOpenAICompletions, "/gw/team-a", "", "/gw/team-a/v1/chat/completions"},
		{"completions on a custom path", frozen.ProtocolOpenAICompletions, "", "/api/paas/v4/chat/completions", "/api/paas/v4/chat/completions"},
		{"responses at the origin", frozen.ProtocolOpenAIResponses, "", "", "/v1/responses"},
		{"responses behind a prefix", frozen.ProtocolOpenAIResponses, "/gw", "/openai/v1/responses", "/gw/openai/v1/responses"},
		{"messages at the origin", frozen.ProtocolAnthropicMessages, "", "", "/v1/messages"},
		{"messages behind a prefix", frozen.ProtocolAnthropicMessages, "/gw/anthropic", "", "/gw/anthropic/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{ID: "p", Name: "P", Protocol: tc.protocol, BaseURL: server.URL + tc.prefix, ChatPath: tc.path, APIKey: "test-key", Models: []string{"m"}}
			if err := cfg.NormalizeEndpoint(); err != nil {
				t.Fatal(err)
			}
			if cfg.ChatURL() != server.URL+tc.want {
				t.Fatalf("shown address = %s, want %s", cfg.ChatURL(), server.URL+tc.want)
			}
			_, err := NewProviderClient(cfg).Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "ping"}}, MaxTokens: 16})
			if err == nil {
				t.Fatal("the recording provider's refusal was not returned")
			}
			if got := recorder.last(); got != tc.want {
				t.Fatalf("request went to %s, want %s", got, tc.want)
			}
		})
	}
}

func TestProviderClientRefusesAnUnnormalizedEndpointBeforeSending(t *testing.T) {
	recorder := &recordingProvider{}
	server := recorder.start(t)
	for _, cfg := range []ProviderConfig{
		{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: server.URL + "/v1", ChatPath: "/v1/chat/completions"},
		{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: server.URL + "/v1/chat/completions", ChatPath: "/v1/chat/completions"},
		{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: server.URL},
	} {
		cfg.ID, cfg.Name, cfg.Models = "p", "P", []string{"m"}
		if _, err := NewProviderClient(cfg).Chat(context.Background(), contract.ChatRequest{Model: "m", Messages: []contract.Message{{Role: "user", Content: "ping"}}}); err == nil {
			t.Fatalf("%+v was sent", cfg)
		}
	}
	if got := recorder.last(); got != "" {
		t.Fatalf("a refused endpoint still reached the provider at %s", got)
	}
}

func TestOpenAISDKBaseReachesTheSameAddress(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{
		{"https://api.example.test", "/v1/chat/completions", "https://api.example.test/v1"},
		{"https://api.example.test/v1", "/chat/completions", "https://api.example.test/v1"},
		{"https://gw.example.test/x", "/api/v4/chat/completions", "https://gw.example.test/x/api/v4"},
	} {
		cfg := ProviderConfig{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: tc.base, ChatPath: tc.path}
		if got, ok := cfg.OpenAISDKBaseURL(); !ok || got != tc.want || got+"/chat/completions" != cfg.ChatURL() {
			t.Fatalf("%+v: sdk base = %s, %v", cfg, got, ok)
		}
	}
	if _, ok := (ProviderConfig{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: "https://x.test", ChatPath: "/v1/completions/chat"}).OpenAISDKBaseURL(); ok {
		t.Fatal("a path the SDK cannot reach was given an SDK base")
	}
	if _, ok := (ProviderConfig{Protocol: frozen.ProtocolAnthropicMessages, BaseURL: "https://x.test", ChatPath: "/v1/messages"}).OpenAISDKBaseURL(); ok {
		t.Fatal("a native protocol was given an SDK base")
	}
}

func TestProbeClassifiesWhatTheEndpointAnswered(t *testing.T) {
	answer := func(status int, body string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.Header.Get("Authorization"), "probe-secret") {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(server.Close)
		return server
	}
	ok := `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	for _, tc := range []struct {
		name     string
		server   *httptest.Server
		category string
		status   int
	}{
		{"answers", answer(200, ok), ProbeOK, 0},
		{"wrong key", answer(401, `{"error":{"message":"bad key probe-secret"}}`), ProbeUnauthorized, 401},
		{"wrong path", answer(404, `not found`), ProbeEndpointNotFound, 404},
		{"wrong model", answer(400, `{"error":{"message":"model"}}`), ProbeRequestRejected, 400},
		{"limited", answer(429, `{}`), ProbeRateLimited, 429},
		{"broken", answer(502, `{}`), ProbeProviderError, 502},
		{"not the protocol", answer(200, `<html>console</html>`), ProbeInvalidResponse, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{ID: "p", Name: "P", Protocol: frozen.ProtocolOpenAICompletions, BaseURL: tc.server.URL, ChatPath: "/v1/chat/completions", APIKey: "probe-secret", Models: []string{"m"}}
			result := ProbeProvider(context.Background(), cfg, "m", 5*time.Second)
			if result.Category != tc.category || result.Status != tc.status || result.ChatURL != tc.server.URL+"/v1/chat/completions" || result.Model != "m" {
				t.Fatalf("result = %+v", result)
			}
		})
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	cfg := ProviderConfig{ID: "p", Name: "P", Protocol: frozen.ProtocolOpenAICompletions, BaseURL: address, ChatPath: "/v1/chat/completions", APIKey: "k", Models: []string{"m"}}
	if result := ProbeProvider(context.Background(), cfg, "m", 5*time.Second); result.Category != ProbeUnreachable {
		t.Fatalf("closed port = %+v", result)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(func() { close(release); slow.Close() })
	cfg.BaseURL = slow.URL
	if result := ProbeProvider(context.Background(), cfg, "m", 200*time.Millisecond); result.Category != ProbeTimeout {
		t.Fatalf("slow provider = %+v", result)
	}

	cfg.ChatPath = ""
	if result := ProbeProvider(context.Background(), cfg, "m", time.Second); result.Category != ProbeInvalidConfig {
		t.Fatalf("invalid configuration = %+v", result)
	}
}
