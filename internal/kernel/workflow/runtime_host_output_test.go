package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestFrozenNodeOutputUsesDeclaredProviderModeAndLocalVerification(t *testing.T) {
	for _, objectMode := range []bool{true, false} {
		t.Run(fmt.Sprint(objectMode), func(t *testing.T) {
			var requests []map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				requests = append(requests, body)
				content := `{"answer":"accepted"}`
				if len(requests) == 1 {
					content = `{"answer":"draft","extra":"rejected"}`
				}
				payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}, "finish_reason": "stop"}}})
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
			}))
			defer server.Close()
			binding := frozen.FrozenModelBinding{ProviderID: "system/deepseek", ModelID: "deepseek-flash", BaseURL: server.URL, JSONObjectMode: objectMode}
			binding.CredentialRef.ServiceID = "system-provider:deepseek"
			opts, closer, err := NewRuntimeHostFactory().Build(t.Context(), frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Model: "deepseek-flash"}, PrimaryModel: binding}, thinkingTestResolver{})
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			graph, err := compiler.CompileAgent("ws", &registry.AgentRecord{Name: "worker", Model: "deepseek-flash", Compaction: &registry.CompactionConfig{Enabled: false}}, opts.LLM, opts.Tools, compiler.CompileOpts{})
			if err != nil {
				t.Fatal(err)
			}
			schema := json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)
			result, err := graph.Run(compiler.WithNodeOutputSchema(t.Context(), schema), loom.State{"messages": []contract.Message{{Role: "user", Content: "Produce the requested answer."}}}, loom.NewMemStore())
			if err != nil || result.State["output"] != `{"answer":"accepted"}` || len(requests) != 2 {
				t.Fatalf("result=%#v requests=%d err=%v", result, len(requests), err)
			}
			for _, request := range requests {
				var format struct {
					Type   string          `json:"type"`
					Schema json.RawMessage `json:"json_schema"`
				}
				if err := json.Unmarshal(request["response_format"], &format); err != nil {
					t.Fatal(err)
				}
				if objectMode && (format.Type != "json_object" || len(format.Schema) != 0) {
					t.Fatalf("unsupported schema sent to object-only provider: %s", request["response_format"])
				}
				if !objectMode && format.Type != "json_schema" {
					t.Fatalf("strict provider mode lost: %s", request["response_format"])
				}
				var messages []contract.Message
				if err := json.Unmarshal(request["messages"], &messages); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, m := range messages {
					if strings.Contains(m.Content, string(schema)) {
						found = true
					}
				}
				if !found {
					t.Fatal("object-mode model received no exact schema instruction")
				}
			}
			if !strings.Contains(string(requests[1]["messages"]), "/extra") {
				t.Fatal("invalid server output bypassed local verifier")
			}
		})
	}
}
