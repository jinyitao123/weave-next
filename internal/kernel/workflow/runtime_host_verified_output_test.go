package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type verifiedOutputTools struct{ calls int }

func (*verifiedOutputTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}
func (tools *verifiedOutputTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.calls++
	return &contract.ToolResult{CallID: call.ID, Content: `{"value":42}`}, nil
}

type verifiedOutputJournal struct{ requests []contract.ChatRequest }

func (*verifiedOutputJournal) Active(context.Context) bool { return true }
func (j *verifiedOutputJournal) Execute(_ context.Context, op stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	request, ok := op.Input.(contract.ChatRequest)
	if !ok {
		return nil, fmt.Errorf("expected model request")
	}
	j.requests = append(j.requests, request)
	result, err := perform()
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func TestVerifiedNodeToolsThenJSONCorrectionUsesOneToolEffect(t *testing.T) {
	var requests []map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, request)
		if _, present := request["response_format"]; present {
			t.Error("verified tool round must not force json_object")
		}
		var tools []json.RawMessage
		if json.Unmarshal(request["tools"], &tools) != nil || len(tools) != 1 {
			t.Error("tool disappeared from request")
		}
		delta := map[string]any{"content": `{"answer":"accepted"}`}
		finish := "stop"
		switch len(requests) {
		case 1:
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-lookup", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}
			finish = "tool_calls"
		case 2:
			delta["content"] = `{"answer":"draft","extra":"rejected"}`
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer server.Close()
	binding := frozen.FrozenModelBinding{ProviderID: "fixture", ModelID: "fixture", BaseURL: server.URL, JSONObjectMode: true}
	opts, closer, err := NewRuntimeHostFactory().Build(t.Context(), frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{Model: "fixture"}, PrimaryModel: binding}, thinkingTestResolver{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	tools, journal := &verifiedOutputTools{}, &verifiedOutputJournal{}
	graph, err := compiler.CompileAgent("ws", &registry.AgentRecord{Name: "worker", Model: "fixture", Compaction: &registry.CompactionConfig{Enabled: false}}, opts.LLM, tools, compiler.CompileOpts{ExecutionLLMWrapper: func(inner contract.LLM) contract.LLM { return stdlib.NewJournaledLLM(inner, journal) }})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)
	result, err := graph.Run(compiler.WithNodeOutputSchema(t.Context(), schema), loom.State{"messages": []contract.Message{{Role: "user", Content: "Look up the value and provide the final answer."}}}, loom.NewMemStore())
	if err != nil || result.State["output"] != `{"answer":"accepted"}` || tools.calls != 1 || len(requests) != 3 {
		t.Fatalf("result=%#v tools=%d requests=%d err=%v", result, tools.calls, len(requests), err)
	}
	if len(journal.requests) != 3 {
		t.Fatal("model rounds escaped the logical journal")
	}
	for _, request := range journal.requests {
		if request.Schema == nil || string(*request.Schema) != string(schema) {
			t.Fatal("logical journal lost its exact schema")
		}
	}
	if !strings.Contains(string(requests[2]["messages"]), "/extra") {
		t.Fatal("invalid final output was not corrected within the loop")
	}
	var messages []contract.Message
	if err := json.Unmarshal(requests[2]["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	receipts := 0
	for _, message := range messages {
		if message.Role == "tool" {
			receipts++
			if message.ToolCallID != "call-lookup" || message.Content != `{"value":42}` {
				t.Fatal("tool receipt changed")
			}
		}
	}
	if receipts != 1 || !strings.Contains(messages[0].Content, string(schema)) {
		t.Fatal("receipt or final schema instruction lost")
	}
}

func TestAgentSchemaWithoutNodeVerifierKeepsProviderFormat(t *testing.T) {
	for _, textNode := range []bool{false, true} {
		t.Run(fmt.Sprint(textNode), func(t *testing.T) {
			var format string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Format struct {
						Type string `json:"type"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				format = request.Format.Type
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			client := llmrouter.NewProviderClient(llmrouter.ProviderConfig{BaseURL: server.URL, JSONObjectMode: true})
			schema := json.RawMessage(`{"type":"object"}`)
			graph, err := compiler.CompileAgent("ws", &registry.AgentRecord{Name: "worker", OutputSchema: &schema, Spec: stdlib.AgentSpec{SystemPrompt: "Return JSON."}, Compaction: &registry.CompactionConfig{Enabled: false}}, client, &verifiedOutputTools{}, compiler.CompileOpts{})
			if err != nil {
				t.Fatal(err)
			}
			// An inherited marker cannot replace this invocation's own verifier.
			ctx := llmrouter.WithLocallyVerifiedOutput(t.Context(), schema)
			if textNode {
				ctx = compiler.WithNodeOutputSchema(ctx, nil)
			}
			_, err = graph.Run(ctx, loom.State{"messages": []contract.Message{{Role: "user", Content: "Answer."}}}, loom.NewMemStore())
			if err != nil || format != "json_object" {
				t.Fatalf("unverified format=%q err=%v", format, err)
			}
		})
	}
}
