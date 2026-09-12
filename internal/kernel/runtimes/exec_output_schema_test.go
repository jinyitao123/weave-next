package runtimes

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestBuildExecPayloadUsesAgentOutputSchemaOnlyAsFallback(t *testing.T) {
	agentSchema := json.RawMessage(`{"type":"object","required":["agent"]}`)
	explicitSchema := json.RawMessage(`{"type":"object","required":["workflow"]}`)
	record := &registry.AgentRecord{
		Name:         "worker",
		Engine:       engine.Codex,
		OutputSchema: &agentSchema,
	}
	executor := &Executor{}

	fallback := executor.buildExecPayloadWithSchema("workspace", record, "task", nil, nil)
	if string(fallback.OutputSchema) != string(agentSchema) {
		t.Fatalf("fallback output schema = %s, want %s", fallback.OutputSchema, agentSchema)
	}

	explicit := executor.buildExecPayloadWithSchema("workspace", record, "task", nil, explicitSchema)
	if string(explicit.OutputSchema) != string(explicitSchema) {
		t.Fatalf("explicit output schema = %s, want %s", explicit.OutputSchema, explicitSchema)
	}

	agentSchema[0] = '['
	explicitSchema[0] = '['
	if string(fallback.OutputSchema) != `{"type":"object","required":["agent"]}` ||
		string(explicit.OutputSchema) != `{"type":"object","required":["workflow"]}` {
		t.Fatal("payload output schema aliases mutable request or agent storage")
	}
}
