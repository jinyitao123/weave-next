package mcphost

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestHTTPHostUnknownDispatchOutcomeIsOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[optIn], func(t *testing.T) {
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
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = writer.Write([]byte("connection unavailable"))
					return
				}
				var result any
				switch message.Method {
				case "initialize":
					result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
				case "tools/list":
					result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
				default:
					t.Errorf("unexpected method %q", message.Method)
				}
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
			}))
			defer server.Close()
			opts := []HostOption{}
			if optIn {
				opts = append(opts, WithUnknownDispatchOutcome())
			}
			host := NewHTTPHost(server.URL, opts...)
			result, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "call-1", Name: "calculate", Args: `{}`})
			if optIn {
				if result != nil || !errors.Is(err, ErrDispatchOutcomeUnknown) {
					t.Fatalf("explicit unknown mode returned result=%+v err=%v", result, err)
				}
				return
			}
			if err != nil || result == nil || !result.IsError || result.CallID != "call-1" {
				t.Fatalf("default HTTPHost behavior changed: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestHTTPHostReportsMCPErrorAsExplicitFailureWhenOptedIn(t *testing.T) {
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
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"jsonrpc": "2.0", "id": message.ID,
				"error": map[string]any{"code": -32000, "message": "action rejected"},
			})
			return
		}
		var result any
		switch message.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		default:
			t.Errorf("unexpected method %q", message.Method)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	}))
	defer server.Close()
	host := NewHTTPHost(server.URL, WithUnknownDispatchOutcome())
	result, err := host.Dispatch(t.Context(), contract.ToolCall{ID: "call-1", Name: "calculate", Args: `{}`})
	if result != nil || !errors.Is(err, ErrDispatchExplicitFailure) || errors.Is(err, ErrDispatchOutcomeUnknown) {
		t.Fatalf("explicit MCP error was misclassified: result=%+v err=%v", result, err)
	}
}
