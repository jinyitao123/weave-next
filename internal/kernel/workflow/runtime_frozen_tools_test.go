package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

type changingToolCatalog struct {
	schema json.RawMessage
	calls  int
}

func (c *changingToolCatalog) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "calculate", InputSchema: c.schema}}, nil
}
func (c *changingToolCatalog) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	c.calls++
	return &contract.ToolResult{CallID: call.ID, Content: "42"}, nil
}

func TestFrozenToolsRejectSchemaDriftBeforeDispatch(t *testing.T) {
	host := &changingToolCatalog{schema: json.RawMessage(`{"type":"object", "properties":{}}`)}
	d := &runtimeFrozenMCPDispatcher{inner: host, binding: frozen.FrozenMCPBinding{ServerID: "server", Tools: []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"properties":{},"type":"object"}`)}}}}
	if _, err := d.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "one", Name: "calculate", Args: `{}`}); err != nil {
		t.Fatal(err)
	}
	host.schema = json.RawMessage(`{"type":"object","required":["new_input"]}`)
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "two", Name: "calculate"}); !errors.Is(err, mcphost.ErrFailClosed) {
		t.Fatalf("schema drift=%v", err)
	}
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "three", Name: "unpublished"}); !errors.Is(err, mcphost.ErrFailClosed) {
		t.Fatalf("unknown tool=%v", err)
	}
	if host.calls != 1 {
		t.Fatalf("unverified effect executed: %d calls", host.calls)
	}
}
