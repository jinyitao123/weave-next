package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

// The dispatch guard rechecks authority at the last moment. Its refusal must
// keep failing closed, must never reach the MCP server's tools/call, and must
// keep its cause so callers can tell an expired authorization from a scope
// error instead of reporting every refusal the same way.
func TestHTTPHostDispatchGuardRefusalKeepsItsCauseAndNeverCallsTheTool(t *testing.T) {
	var toolCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Method == "notifications/initialized" {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		if message.Method == "tools/call" {
			toolCalls.Add(1)
		}
		var result any
		switch message.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		default:
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "done"}}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	}))
	defer server.Close()

	errAuthorityLapsed := errors.New("authority lapsed")
	refuse := true
	host := NewHTTPHost(server.URL, WithDispatchGuard(func(context.Context) error {
		if refuse {
			return errAuthorityLapsed
		}
		return nil
	}))

	result, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "call-1", Name: "calculate", Args: `{}`})
	if result != nil || !errors.Is(err, ErrFailClosed) {
		t.Fatalf("a guard refusal did not fail closed: result=%+v err=%v", result, err)
	}
	if !errors.Is(err, errAuthorityLapsed) {
		t.Fatalf("the guard's cause was lost: %v", err)
	}
	if toolCalls.Load() != 0 {
		t.Fatalf("the tool was called %d time(s) after the guard refused", toolCalls.Load())
	}

	refuse = false
	if result, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "call-2", Name: "calculate", Args: `{}`}); err != nil || result == nil || result.IsError {
		t.Fatalf("a passing guard blocked the call: result=%+v err=%v", result, err)
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("tool calls after a passing guard = %d, want 1", toolCalls.Load())
	}
}
