package testutil

import (
	"context"
	"fmt"

	"github.com/jinyitao123/loom/contract"
)

// MockLLM returns deterministic chat and stream responses for tests.
type MockLLM struct{}

// Chat returns a response containing the last user message.
func (m *MockLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	var lastMessage string
	for _, message := range req.Messages {
		if message.Role == "user" {
			lastMessage = message.Content
		}
	}
	return &contract.ChatResponse{
		Content: fmt.Sprintf("Mock response to: %s", lastMessage),
		Usage:   contract.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

// Stream returns a deterministic two-chunk response.
func (m *MockLLM) Stream(_ context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	chunks := make(chan contract.StreamChunk, 3)
	go func() {
		chunks <- contract.StreamChunk{Content: "Mock "}
		chunks <- contract.StreamChunk{Content: "stream"}
		chunks <- contract.StreamChunk{Done: true, Usage: &contract.Usage{InputTokens: 10, OutputTokens: 5}}
		close(chunks)
	}()
	return chunks, nil
}
