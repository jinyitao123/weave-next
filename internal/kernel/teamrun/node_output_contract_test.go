package teamrun

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type nodeContractLLM struct {
	requests  []contract.ChatRequest
	responses []string
}

func (l *nodeContractLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, request)
	if len(l.responses) == 0 {
		return nil, fmt.Errorf("unexpected model call")
	}
	content := l.responses[0]
	l.responses = l.responses[1:]
	return &contract.ChatResponse{Content: content, Usage: contract.Usage{InputTokens: 2, OutputTokens: 1}}, nil
}
func (*nodeContractLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

func TestAgentNodePassesExactOutputSchemaWithoutChangingOtherNodes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)
	llm := &nodeContractLLM{responses: []string{"plain analysis", `{"answer":"done"}`, "another plain analysis"}}
	record := &registry.AgentRecord{WorkspaceID: "ws", ID: "lead", Name: "lead", Version: 1, Model: "model", Spec: stdlib.AgentSpec{SystemPrompt: "Follow the node instruction."}, Compaction: &registry.CompactionConfig{Enabled: false}}
	graph, err := compiler.CompileAgent("ws", record, llm, &compiler.CompileGuardDispatcher{}, compiler.CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	entry := workflow.RuntimeGraphEntry{AgentID: "lead", AgentVersion: 1, Graph: graph}
	entries := map[string]workflow.RuntimeGraphEntry{runtimeEntryKey("lead", 1): entry}
	payload := frozen.ArtifactPayloadV1{Team: frozen.ArtifactTeamV1{LeadAgentID: "lead"}, Bundles: []frozen.FrozenExecutionBundle{{Agent: frozen.FrozenAgentRecord{AgentID: "lead", AgentVersion: 1}}}}
	for index, output := range []*machine.OutputContract{{Type: machine.ValueText}, {Type: machine.ValueJSON, Schema: schema}, {Type: machine.ValueText}} {
		node := machine.Node{ID: fmt.Sprintf("node-%d", index), Type: machine.NodeLead, Config: machine.LeadConfig{Instruction: "Produce the requested answer."}, Output: output}
		if _, _, err := runAgentNode(t.Context(), node, payload, entries, "input", nil, nil, "", nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(llm.requests) != 3 {
		t.Fatalf("model calls=%d", len(llm.requests))
	}
	if llm.requests[0].Schema != nil || llm.requests[2].Schema != nil {
		t.Fatal("JSON schema leaked into text node")
	}
	if llm.requests[1].Schema == nil || string(*llm.requests[1].Schema) != string(schema) {
		t.Fatalf("node schema not passed to model: %v", llm.requests[1].Schema)
	}
	if record.OutputSchema != nil {
		t.Fatal("shared agent record was mutated")
	}
}

func TestAgentNodeCorrectsExtraFieldInsideExistingLoomLoop(t *testing.T) {
	schema := machine.WorkbenchResultSchemaV1()
	llm := &nodeContractLLM{responses: []string{
		`{"disposition":"needs_input","summary":"Need attachment","missing_items":["Technical attachment"],"extra":"not allowed"}`,
		`{"disposition":"needs_input","summary":"Need attachment","missing_items":["Technical attachment"]}`,
	}}
	graph, err := compiler.CompileAgent("ws", &registry.AgentRecord{ID: "worker", Name: "worker", Model: "model", Compaction: &registry.CompactionConfig{Enabled: false}}, llm, &compiler.CompileGuardDispatcher{}, compiler.CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	node := machine.Node{ID: "summarize", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "worker", AgentVersion: 1}, Output: &machine.OutputContract{Type: machine.ValueJSON, Schema: schema}}
	output, _, err := runAgentNode(t.Context(), node, frozen.ArtifactPayloadV1{}, map[string]workflow.RuntimeGraphEntry{runtimeEntryKey("worker", 1): {AgentID: "worker", AgentVersion: 1, Graph: graph}}, "input", nil, nil, "", nil, nil, true)
	if err != nil {
		t.Fatalf("model correction was not allowed within existing loop: %v", err)
	}
	want := map[string]any{"disposition": "needs_input", "summary": "Need attachment", "missing_items": []any{"Technical attachment"}}
	if !reflect.DeepEqual(output, want) || len(llm.requests) != 2 {
		t.Fatalf("output=%#v calls=%d", output, len(llm.requests))
	}
	feedback := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content
	if !strings.Contains(feedback, "/extra") || !strings.Contains(feedback, "workflow_field_unknown") {
		t.Fatalf("feedback lacks contract problem: %q", feedback)
	}
}

func TestAgentNodeInvalidOutputErrorIncludesBoundedFieldPath(t *testing.T) {
	node := machine.Node{ID: "summarize", Output: &machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()}}
	_, err := normalizeAgentNodeOutput(node, `{"disposition":"complete","summary":"done","missing_items":[],"extra":"private value"}`)
	if err == nil || !strings.Contains(err.Error(), "/extra") {
		t.Fatalf("field path missing: %v", err)
	}
	if strings.Contains(err.Error(), "private value") {
		t.Fatal("invalid output contents leaked into error")
	}
}

func TestAgentNodeFailureRecordsDiagnosticWithoutPublishingInvalidOutput(t *testing.T) {
	member := loom.NewGraph("fixture-member", "answer")
	member.AddStep("answer", func(context.Context, loom.State) (loom.State, error) {
		return loom.State{"output": `{"answer":"private material","extra":"private value"}`}, nil
	}, loom.End())
	node := machine.Node{ID: "summarize", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "worker", AgentVersion: 1}, Output: &machine.OutputContract{Type: machine.ValueJSON, Schema: json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)}}
	graph := machine.GraphDefinition{EntryNodeID: node.ID, Nodes: []machine.Node{node, {ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: node.ID}}}}, Edges: []machine.Edge{{FromNodeID: node.ID, ToNodeID: "deliver", Route: machine.RouteSuccess}}}
	var failed map[string]any
	published := false
	result := runSerialMachine(t.Context(), graph, frozen.ArtifactPayloadV1{}, &workflow.RuntimeArtifact{Entries: []workflow.RuntimeGraphEntry{{AgentID: "worker", AgentVersion: 1, Graph: member}}}, "input", serialMachineStart{
		Run: TeamRun{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", TeamID: "team", WorkflowID: "workflow", WorkflowVersion: 1},
		RecordActivity: func(_ context.Context, kind string, _ machine.Node, _ string, _ int64, detail map[string]any) {
			if kind == "member_failed" {
				failed = detail
			}
		},
		RecordOutput: func(context.Context, machine.Node, any, bool) error { published = true; return nil },
	})
	if result.Status != serialFailed || published || failed == nil {
		t.Fatalf("invalid result=%#v published=%v failed=%#v", result, published, failed)
	}
	encoded, _ := json.Marshal(failed)
	if !strings.Contains(string(encoded), `"path":"/extra"`) || !strings.Contains(string(encoded), "output_sha256") || strings.Contains(string(encoded), "private") {
		t.Fatalf("unsafe or missing diagnostic: %s", encoded)
	}
}
