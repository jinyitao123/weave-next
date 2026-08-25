package mcphost

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// ErrFailClosed marks a deliberate governance rejection (resolver fail-closed,
// or a fail-closed composite refusal). Outer dispatchers that skip
// merely-unhealthy sub-dispatchers MUST propagate this instead of swallowing
// it — otherwise governance silently degrades open on the wrapped path.
var ErrFailClosed = errors.New("mcp access fail-closed")

// AccessResolver resolves and validates an AgentRecord before any upstream
// connection is materialized.
type AccessResolver interface {
	ResolveAgent(ctx context.Context, workspaceID string, rec *registry.AgentRecord) ([]mcpregistry.ResolvedAccess, error)
}

// MCPAccessFactory is the governed MCP materialization primitive reused by the
// ToolBroker and the paths that have not yet migrated to it.
type MCPAccessFactory struct {
	resolver AccessResolver
	audit    GovernanceAuditRecorder
	newHost  func(mcpregistry.ResolvedAccess) contract.ToolDispatcher
}

func NewMCPAccessFactory(
	resolver AccessResolver,
	audit GovernanceAuditRecorder,
) *MCPAccessFactory {
	return &MCPAccessFactory{
		resolver: resolver, audit: audit,
		newHost: func(access mcpregistry.ResolvedAccess) contract.ToolDispatcher {
			opts := []HostOption{WithHeaders(access.Headers)}
			// Effective runtime allowlist: the agent's explicit filter if set,
			// otherwise the catalog-vetted tool set for registry refs. This pins
			// a no-filter registry ref to its last-probed catalog so tools added
			// upstream after probe are not silently listable/callable (fail-closed
			// to catalog). Legacy inline has no catalog and keeps expose-all until
			// migrated off inline (R4).
			filter := access.Filter
			if len(filter) == 0 && !access.Legacy {
				filter = access.Tools
			}
			if len(filter) > 0 {
				opts = append(opts, WithFilter(filter))
			}
			return NewHTTPHost(access.URL, opts...)
		},
	}
}

// Build resolves the current agent record and applies the fixed wrapper order
// host/filter -> write gate -> per-server audit.
func (f *MCPAccessFactory) Build(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	conversationID string,
) (contract.ToolDispatcher, error) {
	if rec == nil {
		return nil, fmt.Errorf("build MCP access: agent record is missing")
	}
	if len(rec.MCPServers) == 0 {
		return newFailClosedComposite(nil), nil
	}
	if f == nil || f.resolver == nil {
		return nil, fmt.Errorf("build MCP access: resolver is unavailable")
	}
	accesses, err := f.resolver.ResolveAgent(ctx, workspaceID, rec)
	if err != nil {
		return nil, err
	}
	return f.buildResolved(workspaceID, rec, conversationID, accesses), nil
}

// BuildServer validates the complete current AgentRecord, then materializes
// only the stable server selected by the gateway path.
func (f *MCPAccessFactory) BuildServer(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	serverID string,
	conversationID string,
) (contract.ToolDispatcher, error) {
	if rec == nil {
		return nil, fmt.Errorf("build MCP access: agent record is missing")
	}
	if serverID == "" {
		return nil, fmt.Errorf("build MCP access: server ID is missing")
	}
	if f == nil || f.resolver == nil {
		return nil, fmt.Errorf("build MCP access: resolver is unavailable")
	}
	accesses, err := f.resolver.ResolveAgent(ctx, workspaceID, rec)
	if err != nil {
		return nil, err
	}
	for _, access := range accesses {
		if !access.Legacy && access.ServerID == serverID {
			return f.buildOne(workspaceID, rec, conversationID, access).dispatcher, nil
		}
	}
	return nil, fmt.Errorf("build MCP access: server %q is not referenced by agent %q", serverID, rec.Name)
}

// BuildServerAt validates the complete current AgentRecord, then materializes
// only the connection selected by the index-addressed boundary. The selected
// server retains the same authorize, classify, park-or-execute, and audit
// wrappers used by the main broker path.
func (f *MCPAccessFactory) BuildServerAt(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	index int,
	conversationID string,
) (contract.ToolDispatcher, error) {
	if rec == nil {
		return nil, fmt.Errorf("build MCP access: agent record is missing")
	}
	if index < 0 || index >= len(rec.MCPServers) {
		return nil, fmt.Errorf("build MCP access: server index %d is out of range", index)
	}
	if f == nil || f.resolver == nil {
		return nil, fmt.Errorf("build MCP access: resolver is unavailable")
	}
	accesses, err := f.resolver.ResolveAgent(ctx, workspaceID, rec)
	if err != nil {
		return nil, err
	}
	selected := rec.MCPServers[index]
	for _, access := range accesses {
		if selected.ServerID != "" && !access.Legacy && access.ServerID == selected.ServerID {
			return f.buildOne(workspaceID, rec, conversationID, access).dispatcher, nil
		}
		if selected.ServerID == "" && access.Legacy && access.LegacyIndex == index {
			return f.buildOne(workspaceID, rec, conversationID, access).dispatcher, nil
		}
	}
	return nil, fmt.Errorf("build MCP access: server index %d is not configured for agent %q", index, rec.Name)
}

func (f *MCPAccessFactory) buildResolved(
	workspaceID string,
	rec *registry.AgentRecord,
	conversationID string,
	accesses []mcpregistry.ResolvedAccess,
) contract.ToolDispatcher {
	entries := make([]governedDispatcher, 0, len(accesses))
	for _, access := range accesses {
		entries = append(entries, f.buildOne(workspaceID, rec, conversationID, access))
	}
	return newFailClosedComposite(entries)
}

func (f *MCPAccessFactory) buildOne(
	workspaceID string,
	rec *registry.AgentRecord,
	conversationID string,
	access mcpregistry.ResolvedAccess,
) governedDispatcher {
	inner := f.newHost(access)
	label := access.ServerID
	if access.Legacy {
		label = fmt.Sprintf("legacy:%d", access.LegacyIndex)
		inner = NewWriteGateDispatcher(
			inner, workspaceID, rec.Name, access.URL,
			conversationID, access.WriteTools,
		)
		audited := NewAuditingDispatcher(inner, f.audit, workspaceID, rec.Name)
		audited.writeTools = append([]string(nil), access.WriteTools...)
		inner = audited
	} else {
		inner = NewWriteGateDispatcherForServer(
			inner, workspaceID, rec.Name, access.ServerID,
			access.URL, conversationID, access.WriteTools,
		)
		audited := NewAuditingDispatcherForServer(inner, f.audit, workspaceID, rec.Name, access.ServerID)
		audited.writeTools = append([]string(nil), access.WriteTools...)
		inner = audited
	}
	return governedDispatcher{id: label, dispatcher: inner}
}

type governedDispatcher struct {
	id         string
	dispatcher contract.ToolDispatcher
}

// failClosedComposite never skips an unhealthy server and rejects duplicate
// live tool names instead of silently selecting the first dispatcher.
type failClosedComposite struct {
	entries    []governedDispatcher
	mu         sync.Mutex
	loaded     bool
	loadErr    error
	tools      []contract.ToolDef
	toolByName map[string]contract.ToolDispatcher
}

func newFailClosedComposite(entries []governedDispatcher) *failClosedComposite {
	return &failClosedComposite{entries: entries}
}

func (c *failClosedComposite) load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.loadErr
	}
	c.loaded = true
	c.toolByName = make(map[string]contract.ToolDispatcher)
	owners := make(map[string]string)
	for _, entry := range c.entries {
		tools, err := entry.dispatcher.ListTools(ctx)
		if err != nil {
			c.loadErr = fmt.Errorf("%w: list MCP tools for %s: %w", ErrFailClosed, entry.id, err)
			return c.loadErr
		}
		for _, tool := range tools {
			if previous, exists := owners[tool.Name]; exists {
				c.loadErr = fmt.Errorf("%w: MCP tool-name collision %q between %s and %s", ErrFailClosed, tool.Name, previous, entry.id)
				return c.loadErr
			}
			owners[tool.Name] = entry.id
			c.toolByName[tool.Name] = entry.dispatcher
			c.tools = append(c.tools, tool)
		}
	}
	return nil
}

func (c *failClosedComposite) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return append([]contract.ToolDef(nil), c.tools...), nil
}

func (c *failClosedComposite) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	dispatcher, ok := c.toolByName[call.Name]
	if !ok {
		return &contract.ToolResult{CallID: call.ID, Content: fmt.Sprintf("tool %q not found", call.Name), IsError: true}, nil
	}
	return dispatcher.Dispatch(ctx, call)
}

type rejectedMCPDispatcher struct{ err error }

// NewRejectedMCPDispatcher represents a resolution failure without exposing a
// fallback path that could connect directly to an upstream server.
func NewRejectedMCPDispatcher(err error) contract.ToolDispatcher {
	return &rejectedMCPDispatcher{err: fmt.Errorf("%w: %w", ErrFailClosed, err)}
}

func (d *rejectedMCPDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return nil, d.err
}

func (d *rejectedMCPDispatcher) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, d.err
}

var _ contract.ToolDispatcher = (*failClosedComposite)(nil)
var _ contract.ToolDispatcher = (*rejectedMCPDispatcher)(nil)
