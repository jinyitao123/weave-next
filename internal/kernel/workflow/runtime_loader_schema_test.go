package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type structuredCLIExecutor struct{ schema *json.RawMessage }

func (e structuredCLIExecutor) ExecRemote(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, []execspec.Attachment) (engine.RunResult, error) {
	return engine.RunResult{Status: "completed", Output: "plain"}, nil
}

func (e structuredCLIExecutor) ExecRemoteStructured(_ context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, _ string, _ []execspec.Attachment, schema json.RawMessage) (engine.RunResult, error) {
	*e.schema = schema
	return engine.RunResult{Status: "completed", Output: `{"passed":true}`}, nil
}

func TestRuntimeCLIEntrySendsTheBoundNodeSchema(t *testing.T) {
	var received json.RawMessage
	entry, err := NewRuntimeCLIEntry(structuredCLIExecutor{schema: &received}, &registry.AgentRecord{WorkspaceID: "workspace-1", ID: "agent-1", Version: 1}, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := entry.ExecuteAccounted(t.Context(), "验证"); err != nil || result.Output != "plain" || received != nil {
		t.Fatalf("text node = %q %v schema=%s", result.Output, err, received)
	}
	schema := json.RawMessage(`{"type":"object","required":["passed"]}`)
	ctx := compiler.WithNodeOutputSchema(t.Context(), schema)
	if result, err := entry.ExecuteAccounted(ctx, "验证"); err != nil || result.Output != `{"passed":true}` || string(received) != string(schema) {
		t.Fatalf("json node = %q %v schema=%s", result.Output, err, received)
	}
}
