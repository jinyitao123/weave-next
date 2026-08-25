package mcpregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const (
	CodeDependencyVersionRequired = "workflow_dependency_version_required"
	CodeCredentialUnavailable     = "workflow_credential_unavailable"
	CodeFrozenManifestMismatch    = "workflow_frozen_manifest_mismatch"
)

var (
	ErrDependencyVersionRequired = &Error{code: CodeDependencyVersionRequired}
	ErrCredentialUnavailable     = &Error{code: CodeCredentialUnavailable}
	ErrFrozenManifestMismatch    = &Error{code: CodeFrozenManifestMismatch}
)

type MCPAgentPolicy struct {
	Filter     []string
	WriteTools []string
}

func decodeMCPArgs(raw []byte) ([]string, error) {
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize MCP arguments: %w", err)
	}

	var decoded []*string
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		return nil, fmt.Errorf("decode MCP arguments: %w", err)
	}
	if decoded == nil {
		return nil, errors.New("MCP arguments must be an array")
	}

	args := make([]string, len(decoded))
	for index, arg := range decoded {
		if arg == nil {
			return nil, errors.New("MCP arguments contain a null element")
		}
		args[index] = *arg
	}
	return args, nil
}

func ResolveMCPRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, serverID string,
	dependencyVersion *int64,
	policy MCPAgentPolicy,
) (frozen.FrozenMCPBinding, error) {
	if tx == nil ||
		workspaceID == "" ||
		serverID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		serverID != strings.TrimSpace(serverID) ||
		dependencyVersion == nil ||
		*dependencyVersion < 1 ||
		*dependencyVersion > frozen.MaxJCSSafeInteger {
		return frozen.FrozenMCPBinding{}, coded(
			ErrDependencyVersionRequired,
			"exact workspace, server identity, and functional revision are required",
		)
	}

	var (
		transport string
		url       *string
		command   *string
		argsJSON  []byte
		enabled   bool
		revokedAt *time.Time
		deletedAt *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT transport, url, command, args, enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2 AND functional_revision=$3
		FOR SHARE
	`, workspaceID, serverID, *dependencyVersion).Scan(
		&transport, &url, &command, &argsJSON, &enabled, &revokedAt, &deletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozen.FrozenMCPBinding{}, coded(
			ErrDependencyVersionRequired,
			"exact MCP functional revision is unavailable",
		)
	}
	if err != nil {
		return frozen.FrozenMCPBinding{}, fmt.Errorf("resolve MCP functional revision: %w", err)
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return frozen.FrozenMCPBinding{}, coded(
			ErrCredentialUnavailable,
			"MCP server access is closed",
		)
	}

	args, err := decodeMCPArgs(argsJSON)
	if err != nil {
		return frozen.FrozenMCPBinding{}, coded(
			ErrFrozenManifestMismatch,
			"stored MCP arguments are malformed",
		)
	}

	binding := frozen.FrozenMCPBinding{
		SchemaVersion:  frozen.FrozenSchemaVersion,
		WorkspaceID:    workspaceID,
		ServerID:       serverID,
		ServerRevision: *dependencyVersion,
		Args:           args,
		Filter:         append([]string(nil), policy.Filter...),
		WriteTools:     append([]string(nil), policy.WriteTools...),
		AccessRef: frozen.CredentialReference{
			SchemaVersion:     frozen.FrozenSchemaVersion,
			WorkspaceID:       workspaceID,
			Kind:              frozen.CredentialMCPServerAccess,
			ResourceID:        serverID,
			Slot:              "access",
			CredentialVersion: nil,
		},
	}
	switch transport {
	case string(TransportStreamableHTTP):
		binding.Transport = "http"
		if url != nil {
			binding.URL = *url
		}
		if command != nil {
			binding.Command = *command
		}
	case string(TransportStdio):
		binding.Transport = "stdio"
		if url != nil {
			binding.URL = *url
		}
		if command != nil {
			binding.Command = *command
		}
	default:
		return frozen.FrozenMCPBinding{}, coded(
			ErrFrozenManifestMismatch,
			"stored MCP transport is unsupported",
		)
	}

	contentHash, err := frozen.HashDTO(binding, frozen.PreorderFrozenMCPBinding)
	if err != nil {
		return frozen.FrozenMCPBinding{}, coded(
			ErrFrozenManifestMismatch,
			"stored MCP function or Agent policy is invalid",
		)
	}
	binding.ContentHash = contentHash

	raw, err := json.Marshal(binding)
	if err != nil {
		return frozen.FrozenMCPBinding{}, coded(
			ErrFrozenManifestMismatch,
			"encode frozen MCP binding",
		)
	}
	normalized, err := frozen.DecodeFrozenMCPBinding(raw)
	if err != nil {
		return frozen.FrozenMCPBinding{}, coded(
			ErrFrozenManifestMismatch,
			"normalize frozen MCP binding",
		)
	}
	if normalized.Args == nil {
		normalized.Args = []string{}
	}
	return normalized, nil
}
