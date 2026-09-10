package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type FrozenMCPDispatcher struct {
	inner        contract.ToolDispatcher
	binding      frozen.FrozenMCPBinding
	contractOnce sync.Once
	toolContract *ToolContract
	contractErr  error
}

func (d *FrozenMCPDispatcher) BoundContract() (*ToolContract, error) {
	d.contractOnce.Do(func() {
		definitions := make([]contract.ToolDef, 0, len(d.binding.Tools))
		for _, tool := range d.binding.Tools {
			definitions = append(definitions, contract.ToolDef{Name: tool.Name, Description: tool.Description,
				InputSchema: tool.InputSchema, ReadOnly: tool.ReadOnly})
		}
		d.toolContract, d.contractErr = NewToolContract(definitions)
	})
	return d.toolContract, d.contractErr
}

func (d *FrozenMCPDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	actual, err := d.inner.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q tool catalog is unavailable", ErrFailClosed, d.binding.ServerID)
	}
	definitions := make([]frozen.FrozenToolDefinition, 0, len(actual))
	for _, tool := range actual {
		definitions = append(definitions, frozen.FrozenToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, ReadOnly: tool.ReadOnly})
	}
	normalized, err := frozen.NormalizeToolDefinitions(definitions)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q returned an invalid tool contract", ErrFailClosed, d.binding.ServerID)
	}
	expected, err := frozen.NormalizeToolDefinitions(d.binding.Tools)
	if err != nil {
		return nil, fmt.Errorf("%w: MCP server %q frozen tool contract is invalid", ErrFailClosed, d.binding.ServerID)
	}
	rawActual, _ := json.Marshal(normalized)
	rawExpected, _ := json.Marshal(expected)
	if !bytes.Equal(rawActual, rawExpected) {
		return nil, fmt.Errorf("%w: MCP server %q tool definitions changed since publication; re-probe and publish a new version", ErrFailClosed, d.binding.ServerID)
	}
	return actual, nil
}

func (d *FrozenMCPDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if !slices.ContainsFunc(d.binding.Tools, func(tool frozen.FrozenToolDefinition) bool { return tool.Name == call.Name }) {
		return nil, fmt.Errorf("%w: tool %q was not frozen for MCP server %q", ErrFailClosed, call.Name, d.binding.ServerID)
	}
	// Recheck before an effect too: a cached composite index is not evidence
	// that the remote callable contract is still the one supplied to the model.
	if _, err := d.ListTools(ctx); err != nil {
		return nil, err
	}
	bound, err := d.BoundContract()
	if err != nil {
		return nil, err
	}
	if rejected := bound.Validate(call); rejected != nil {
		return rejected, nil
	}
	return d.inner.Dispatch(ctx, call)
}

// NewFrozenMCPDispatcher binds validation and live drift checks to a published
// server contract. The transport must already use that binding's connection.
func NewFrozenMCPDispatcher(inner contract.ToolDispatcher, binding frozen.FrozenMCPBinding) *FrozenMCPDispatcher {
	return &FrozenMCPDispatcher{inner: inner, binding: binding}
}
