package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
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
	d := &FrozenMCPDispatcher{inner: host, binding: frozen.FrozenMCPBinding{ServerID: "server", Tools: []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"properties":{},"type":"object"}`)}}}}
	if _, err := d.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "one", Name: "calculate", Args: `{}`}); err != nil {
		t.Fatal(err)
	}
	host.schema = json.RawMessage(`{"type":"object","required":["new_input"]}`)
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "two", Name: "calculate"}); !errors.Is(err, ErrFailClosed) {
		t.Fatalf("schema drift=%v", err)
	}
	if _, err := d.Dispatch(t.Context(), contract.ToolCall{ID: "three", Name: "unpublished"}); !errors.Is(err, ErrFailClosed) {
		t.Fatalf("unknown tool=%v", err)
	}
	if host.calls != 1 {
		t.Fatalf("unverified effect executed: %d calls", host.calls)
	}
}

func TestFrozenToolsRejectRedirectedEndpoint(t *testing.T) {
	var calls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	binding := frozen.FrozenMCPBinding{WorkspaceID: "ws", ServerID: "server", Transport: "http", URL: origin.URL, Tools: []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	dispatcher, err := NewMCPAccessFactory(nil, nil).BuildFrozenServer("ws", &registry.AgentRecord{WorkspaceID: "ws", Name: "worker"}, binding, map[string]string{"Authorization": "Bearer fixture-private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "redirect", Name: "calculate", Args: `{}`}); err == nil || calls.Load() != 0 {
		t.Fatalf("frozen endpoint redirected: err=%v requests=%d", err, calls.Load())
	}
}
