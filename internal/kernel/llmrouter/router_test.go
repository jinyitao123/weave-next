package llmrouter

import (
	"context"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type recordingLLM struct {
	requests []contract.ChatRequest
}

func (l *recordingLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, req)
	return &contract.ChatResponse{}, nil
}

func (l *recordingLLM) Stream(_ context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	l.requests = append(l.requests, req)
	stream := make(chan contract.StreamChunk)
	close(stream)
	return stream, nil
}

func TestRouterAppliesFallbackModelToProviderRequest(t *testing.T) {
	const fallback = "gpt-fallback"
	client := &recordingLLM{}
	router := New(fallback)
	router.models[fallback] = "provider"
	router.clients["provider"] = client

	if _, err := router.Chat(context.Background(), contract.ChatRequest{}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if _, err := router.Stream(context.Background(), contract.ChatRequest{}); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(client.requests))
	}
	for index, request := range client.requests {
		if request.Model != fallback {
			t.Errorf("provider request %d model = %q, want %q", index, request.Model, fallback)
		}
	}
}
