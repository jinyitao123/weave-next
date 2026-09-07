package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

type runtimeFrozenMCPDispatcher struct {
	inner   contract.ToolDispatcher
	binding frozen.FrozenMCPBinding
}

func (d *runtimeFrozenMCPDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	actual, err := d.inner.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q tool catalog is unavailable", mcphost.ErrFailClosed, d.binding.ServerID)
	}
	definitions := make([]frozen.FrozenToolDefinition, 0, len(actual))
	for _, tool := range actual {
		definitions = append(definitions, frozen.FrozenToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, ReadOnly: tool.ReadOnly})
	}
	normalized, err := frozen.NormalizeToolDefinitions(definitions)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q returned an invalid tool contract", mcphost.ErrFailClosed, d.binding.ServerID)
	}
	expected, err := frozen.NormalizeToolDefinitions(d.binding.Tools)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q frozen tool contract is invalid", mcphost.ErrFailClosed, d.binding.ServerID)
	}
	rawActual, _ := json.Marshal(normalized)
	rawExpected, _ := json.Marshal(expected)
	if !bytes.Equal(rawActual, rawExpected) {
		return nil, fmt.Errorf("%w: MCP server %q tool definitions changed since publication; re-probe and publish a new version", mcphost.ErrFailClosed, d.binding.ServerID)
	}
	return actual, nil
}

func (d *runtimeFrozenMCPDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if !slices.ContainsFunc(d.binding.Tools, func(tool frozen.FrozenToolDefinition) bool { return tool.Name == call.Name }) {
		return nil, fmt.Errorf("%w: tool %q was not frozen for MCP server %q", mcphost.ErrFailClosed, call.Name, d.binding.ServerID)
	}
	// Recheck before an effect too: a cached composite index is not evidence
	// that the remote callable contract is still the one supplied to the model.
	if _, err := d.ListTools(ctx); err != nil {
		return nil, err
	}
	return d.inner.Dispatch(ctx, call)
}
