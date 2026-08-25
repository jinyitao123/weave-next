package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

var (
	// ErrRestoreVersionHashMismatch reports that the baseline-pinned agent
	// version's stored spec does not hash to the frozen pin content hash.
	ErrRestoreVersionHashMismatch = errors.New("agent restore version content hash mismatch")
	// ErrRestoreVersionIdentityMismatch reports that a baseline-pinned agent
	// version's frozen identity does not match its version key or the current
	// head's stable identity.
	ErrRestoreVersionIdentityMismatch = errors.New("agent restore version identity mismatch")
)

// AgentVersionRestoreResult is one agent restore outcome. Restored is false
// when the head already carried the exact baseline content (idempotent
// replay), in which case Version is the unchanged current head version.
type AgentVersionRestoreResult struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Restored bool   `json:"restored"`
}

// RestoreVersionTx restores one agent to a baseline-pinned historical version
// by copying the frozen spec as a new head version inside the caller-owned
// transaction. It never commits or rolls back the supplied transaction.
//
// The restore is locked by stable agent ID (workspace + id), verifies the
// head's stable ID/name and the pinned version's workspace/history hash, keeps
// the current legacy team context and owner/role protections, and only writes
// a new version when the head does not already carry the baseline content
// (content-idempotent replay: the same target content never mints a second
// version drift).
func (r *AgentRegistry) RestoreVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, agentID, operatorID string,
	pinnedVersion int,
	expectedContentHash string,
) (*AgentVersionRestoreResult, error) {
	if tx == nil {
		return nil, errors.New("restore agent version: transaction is required")
	}
	if workspaceID == "" || workspaceID != strings.TrimSpace(workspaceID) ||
		agentID == "" || agentID != strings.TrimSpace(agentID) ||
		operatorID == "" || operatorID != strings.TrimSpace(operatorID) {
		return nil, errors.New("restore agent version: workspace, agent id, and operator are required")
	}
	if pinnedVersion < 1 {
		return nil, errors.New("restore agent version: pinned version must be positive")
	}

	if err := lockOrganizationWorkspace(ctx, tx, workspaceID); err != nil {
		return nil, fmt.Errorf("restore agent version: %w", err)
	}

	var (
		headID        string
		headName      string
		headTeamID    *string
		headOwnerUser *string
		headDisplay   string
		headRole      string
		headSpec      []byte
		headVersion   int
		headCreatedAt time.Time
		headUpdatedAt time.Time
		headDeleted   bool
	)
	err := tx.QueryRow(ctx, `
		SELECT id, name, team_id, owner_user_id, display_name, role, spec,
		       version, created_at, updated_at, deleted
		FROM weave_agents
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, agentID).Scan(
		&headID, &headName, &headTeamID, &headOwnerUser, &headDisplay,
		&headRole, &headSpec, &headVersion, &headCreatedAt, &headUpdatedAt,
		&headDeleted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"restore agent version: agent %q not found in workspace %q",
			agentID,
			workspaceID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("restore agent version: lock agent head: %w", err)
	}
	if headID != agentID {
		return nil, fmt.Errorf(
			"restore agent version: locked head %q does not match stable id %q",
			headID,
			agentID,
		)
	}
	if headDeleted {
		return nil, fmt.Errorf(
			"restore agent version: agent %q is deleted in workspace %q",
			agentID,
			workspaceID,
		)
	}

	var pinnedSpec []byte
	err = tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
	`, workspaceID, agentID, pinnedVersion).Scan(&pinnedSpec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"restore agent version: pinned version %d of agent %q not found in workspace %q",
			pinnedVersion,
			agentID,
			workspaceID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("restore agent version: read pinned version: %w", err)
	}

	pinnedHash, err := canonicalAgentSpecHash(pinnedSpec)
	if err != nil {
		return nil, fmt.Errorf("restore agent version: hash pinned version: %w", err)
	}
	if pinnedHash != expectedContentHash {
		return nil, fmt.Errorf(
			"restore agent version: %w: agent %q pinned version %d",
			ErrRestoreVersionHashMismatch,
			agentID,
			pinnedVersion,
		)
	}

	var pinned AgentRecord
	if err := json.Unmarshal(pinnedSpec, &pinned); err != nil {
		return nil, fmt.Errorf("restore agent version: decode pinned version: %w", err)
	}
	if pinned.ID != agentID ||
		pinned.WorkspaceID != workspaceID ||
		pinned.Version != pinnedVersion ||
		pinned.Name == "" {
		return nil, fmt.Errorf(
			"restore agent version: %w: agent %q pinned version %d",
			ErrRestoreVersionIdentityMismatch,
			agentID,
			pinnedVersion,
		)
	}
	if pinned.Name != headName {
		return nil, fmt.Errorf(
			"restore agent version: %w: pinned name %q does not match head name %q",
			ErrRestoreVersionIdentityMismatch,
			pinned.Name,
			headName,
		)
	}

	// Content-idempotent replay: when the head already carries the baseline
	// content (ignoring identity, ownership, and timestamps), the restore is a
	// no-op and no second version is minted.
	var headRecord AgentRecord
	if err := json.Unmarshal(headSpec, &headRecord); err != nil {
		return nil, fmt.Errorf("restore agent version: decode current head: %w", err)
	}
	headContentHash, err := agentRestoreContentHash(headRecord)
	if err != nil {
		return nil, fmt.Errorf("restore agent version: hash current head: %w", err)
	}
	pinnedContentHash, err := agentRestoreContentHash(pinned)
	if err != nil {
		return nil, fmt.Errorf("restore agent version: hash pinned content: %w", err)
	}
	if headContentHash == pinnedContentHash {
		return &AgentVersionRestoreResult{
			AgentID:  agentID,
			Name:     headName,
			Version:  headVersion,
			Restored: false,
		}, nil
	}

	// Build the restored spec: exact baseline content with the authoritative
	// head identity, the preserved legacy team context, the reused owner/role
	// protections, and the next version number.
	restored := pinned
	restored.ID = headID
	restored.WorkspaceID = workspaceID
	restored.Name = headName
	restored.TeamID = ""
	if headTeamID != nil {
		restored.TeamID = *headTeamID
	}
	restored.OwnerUserID = headOwnerUser
	restored.Role = headRole
	restored.DisplayName = pinned.DisplayName
	restored.Version = headVersion + 1
	restored.CreatedAt = headCreatedAt
	restored.UpdatedAt = time.Now().Truncate(time.Microsecond)
	restored.Deleted = false

	data, err := json.Marshal(restored)
	if err != nil {
		return nil, fmt.Errorf("restore agent version: encode restored spec: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_agents
		SET spec=$3, display_name=$4, version=$5, updated_at=$6, deleted=false
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, agentID, data, restored.DisplayName, restored.Version,
		restored.UpdatedAt); err != nil {
		return nil, fmt.Errorf("restore agent version: update agent head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agent_versions (workspace_id, agent_id, version, spec)
		VALUES ($1, $2, $3, $4)
	`, workspaceID, agentID, restored.Version, data); err != nil {
		return nil, fmt.Errorf("restore agent version: insert restored version: %w", err)
	}
	return &AgentVersionRestoreResult{
		AgentID:  agentID,
		Name:     headName,
		Version:  restored.Version,
		Restored: true,
	}, nil
}

// canonicalAgentSpecHash is the sha256 of the canonical frozen spec JSON, the
// same hash the baseline pin captured via ResolveAgentVersionContentTx.
func canonicalAgentSpecHash(spec []byte) (string, error) {
	canonical, err := frozen.CanonicalizeJSON(spec)
	if err != nil {
		return "", fmt.Errorf("canonicalize agent spec: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// agentRestoreContentHash hashes the mutable agent content of one record:
// identity, ownership, version, and timestamp fields are zeroed so a restored
// head (new version number / new updated_at) still compares equal to its
// frozen baseline content.
func agentRestoreContentHash(rec AgentRecord) (string, error) {
	value := rec
	value.ID = ""
	value.WorkspaceID = ""
	value.TeamID = ""
	value.OwnerUserID = nil
	value.Version = 0
	value.CreatedAt = time.Time{}
	value.UpdatedAt = time.Time{}
	value.Deleted = false
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode agent restore content: %w", err)
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize agent restore content: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
