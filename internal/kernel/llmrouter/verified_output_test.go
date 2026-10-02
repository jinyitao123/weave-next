package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

var verifiedTestSchema = json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)

func verifiedTestRequest() contract.ChatRequest {
	schema := append(json.RawMessage(nil), verifiedTestSchema...)
	return contract.ChatRequest{Model: "fixture", Schema: &schema, Messages: []contract.Message{{Role: "user", Content: "Return final JSON after using the tool."}}, Tools: []contract.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
}

func writeVerifiedTestResponse(w http.ResponseWriter, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"answer\\\":\\\"ok\\\"}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		return
	}
	_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}]}`)
}

func TestLocallyVerifiedOutputHTTPMatrix(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, test := range []struct {
			name                                                   string
			objectMode, tools, marked, mismatch, cleared, noSchema bool
			wantFormat                                             string
		}{
			{name: "verified tools", objectMode: true, tools: true, marked: true},
			{name: "unmarked tools", objectMode: true, tools: true, wantFormat: "json_object"},
			{name: "different schema", objectMode: true, tools: true, marked: true, mismatch: true, wantFormat: "json_object"},
			{name: "cleared marker", objectMode: true, tools: true, marked: true, cleared: true, wantFormat: "json_object"},
			{name: "no tools", objectMode: true, marked: true, wantFormat: "json_object"},
			{name: "native schema", tools: true, marked: true, wantFormat: "json_schema"},
			{name: "no schema", objectMode: true, tools: true, marked: true, noSchema: true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", test.name, stream), func(t *testing.T) {
				var captured map[string]json.RawMessage
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
						t.Error(err)
						return
					}
					writeVerifiedTestResponse(w, stream)
				}))
				defer server.Close()
				llm := NewProviderClient(ProviderConfig{BaseURL: server.URL, JSONObjectMode: test.objectMode})
				request := verifiedTestRequest()
				ctx := t.Context()
				if test.marked {
					ctx = WithLocallyVerifiedOutput(ctx, *request.Schema)
				}
				if test.mismatch {
					*request.Schema = json.RawMessage(`{"type":"object"}`)
				}
				if test.cleared {
					ctx = WithLocallyVerifiedOutput(ctx, nil)
				}
				if !test.tools {
					request.Tools = nil
				}
				if test.noSchema {
					request.Schema = nil
				}
				before, _ := json.Marshal(request)
				if stream {
					chunks, err := llm.Stream(ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := llm.Chat(ctx, request); err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(request)
				if !bytes.Equal(before, after) {
					t.Fatal("provider mapping mutated the logical request")
				}
				var format struct {
					Type   string `json:"type"`
					Schema struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"json_schema"`
				}
				if raw, present := captured["response_format"]; present {
					if err := json.Unmarshal(raw, &format); err != nil {
						t.Fatal(err)
					}
				} else if test.wantFormat != "" {
					t.Fatal("required response format missing")
				}
				if format.Type != test.wantFormat {
					t.Fatalf("format=%q want=%q", format.Type, test.wantFormat)
				}
				if test.wantFormat == "json_schema" && !bytes.Equal(format.Schema.Schema, *request.Schema) {
					t.Fatal("native schema changed")
				}
				var messages []contract.Message
				if json.Unmarshal(captured["messages"], &messages) != nil || len(messages) != 1 || messages[0].Content != request.Messages[0].Content {
					t.Fatal("schema instructions changed")
				}
				var tools []json.RawMessage
				if raw, ok := captured["tools"]; ok {
					_ = json.Unmarshal(raw, &tools)
				}
				if len(tools) != len(request.Tools) {
					t.Fatal("tool advertisement changed")
				}
			})
		}
	}
}

type verifiedRequestJournal struct {
	input, response json.RawMessage
	executions      int
}

func (*verifiedRequestJournal) Active(context.Context) bool { return true }
func (j *verifiedRequestJournal) Execute(_ context.Context, operation stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	input, err := json.Marshal(operation.Input)
	if err != nil {
		return nil, err
	}
	if j.input != nil {
		if !bytes.Equal(j.input, input) {
			return nil, fmt.Errorf("logical request changed on replay")
		}
		return j.response, nil
	}
	j.input = input
	j.executions++
	response, err := perform()
	if err != nil {
		return nil, err
	}
	j.response, err = json.Marshal(response)
	return j.response, err
}

func TestLocallyVerifiedLeafKeepsJournalRequestAndReplay(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if _, present := request["response_format"]; present {
			t.Error("marked tool request retained json_object")
		}
		writeVerifiedTestResponse(w, false)
	}))
	defer server.Close()
	journal := &verifiedRequestJournal{}
	llm := stdlib.NewJournaledLLM(NewProviderClient(ProviderConfig{BaseURL: server.URL, JSONObjectMode: true}), journal)
	request := verifiedTestRequest()
	expected, _ := json.Marshal(request)
	if _, err := llm.Chat(WithLocallyVerifiedOutput(t.Context(), *request.Schema), request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal.input, expected) {
		t.Fatal("journal did not retain the original schema-bearing request")
	}
	// Existing completed responses remain recorded facts, even if the leaf
	// mapping context changes. Replay never manufactures another model call.
	if _, err := llm.Chat(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || journal.executions != 1 {
		t.Fatal("replay repeated the provider call")
	}
}

func TestLocallyVerifiedOutputFallbackUsesEachProviderProfile(t *testing.T) {
	var formats []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model  string `json:"model"`
			Format *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		format := ""
		if request.Format != nil {
			format = request.Format.Type
		}
		formats = append(formats, format)
		if request.Model == "object" {
			http.Error(w, "fixture provider unavailable", http.StatusBadRequest)
			return
		}
		writeVerifiedTestResponse(w, false)
	}))
	defer server.Close()
	router := New("object")
	router.RegisterProvider(ProviderConfig{ID: "object", BaseURL: server.URL, Models: []string{"object"}, JSONObjectMode: true})
	router.RegisterProvider(ProviderConfig{ID: "native", BaseURL: server.URL, Models: []string{"native"}})
	llm := NewFallbackLLM(router, "object", []string{"native"}, 1)
	request := verifiedTestRequest()
	if _, err := llm.Chat(WithLocallyVerifiedOutput(t.Context(), *request.Schema), request); err != nil {
		t.Fatal(err)
	}
	if len(formats) != 2 || formats[0] != "" || formats[1] != "json_schema" {
		t.Fatalf("fallback formats=%v", formats)
	}
}
