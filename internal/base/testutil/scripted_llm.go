package testutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// ErrScriptedExhausted reports a prompt that matched neither a scripted
// response nor the default response.
var ErrScriptedExhausted = errors.New("scripted LLM has no response for prompt")

// ScriptedResponse pairs a prompt-content matcher with one deterministic
// chat response.
type ScriptedResponse struct {
	// MatchContains is matched against the last user message. An empty
	// matcher matches every prompt and therefore acts as a catch-all that
	// takes priority over the default response.
	MatchContains string
	// MatchToolContains, when non-empty, additionally requires some
	// role="tool" message in the request to contain the substring. Tool
	// loops append every tool result as a tool message, so scripts can step
	// a multi-turn tool-call sequence deterministically without any change
	// in the last user message.
	MatchToolContains string
	Response      *contract.ChatResponse
	// Respond, when non-nil, builds the response dynamically from the full
	// request (for example to weave real IDs from tool results into the
	// next tool calls). It takes precedence over Response; a nil return
	// falls through to the next scripted response.
	Respond func(req contract.ChatRequest) *contract.ChatResponse
}

// ScriptedLLM is a deterministic contract.LLM for tests. It records every
// prompt, returns the first scripted response whose matcher appears in the
// prompt, falls back to the default response, and fails with
// ErrScriptedExhausted when neither matches. It follows the ordered-script
// pattern established by scriptedTeamLLM in the team compiler tests.
type ScriptedLLM struct {
	mu         sync.Mutex
	responses  []ScriptedResponse
	defaultRsp *contract.ChatResponse
	prompts    []string
}

// NewScriptedLLM creates a scripted LLM. The default response may be nil;
// scripts are evaluated in declaration order.
func NewScriptedLLM(defaultResponse *contract.ChatResponse, responses ...ScriptedResponse) *ScriptedLLM {
	return &ScriptedLLM{defaultRsp: defaultResponse, responses: responses}
}

// Chat returns the first scripted response matching the last user message,
// or the default response when configured, and records the prompt.
func (l *ScriptedLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	prompt := lastScriptedMessage(req.Messages)
	lastUser := lastScriptedUserMessage(req.Messages)
	toolText := joinedScriptedToolMessages(req.Messages)
	l.mu.Lock()
	l.prompts = append(l.prompts, prompt)
	responses, defaultRsp := l.responses, l.defaultRsp
	l.mu.Unlock()
	for _, scripted := range responses {
		if scripted.Respond == nil && scripted.Response == nil {
			continue
		}
		if scripted.MatchContains != "" && !strings.Contains(lastUser, scripted.MatchContains) {
			continue
		}
		if scripted.MatchToolContains != "" && !strings.Contains(toolText, scripted.MatchToolContains) {
			continue
		}
		if scripted.Respond != nil {
			if response := scripted.Respond(req); response != nil {
				return response, nil
			}
			continue
		}
		return scripted.Response, nil
	}
	if defaultRsp != nil {
		return defaultRsp, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrScriptedExhausted, prompt)
}

// Stream follows the scriptedTeamLLM precedent: a closed empty stream, so a
// scripted graph that never streams still completes deterministically.
func (l *ScriptedLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	stream := make(chan contract.StreamChunk)
	close(stream)
	return stream, nil
}

// PromptCount returns the number of Chat calls made so far.
func (l *ScriptedLLM) PromptCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prompts)
}

// Prompts returns a copy of every recorded prompt in call order.
func (l *ScriptedLLM) Prompts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.prompts...)
}

func lastScriptedUserMessage(messages []contract.Message) string {
	last := ""
	for _, message := range messages {
		if message.Role == "user" {
			last = message.Content
		}
	}
	return last
}

// lastScriptedMessage returns the content of the last message of any role,
// so recorded prompts expose tool results as well as user turns.
func lastScriptedMessage(messages []contract.Message) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].Content
}

// joinedScriptedToolMessages concatenates every role="tool" message content,
// so a scripted matcher can find any result of a multi-call batch.
func joinedScriptedToolMessages(messages []contract.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		if message.Role == "tool" {
			builder.WriteString(message.Content)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}
