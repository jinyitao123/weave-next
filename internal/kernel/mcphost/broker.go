package mcphost

import (
	"context"
	"fmt"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// ToolBroker is the single assembly point for the complete in-process tool
// pipeline. MCP tools retain the access factory's resolve, authorize, gate,
// and audit layers; platform tools are merged only at the governed boundary.
type ToolBroker struct {
	access *MCPAccessFactory
}

// NewToolBroker creates a broker backed by the existing governed MCP access
// factory.
func NewToolBroker(access *MCPAccessFactory) *ToolBroker {
	return &ToolBroker{access: access}
}

// PlatformToolBuilder materializes non-MCP tools from the already-resolved
// per-workspace runtime snapshots supplied to the broker.
type PlatformToolBuilder func(contract.LLM, *memory.Service) []contract.ToolDispatcher

// ToolBrokerRequest contains one run's immutable tool assembly inputs.
type ToolBrokerRequest struct {
	WorkspaceID    string
	Agent          *registry.AgentRecord
	ConversationID string
	LLM            contract.LLM
	Memory         *memory.Service
	PlatformTools  PlatformToolBuilder
}

// Build assembles governed MCP tools with any platform dispatchers. Resolution
// failures remain a dispatcher-level fail-closed error so chat compilation
// observes the same behavior as before broker adoption.
func (b *ToolBroker) Build(
	ctx context.Context,
	request ToolBrokerRequest,
) contract.ToolDispatcher {
	var access *MCPAccessFactory
	if b != nil {
		access = b.access
	}
	mcp, err := access.Build(ctx, request.WorkspaceID, request.Agent, request.ConversationID)
	if err != nil {
		mcp = NewRejectedMCPDispatcher(err)
	}
	var platform []contract.ToolDispatcher
	if request.PlatformTools != nil {
		platform = request.PlatformTools(request.LLM, request.Memory)
	}
	dispatchers := make([]contract.ToolDispatcher, 0, 1+len(platform))
	dispatchers = append(dispatchers, mcp)
	dispatchers = append(dispatchers, platform...)
	return NewCompositeDispatcher(dispatchers...)
}

// BuildMCPServerAt builds one index-addressed MCP boundary through the same
// governed pipeline as Build, without exposing the other agent connections.
func (b *ToolBroker) BuildMCPServerAt(
	ctx context.Context,
	request ToolBrokerRequest,
	index int,
) (contract.ToolDispatcher, error) {
	if b == nil || b.access == nil {
		return nil, fmt.Errorf("%w: governed MCP access is unavailable", ErrFailClosed)
	}
	dispatcher, err := b.access.BuildServerAt(
		ctx, request.WorkspaceID, request.Agent, index, request.ConversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFailClosed, err)
	}
	return dispatcher, nil
}
