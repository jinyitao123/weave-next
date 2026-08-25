package mcpregistry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

var ErrResolverUnavailable = errors.New("MCP resolver registry is unavailable")

// ResolverStore is the workspace-scoped registry surface required to resolve
// an agent's stable MCP server references.
type ResolverStore interface {
	Resolve(ctx context.Context, workspaceID, id string) (ResolvedServer, error)
	Catalog(ctx context.Context, workspaceID, id string) (ProbeResult, error)
}

// ResolvedAccess is internal-only connection material plus the agent-scoped
// policy that must be applied before the server joins a tool dispatcher.
type ResolvedAccess struct {
	ServerID    string
	Transport   Transport
	URL         string
	Headers     map[string]string
	Command     string
	Args        []string
	Env         map[string]string
	Filter      []string
	WriteTools  []string
	Tools       []string
	Legacy      bool
	LegacyIndex int
}

// Resolver turns versioned AgentRecord MCP declarations into governed runtime
// access. A nil store intentionally supports legacy inline records only.
type Resolver struct {
	store ResolverStore
}

func NewResolver(store ResolverStore) *Resolver {
	return &Resolver{store: store}
}

// ResolveAgent resolves every declared MCP server and validates the current
// catalog before returning any registry connection material.
func (r *Resolver) ResolveAgent(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
) ([]ResolvedAccess, error) {
	if rec == nil {
		return nil, errors.New("resolve MCP access: agent record is missing")
	}

	accesses := make([]ResolvedAccess, 0, len(rec.MCPServers))
	seenRefs := make(map[string]struct{})
	exposedBy := make(map[string]string)
	for index, cfg := range rec.MCPServers {
		if cfg.ServerID == "" {
			if strings.TrimSpace(cfg.URL) == "" {
				return nil, fmt.Errorf("resolve MCP access %d: missing server_id and legacy URL", index)
			}
			accesses = append(accesses, ResolvedAccess{
				URL: cfg.URL, Headers: cloneStringMap(cfg.Headers),
				Filter: cloneStrings(cfg.Filter), WriteTools: cloneStrings(cfg.WriteTools),
				Legacy: true, LegacyIndex: index,
			})
			continue
		}
		if strings.TrimSpace(cfg.URL) != "" || cfg.Headers != nil {
			return nil, fmt.Errorf("resolve MCP server %q: ambiguous config mixes server_id with inline connection material", cfg.ServerID)
		}
		if _, exists := seenRefs[cfg.ServerID]; exists {
			return nil, fmt.Errorf("resolve MCP server %q: duplicate server ref", cfg.ServerID)
		}
		seenRefs[cfg.ServerID] = struct{}{}
		if r == nil || r.store == nil {
			return nil, fmt.Errorf("resolve MCP server %q: %w", cfg.ServerID, ErrResolverUnavailable)
		}

		resolved, err := r.store.Resolve(ctx, workspaceID, cfg.ServerID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("resolve MCP server %q: not found in workspace", cfg.ServerID)
			}
			return nil, fmt.Errorf("resolve MCP server %q: %w", cfg.ServerID, err)
		}
		if resolved.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("resolve MCP server %q: not found in workspace", cfg.ServerID)
		}
		if !resolved.Enabled || resolved.RevokedAt != nil || resolved.DeletedAt != nil {
			return nil, fmt.Errorf("resolve MCP server %q: %w", cfg.ServerID, coded(
				ErrClosed,
				"server is disabled, revoked, or deleted",
			))
		}
		switch resolved.Transport {
		case TransportStreamableHTTP:
			if strings.TrimSpace(resolved.URL) == "" {
				return nil, fmt.Errorf("resolve MCP server %q: unsupported connection", cfg.ServerID)
			}
		case TransportStdio:
			if strings.TrimSpace(resolved.Command) == "" {
				return nil, fmt.Errorf("resolve MCP server %q: unsupported connection", cfg.ServerID)
			}
		default:
			return nil, fmt.Errorf("resolve MCP server %q: unsupported connection", cfg.ServerID)
		}

		catalog, err := r.store.Catalog(ctx, workspaceID, cfg.ServerID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("resolve MCP server %q catalog: not found in workspace", cfg.ServerID)
			}
			return nil, fmt.Errorf("resolve MCP server %q catalog: %w", cfg.ServerID, err)
		}
		if catalog.Server.LastHandshakeAt == nil {
			return nil, fmt.Errorf("resolve MCP server %q: successful catalog probe is required", cfg.ServerID)
		}
		exposed, err := validatePolicy(cfg.ServerID, cfg.Filter, cfg.WriteTools, catalog.Tools)
		if err != nil {
			return nil, err
		}
		for tool := range exposed {
			if previous, exists := exposedBy[tool]; exists {
				return nil, fmt.Errorf("resolve MCP access: tool-name collision %q between servers %q and %q", tool, previous, cfg.ServerID)
			}
			exposedBy[tool] = cfg.ServerID
		}
		accesses = append(accesses, ResolvedAccess{
			ServerID: cfg.ServerID, Transport: resolved.Transport,
			URL: resolved.URL, Headers: cloneStringMap(resolved.Headers),
			Command: resolved.Command, Args: cloneStrings(resolved.Args), Env: cloneStringMap(resolved.Env),
			Filter: cloneStrings(cfg.Filter), WriteTools: cloneStrings(cfg.WriteTools), Tools: sortedToolNames(exposed),
			LegacyIndex: -1,
		})
	}
	return accesses, nil
}

func sortedToolNames(tools map[string]struct{}) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validatePolicy(serverID string, filter, writeTools []string, tools []Tool) (map[string]struct{}, error) {
	catalog := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		catalog[tool.Name] = struct{}{}
	}
	filterSet := make(map[string]struct{}, len(filter))
	for _, name := range filter {
		if _, ok := catalog[name]; !ok {
			return nil, fmt.Errorf("resolve MCP server %q: filter tool %q is absent from catalog", serverID, name)
		}
		filterSet[name] = struct{}{}
	}
	for _, name := range writeTools {
		if _, ok := catalog[name]; !ok {
			return nil, fmt.Errorf("resolve MCP server %q: write_tools entry %q is absent from catalog", serverID, name)
		}
		if len(filterSet) > 0 {
			if _, ok := filterSet[name]; !ok {
				return nil, fmt.Errorf("resolve MCP server %q: write_tools must be a subset of filter", serverID)
			}
		}
	}
	if len(filterSet) > 0 {
		return filterSet, nil
	}
	return catalog, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}
