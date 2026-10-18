package workflow

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

func TestMCPImageReachesNextWorkflowModelRound(t *testing.T) {
	const image = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aB9sAAAAASUVORK5CYII="
	var modelCalls, toolCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/mcp" {
			method, _ := body["method"].(string)
			if method == "notifications/initialized" {
				w.WriteHeader(202)
				return
			}
			var result any
			switch method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "image", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []map[string]any{{"name": "read_image", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				toolCalls.Add(1)
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "synthetic image"}, {"type": "image", "mimeType": "image/png", "data": image}}}
			default:
				t.Errorf("unexpected MCP method %s", method)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
			return
		}
		call := modelCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"image-call\",\"type\":\"function\",\"function\":{\"name\":\"read_image\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		messages := body["messages"].([]any)
		encoded, _ := json.Marshal(messages)
		if !strings.Contains(string(encoded), "data:image/png;base64,"+image) || !strings.Contains(string(encoded), `"tool_call_id":"image-call"`) {
			t.Error("next model request lost image or its tool result association")
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"image received\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	model := &runtimeStreamingLLM{inner: openai.New("synthetic", openai.WithBaseURL(server.URL))}
	graph := loom.NewGraph("image-loop", "chat")
	graph.AddStep("chat", stdlib.NewToolLoopStep(model, mcphost.NewHTTPHost(server.URL+"/mcp"), stdlib.ToolLoopOpts{Model: "synthetic", MaxIterations: 3}), loom.End())
	result, err := graph.Run(t.Context(), loom.State{"messages": []contract.Message{{Role: "user", Content: "inspect image"}}}, nil)
	if err != nil || result.State["output"] != "image received" || modelCalls.Load() != 2 || toolCalls.Load() != 1 {
		t.Fatalf("image workflow: result=%+v err=%v models=%d tools=%d", result, err, modelCalls.Load(), toolCalls.Load())
	}
}
