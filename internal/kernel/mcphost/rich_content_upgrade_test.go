package mcphost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func upgradeImageResult(t *testing.T, content []map[string]any, opts ...HostOption) (*contract.ToolResult, error) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch message.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "synthetic", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "inspect_image", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		case "tools/call":
			result = map[string]any{"content": content}
		default:
			t.Errorf("unexpected method %q", message.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	}))
	defer server.Close()
	return NewHTTPHost(server.URL, opts...).Dispatch(t.Context(), contract.ToolCall{ID: "image-call", Name: "inspect_image", Args: `{}`})
}

func TestLoomUpgradeMCPRetainsToolImageParts(t *testing.T) {
	const image = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aB9sAAAAASUVORK5CYII="
	result, err := upgradeImageResult(t, []map[string]any{{"type": "text", "text": "synthetic caption"}, {"type": "image", "mimeType": "image/png", "data": image}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "synthetic caption" || len(result.Parts) != 2 || result.Parts[0].Type != "text" || result.Parts[1].Type != "image" || result.Parts[1].Data != image {
		t.Fatalf("ordered rich result=%+v", result)
	}
}

func TestMCPImageValidationPreservesUnknownWriteOutcome(t *testing.T) {
	for _, bad := range []map[string]any{
		{"type": "image", "mimeType": "text/plain", "data": "eA=="},
		{"type": "image", "mimeType": "image/png", "data": "!bad"},
		{"type": "image", "mimeType": "image/png", "data": ""},
		{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(make([]byte, (2<<20)+1))},
	} {
		result, err := upgradeImageResult(t, []map[string]any{bad}, WithUnknownDispatchOutcome())
		if result != nil || !errors.Is(err, ErrDispatchOutcomeUnknown) {
			t.Fatalf("invalid possible-write receipt=%+v err=%v", result, err)
		}
	}
	result, err := upgradeImageResult(t, []map[string]any{{"type": "text", "text": "a"}, {"type": "text", "text": "b"}})
	if err != nil || result.Content != "ab" || result.Parts != nil {
		t.Fatalf("plain text wire changed: %+v %v", result, err)
	}
}
