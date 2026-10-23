package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

const privateFactsMarker = "private-facts-marker"

func sseFrame(delta string, reason string) string {
	frame := map[string]any{"id": "x", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": json.RawMessage(delta), "finish_reason": nil}}}
	if reason != "" {
		frame["choices"].([]any)[0].(map[string]any)["finish_reason"] = reason
	}
	raw, _ := json.Marshal(frame)
	return "data: " + string(raw) + "\n\n"
}

// Every terminal model event carries what the provider boundary saw: the
// status of any response, 200 included, the provider's request ID, how the
// exchange ended and the request as sent, so a failure can be filed with the
// provider without recording the exchange (#108).
func TestModelCallEventsCarryProviderFacts(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		requestID string
		body      string
		phase     ModelCallPhase
		class     ModelCallErrorClass
		end       string
		finish    string
		content   int
	}{
		{name: "200 cut at the output limit with no answer", status: 200, requestID: "req_length-1",
			body: sseFrame(`{"role":"assistant","reasoning_content":"`+privateFactsMarker+`"}`, "") + sseFrame(`{}`, "length") + "data: [DONE]\n\n",
			phase: ModelCallFailed, class: ModelCallErrorClass(llmrouter.CallFailureOutputLimit), end: "empty_response", finish: "length", content: 0},
		{name: "200 with an empty answer", status: 200, requestID: "req_empty-2",
			body:  sseFrame(`{"role":"assistant","content":""}`, "stop") + "data: [DONE]\n\n",
			phase: ModelCallFailed, class: ModelCallErrorClass(llmrouter.CallFailureEmptyResponse), end: "empty_response", finish: "stop", content: 0},
		{name: "200 answered", status: 200, requestID: "req_ok-3",
			body:  sseFrame(`{"role":"assistant","content":"done"}`, "stop") + "data: [DONE]\n\n",
			phase: ModelCallCompleted, end: "done", finish: "stop", content: 4},
		{name: "429", status: 429, requestID: "req_limited-4", body: `{"error":{"message":"` + privateFactsMarker + `"}}`,
			phase: ModelCallFailed, class: ModelCallErrorClass(llmrouter.CallFailureRateLimited), end: "http_error"},
		{name: "401", status: 401, body: `{"error":{"message":"` + privateFactsMarker + `"}}`,
			phase: ModelCallFailed, class: ModelCallErrorClass(llmrouter.CallFailureAuth), end: "http_error"},
		{name: "500", status: 500, requestID: "req_upstream-5", body: privateFactsMarker,
			phase: ModelCallFailed, class: ModelCallErrorClass(llmrouter.CallFailureUpstream5xx), end: "http_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Set-Cookie", privateFactsMarker+"=1")
				if tc.requestID != "" {
					w.Header().Set("X-Request-Id", tc.requestID)
				}
				if tc.status == 200 {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			event := factsModelEvent(t, server.URL, tc.phase)
			if tc.class != "" && event.ErrorClass != tc.class {
				t.Fatalf("class = %q, want %q", event.ErrorClass, tc.class)
			}
			facts := event.Facts
			if facts == nil || facts.HTTPStatus != tc.status || facts.ProviderRequestID != tc.requestID || facts.End != tc.end ||
				facts.FinishReason != tc.finish || facts.RequestBytes == 0 || facts.Request == nil ||
				facts.Request.Model != "fixture-model" || !facts.Request.Stream || facts.Request.OutputLimitField != "max_tokens" || facts.Request.OutputLimit != 512 {
				t.Fatalf("facts = %+v request=%+v", facts, facts.Request)
			}
			if tc.status == 200 && (facts.ContentBytes == nil || *facts.ContentBytes != tc.content) {
				t.Fatalf("content bytes = %v, want %d observed", facts.ContentBytes, tc.content)
			}
			encoded, _ := json.Marshal(event)
			for _, private := range []string{privateFactsMarker, "test-only-key", "recorded-prompt"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("event leaked %q: %s", private, encoded)
				}
			}
		})
	}
	t.Run("no response", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		event := factsModelEvent(t, url, ModelCallFailed)
		if event.Facts == nil || event.Facts.HTTPStatus != 0 || event.Facts.End != "transport_error" || event.Facts.ContentBytes == nil || *event.Facts.ContentBytes != 0 {
			t.Fatalf("no-response facts = %+v", event.Facts)
		}
	})
}

func factsModelEvent(t *testing.T, url string, phase ModelCallPhase) ModelCallEvent {
	t.Helper()
	provider := llmrouter.NewProviderClient(llmrouter.ProviderConfig{
		ID: "recorded", BaseURL: url, APIKey: "test-only-key", Models: []string{"fixture-model"}, AttemptTimeoutSeconds: 10,
	})
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, &runtimeStreamingLLM{inner: provider}, recorder.observe)
	_, _ = model.Chat(context.Background(), contract.ChatRequest{Model: "fixture-model", MaxTokens: 512,
		Messages: []contract.Message{{Role: "user", Content: "recorded-prompt"}}})
	return recorder.wait(t, phase)
}
