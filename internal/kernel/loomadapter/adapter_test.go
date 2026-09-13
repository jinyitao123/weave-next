package loomadapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

type fakeLLM struct{}

func (fakeLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return &contract.ChatResponse{}, nil
}
func (fakeLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return make(chan contract.StreamChunk), nil
}

type fakeTools struct{}

func (fakeTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (fakeTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{}, nil
}

type fakeGraph struct{ ran bool }

func (g *fakeGraph) Run(context.Context) (RunResult, error) {
	g.ran = true
	return RunResult{Output: "done"}, nil
}

type fakeAssembler struct{ graph *fakeGraph }

func (a fakeAssembler) Assemble(context.Context, runtimeprotocol.ExecutionClaim, Ports) (Graph, error) {
	return a.graph, nil
}

func TestExecuteAcceptsOnlyValidatedLoomClaim(t *testing.T) {
	now := time.Now().UTC()
	frozenAgent := json.RawMessage(`{"id":"agent-1"}`)
	claim := runtimeprotocol.ExecutionClaim{
		SchemaVersion: runtimeprotocol.ClaimSchemaV1, TaskID: "task-1", WorkspaceID: "workspace-1",
		Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, ClaimEpoch: 1,
		LeaseIssuedAt: now, LeaseExpiresAt: now.Add(time.Minute),
		Agent:   runtimeprotocol.AgentIdentity{ID: "agent-1", Version: 1, Name: "worker", ExecutionScope: execution.ScopeTeamWorkerLeaf},
		Request: runtimeprotocol.ExecutionRequest{SchemaVersion: runtimeprotocol.RequestSchemaV1, Engine: "loom", FrozenAgent: frozenAgent, FrozenAgentHash: fmt.Sprintf("%x", sha256.Sum256(frozenAgent))},
	}
	graph := &fakeGraph{}
	result, err := Execute(t.Context(), fakeAssembler{graph: graph}, RunRequest{Claim: claim, Ports: Ports{Model: fakeLLM{}, Tools: fakeTools{}}})
	if err != nil || result.Output != "done" || !graph.ran {
		t.Fatalf("result=%+v ran=%v err=%v", result, graph.ran, err)
	}
	claim.Request.SchemaVersion = 2
	if _, err := Execute(t.Context(), fakeAssembler{graph: graph}, RunRequest{Claim: claim, Ports: Ports{Model: fakeLLM{}, Tools: fakeTools{}}}); err == nil {
		t.Fatal("unsupported claim reached graph assembly")
	}
}
