package mcphost

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom/contract"
)

// FallbackDispatcher is a ToolDispatcher that can attempt to handle unknown
// tool names. If it recognizes the name it returns a result; otherwise it
// returns nil to signal "not mine".
type FallbackDispatcher interface {
	contract.ToolDispatcher
	// TryDispatch attempts to handle a tool call that didn't match any
	// registered tool name. Returns (nil, nil) if the name is unrecognized.
	TryDispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error)
}

// CompositeDispatcher merges multiple ToolDispatchers into one.
// Tool calls are routed to the dispatcher that owns the tool.
// If no dispatcher claims a tool, FallbackDispatchers are tried in order.
type CompositeDispatcher struct {
	dispatchers []contract.ToolDispatcher
	toolIndex   map[string]contract.ToolDispatcher
	mu          sync.Mutex
	indexed     bool
}

// NewCompositeDispatcher creates a dispatcher that combines tools from multiple sources.
func NewCompositeDispatcher(dispatchers ...contract.ToolDispatcher) *CompositeDispatcher {
	return &CompositeDispatcher{
		dispatchers: dispatchers,
		toolIndex:   make(map[string]contract.ToolDispatcher),
	}
}

func (c *CompositeDispatcher) buildIndex(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.indexed {
		return nil
	}

	owners := make(map[string]int)
	candidate := make(map[string]contract.ToolDispatcher)
	for i, d := range c.dispatchers {
		tools, err := d.ListTools(ctx)
		if err != nil {
			if errors.Is(err, ErrFailClosed) {
				return err // deliberate governance rejection must not be swallowed
			}
			continue // skip merely-unhealthy dispatchers
		}
		for _, t := range tools {
			if previous, exists := owners[t.Name]; exists {
				return fmt.Errorf(
					"%w: tool-name collision %q between dispatchers %d and %d",
					ErrFailClosed, t.Name, previous, i,
				)
			}
			owners[t.Name] = i
			candidate[t.Name] = d
		}
	}
	c.toolIndex = candidate
	c.indexed = true
	return nil
}

// ListTools returns the union of all tools from all dispatchers.
func (c *CompositeDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	var all []contract.ToolDef
	owners := make(map[string]int)

	for i, d := range c.dispatchers {
		tools, err := d.ListTools(ctx)
		if err != nil {
			if errors.Is(err, ErrFailClosed) {
				// A wrapped governance rejection (disabled/deleted/cross-workspace/
				// missing ref/tool-name collision/resolver-unavailable) must fail the
				// whole listing so the ToolLoop aborts, matching the sub-agent path.
				return nil, err
			}
			continue
		}
		for _, t := range tools {
			if previous, exists := owners[t.Name]; exists {
				return nil, fmt.Errorf(
					"%w: tool-name collision %q between dispatchers %d and %d",
					ErrFailClosed, t.Name, previous, i,
				)
			}
			owners[t.Name] = i
			all = append(all, t)
		}
	}

	// Build index as side effect.
	if err := c.buildIndex(ctx); err != nil {
		return nil, err
	}

	return all, nil
}

// Dispatch routes the call to the dispatcher that owns the tool.
// If no dispatcher claims the tool, FallbackDispatchers are tried in order.
func (c *CompositeDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if err := c.buildIndex(ctx); err != nil {
		return nil, err
	}

	// Exact match in index.
	if d, ok := c.toolIndex[call.Name]; ok {
		return d.Dispatch(ctx, call)
	}

	// Try fallback dispatchers.
	for _, d := range c.dispatchers {
		if fb, ok := d.(FallbackDispatcher); ok {
			result, err := fb.TryDispatch(ctx, call)
			if result != nil || err != nil {
				return result, err
			}
			// (nil, nil) means "not mine, try next"
		}
	}

	return &contract.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("tool %q not found", call.Name),
		IsError: true,
	}, nil
}

// Compile-time interface check.
var _ contract.ToolDispatcher = (*CompositeDispatcher)(nil)
