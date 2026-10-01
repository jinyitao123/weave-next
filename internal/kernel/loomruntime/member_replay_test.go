package loomruntime

import (
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestValidateProviderHistoryRejectsWhatProvidersReject(t *testing.T) {
	call := contract.ToolCall{ID: "x", Name: "write", Args: `{}`}
	broken := map[string][]contract.Message{
		"unanswered call":    {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}},
		"orphan result":      {{Role: "user", Content: "u"}, {Role: "tool", ToolCallID: "x", Content: "r"}},
		"user before result": {{Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "user", Content: "again"}},
		"duplicate id":       {{Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "tool", ToolCallID: "x"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}},
		"bad arguments":      {{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "y", Name: "w", Args: "{"}}}, {Role: "tool", ToolCallID: "y"}},
		"unknown role":       {{Role: "developer", Content: "x"}},
	}
	for name, messages := range broken {
		if validateProviderHistory(messages) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	valid := []contract.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}, {Role: "assistant", ToolCalls: []contract.ToolCall{call}}, {Role: "tool", ToolCallID: "x", Content: "r"}}
	if err := validateProviderHistory(valid); err != nil {
		t.Errorf("valid history rejected: %v", err)
	}
}
