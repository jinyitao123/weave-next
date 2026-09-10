package mcpregistry

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// ResolveCurrentMCPToolsTx is a publication-only resolver. The server share
// lock also excludes concurrent catalog replacement, whose writer holds an
// update lock on this row. Runtime consumers use the resulting frozen bundle.
func ResolveCurrentMCPToolsTx(ctx context.Context, tx pgx.Tx, workspaceID, serverID string, policy MCPAgentPolicy) (frozen.FrozenMCPBinding, error) {
	if tx == nil || workspaceID == "" || serverID == "" {
		return frozen.FrozenMCPBinding{}, coded(ErrDependencyVersionRequired, "MCP publication identity is required")
	}
	var revision int64
	var probed bool
	err := tx.QueryRow(ctx, `SELECT functional_revision, last_handshake_at IS NOT NULL
		FROM weave_mcp_servers WHERE workspace_id=$1 AND id=$2 AND enabled=true
		AND revoked_at IS NULL AND deleted_at IS NULL FOR SHARE`, workspaceID, serverID).Scan(&revision, &probed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return frozen.FrozenMCPBinding{}, fmt.Errorf("read MCP publication revision: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) || !probed {
		return frozen.FrozenMCPBinding{}, coded(ErrDependencyVersionRequired, fmt.Sprintf("MCP server %q requires an available, successfully probed revision", serverID))
	}
	binding, err := ResolveMCPRevisionTx(ctx, tx, workspaceID, serverID, &revision, policy)
	if err != nil {
		return frozen.FrozenMCPBinding{}, err
	}
	if binding.Transport != "http" {
		return frozen.FrozenMCPBinding{}, coded(ErrFrozenManifestMismatch, fmt.Sprintf("MCP server %q requires HTTP transport for a frozen member", serverID))
	}
	rows, err := tx.Query(ctx, `SELECT name, description, input_schema, COALESCE(read_only_hint,false)
		FROM weave_mcp_tools WHERE workspace_id=$1 AND server_id=$2 ORDER BY name`, workspaceID, serverID)
	if err != nil {
		return frozen.FrozenMCPBinding{}, fmt.Errorf("read frozen MCP tool catalog: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tool frozen.FrozenToolDefinition
		if err := rows.Scan(&tool.Name, &tool.Description, &tool.InputSchema, &tool.ReadOnly); err != nil {
			return frozen.FrozenMCPBinding{}, err
		}
		if len(binding.Filter) == 0 || slices.Contains(binding.Filter, tool.Name) {
			binding.Tools = append(binding.Tools, tool)
		}
	}
	if err := rows.Err(); err != nil {
		return frozen.FrozenMCPBinding{}, err
	}
	if len(binding.Tools) == 0 {
		return frozen.FrozenMCPBinding{}, coded(ErrFrozenManifestMismatch, fmt.Sprintf("MCP server %q has no available tools after filtering", serverID))
	}
	for _, name := range binding.Filter {
		if !slices.ContainsFunc(binding.Tools, func(tool frozen.FrozenToolDefinition) bool { return tool.Name == name }) {
			return frozen.FrozenMCPBinding{}, coded(ErrFrozenManifestMismatch, fmt.Sprintf("MCP server %q is missing required tool %q", serverID, name))
		}
	}
	binding.Tools, err = frozen.NormalizeToolDefinitions(binding.Tools)
	if err != nil {
		return frozen.FrozenMCPBinding{}, coded(ErrFrozenManifestMismatch, fmt.Sprintf("MCP server %q has an invalid tool contract", serverID))
	}
	binding.ContentHash, err = frozen.HashDTO(binding, frozen.PreorderFrozenMCPBinding)
	return binding, err
}
