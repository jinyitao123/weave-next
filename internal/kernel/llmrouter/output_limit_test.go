package llmrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestProviderOutputLimitWireFields(t *testing.T) {
	bodies := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	for _, field := range []string{"", frozen.OutputLimitMaxTokens, frozen.OutputLimitMaxCompletionTokens} {
		t.Run("field="+field, func(t *testing.T) {
			cfg := ProviderConfig{Protocol: frozen.ProtocolOpenAICompletions, BaseURL: server.URL, ChatPath: "/v1/chat/completions", OutputLimitField: field}
			client := NewProviderClient(cfg)
			request := contract.ChatRequest{Model: "arbitrary-model-name", MaxTokens: 123, Messages: []contract.Message{{Role: "user", Content: "hi"}}}
			if _, err := client.Chat(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			stream, err := client.Stream(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range stream {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
			want, gone := frozen.OutputLimitMaxTokens, frozen.OutputLimitMaxCompletionTokens
			if field == frozen.OutputLimitMaxCompletionTokens {
				want, gone = gone, want
			}
			for range 2 {
				body := <-bodies
				if body[want] != float64(123) || body[gone] != nil {
					t.Fatalf("wrong output fields: %v", body)
				}
			}
		})
	}
	for _, cfg := range []ProviderConfig{
		{Protocol: frozen.ProtocolOpenAICompletions, OutputLimitField: "max_output_tokens"},
		{Protocol: frozen.ProtocolAnthropicMessages, OutputLimitField: frozen.OutputLimitMaxTokens},
		{Protocol: frozen.ProtocolOpenAIResponses, OutputLimitField: frozen.OutputLimitMaxCompletionTokens},
	} {
		cfg.BaseURL, cfg.ChatPath = server.URL, "/v1/chat/completions"
		if cfg.Protocol == frozen.ProtocolAnthropicMessages {
			cfg.ChatPath = "/v1/messages"
		}
		if cfg.Protocol == frozen.ProtocolOpenAIResponses {
			cfg.ChatPath = "/v1/responses"
		}
		client := NewProviderClient(cfg)
		if _, err := client.Chat(context.Background(), contract.ChatRequest{}); err == nil || !strings.Contains(err.Error(), "output_limit_field") {
			t.Fatal("invalid declaration accepted")
		}
		if _, err := client.Stream(context.Background(), contract.ChatRequest{}); err == nil || !strings.Contains(err.Error(), "output_limit_field") {
			t.Fatal("invalid stream declaration accepted")
		}
	}
	if len(bodies) != 0 {
		t.Fatal("invalid declaration reached HTTP")
	}
}
