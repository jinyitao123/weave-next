package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

type schemaTestLLM struct {
	responses []contract.ChatResponse
	requests  []contract.ChatRequest
}

func (l *schemaTestLLM) Chat(_ context.Context, r contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, r)
	if len(l.responses) == 0 {
		return nil, errors.New("unexpected model call")
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	return &response, nil
}
func (*schemaTestLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

var outputTestSchema = json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)

func schemaInput() loom.State {
	return loom.State{"messages": []contract.Message{{Role: "user", Content: "Produce the answer."}}}
}

func TestNodeOutputVerifierHardStopKeepsBoundedEvidence(t *testing.T) {
	bad := `{"answer":"private material","extra":"do not log this value"}`
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: bad}, {Content: bad}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 2})
	result, err := step(WithNodeOutputSchema(t.Context(), outputTestSchema), schemaInput())
	var violation *NodeOutputViolation
	if !errors.As(err, &violation) || !errors.Is(err, stdlib.ErrCompletionUnverified) {
		t.Fatalf("lost final verification failure: %v", err)
	}
	if len(llm.requests) != 2 || result["output"] != nil {
		t.Fatalf("hard stop published output or made extra calls: %#v", result)
	}
	encoded, _ := json.Marshal(violation)
	if violation.Path != "/extra" || len(violation.OutputSHA256) != 64 || violation.OutputBytes != len(bad) {
		t.Fatalf("diagnostic=%s", encoded)
	}
	if strings.Contains(string(encoded), "private material") || strings.Contains(err.Error(), "do not log") {
		t.Fatal("diagnostic leaked model content")
	}
}

func TestNodeOutputVerifierPreservesControlledRecoveryAndSchemaIdentity(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: `{"answer":"draft","extra":true}`}, {Content: `{"answer":"accepted"}`}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}})
	graph := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", step, loom.End())
	store := loom.NewMemStore()
	ctx := WithNodeOutputSchema(t.Context(), outputTestSchema)
	paused, err := graph.Run(ctx, schemaInput(), store)
	if err != nil || !paused.Yielded {
		t.Fatalf("expected bounded pause: %#v %v", paused, err)
	}
	outcome, present, err := stdlib.ReadToolLoopOutcome(paused.State)
	if err != nil || !present || outcome.TotalRoundsUsed != 1 {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	raw, _ := json.Marshal(paused.State["__checkpoint_seq"])
	var seq int64
	_ = json.Unmarshal(raw, &seq)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, stdlib.ToolLoopResumeGrant{ID: "resume", ExpectedRunID: paused.RunID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: paused.State["__yield_token"].(string), ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	// A different schema cannot consume a checkpoint under the same node.
	changed := WithNodeOutputSchema(t.Context(), json.RawMessage(`{"type":"object"}`))
	if _, err := graph.Resume(changed, paused.RunID, delta, store); err == nil {
		t.Fatal("changed schema resumed old policy")
	}
	if len(llm.requests) != 1 {
		t.Fatal("changed schema reached model")
	}
	completed, err := graph.Resume(ctx, paused.RunID, delta, store)
	if err != nil || completed.State["output"] != `{"answer":"accepted"}` {
		t.Fatalf("resume=%#v %v", completed, err)
	}
	if len(llm.requests) != 2 || !strings.Contains(llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content, "/extra") {
		t.Fatal("resume lost correction observation")
	}
}

func TestNodeOutputDiagnosticPathCannotCarryUnboundedModelText(t *testing.T) {
	path := "/" + strings.Repeat("合同", 2000) + "/line\nsecret"
	got := diagnosticFieldPath(path)
	if len(got) > 195 || !utf8.ValidString(got) || strings.Contains(got, "secret") || strings.Contains(got, "\n") {
		t.Fatalf("unsafe path=%q", got)
	}
}

type schemaEchoLLM struct{}

func (schemaEchoLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if request.Schema == nil {
		return nil, errors.New("missing invocation schema")
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(*request.Schema, &schema); err != nil || len(schema.Required) != 1 {
		return nil, errors.New("unexpected schema")
	}
	content, _ := json.Marshal(map[string]string{schema.Required[0]: "done"})
	return &contract.ChatResponse{Content: string(content)}, nil
}
func (schemaEchoLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

func TestNodeOutputBindingsAreIsolatedForConcurrentSharedSteps(t *testing.T) {
	step := nodeToolLoopStep(schemaEchoLLM{}, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 2})
	var group sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			schema, _ := json.Marshal(map[string]any{"type": "object", "required": []string{name}, "properties": map[string]any{name: map[string]any{"type": "string"}}, "additionalProperties": false})
			ctx := WithNodeOutputSchema(t.Context(), schema)
			// The binding must own the frozen bytes independently of its caller.
			for index := range schema {
				schema[index] = ' '
			}
			result, err := step(ctx, schemaInput())
			if err != nil || result["output"] != `{"`+name+`":"done"}` {
				t.Errorf("node %s: output=%#v err=%v", name, result, err)
			}
		}(name)
	}
	group.Wait()
}

const protocolCandidate = "I will inspect the provided material.\n\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read_frozen_material\">\n<｜｜DSML｜｜ parameter name=\"materialId\" string=\"true\">fixture-file</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"

func TestNodeOutputProtocolGuardCoversTextAndComposesWithJSON(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[structured], func(t *testing.T) {
			responses := []contract.ChatResponse{{Content: protocolCandidate}, {Content: "Verified inspection result."}}
			var schema json.RawMessage
			if structured {
				schema = outputTestSchema
				responses = []contract.ChatResponse{{Content: protocolCandidate}, {Content: `{"answer":"draft","extra":true}`}, {Content: `{"answer":"accepted"}`}}
			}
			llm := &schemaTestLLM{responses: responses}
			step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 3})
			result, err := step(WithNodeOutputSchema(t.Context(), schema), schemaInput())
			if err != nil || strings.Contains(result["output"].(string), "DSML") {
				t.Fatalf("protocol text became final result: %#v %v", result, err)
			}
			if len(llm.requests) != len(responses) {
				t.Fatalf("calls=%d want=%d", len(llm.requests), len(responses))
			}
			if !structured && llm.requests[1].Schema != nil {
				t.Fatal("text node acquired a JSON schema")
			}
			if structured && !strings.Contains(llm.requests[2].Messages[len(llm.requests[2].Messages)-1].Content, "/extra") {
				t.Fatal("protocol guard replaced exact JSON verifier")
			}
		})
	}
}

func TestNodeOutputProtocolGuardIsScopedAndCannotSynthesizeCalls(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: protocolCandidate}, {Content: protocolCandidate}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 1})
	// Ordinary agent graphs retain their previous opt-out behavior.
	result, err := step(t.Context(), schemaInput())
	if err != nil || result["output"] != protocolCandidate {
		t.Fatalf("unscoped behavior changed: %#v %v", result, err)
	}
	result, err = step(WithNodeOutputSchema(t.Context(), nil), schemaInput())
	var violation *NodeOutputViolation
	if !errors.As(err, &violation) || violation.Code != "unexecuted_tool_protocol" || result["output"] != nil {
		t.Fatalf("scoped protocol was accepted or executed: %#v %v", result, err)
	}
	if !errors.Is(err, stdlib.ErrCompletionUnverified) || len(llm.requests) != 2 {
		t.Fatal("protocol guard exceeded existing budget")
	}
}
