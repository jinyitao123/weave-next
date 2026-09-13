package agentcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/registry"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// AgentRegistry provides CRUD operations on agent specs.
type AgentRegistry struct {
	pool           *pgxpool.Pool
	memberVerifier WorkspaceMemberVerifier
}

// New creates an AgentRegistry backed by PostgreSQL.
func New(pool *pgxpool.Pool, options ...Option) *AgentRegistry {
	r := &AgentRegistry{pool: pool}
	for _, option := range options {
		option(r)
	}
	return r
}

// Get retrieves the latest version of an agent.
func (r *AgentRegistry) Get(ctx context.Context, tenant, name string) (*registry.AgentRecord, error) {
	var visibility string
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, team_id, owner_user_id, display_name, role, spec, version, visibility
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, name)
	rec, err := scanAgentRecord(row, tenant, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// GetVersion retrieves one immutable historical AgentRecord by stable ID and
// version. The workspace join is an authorization boundary; a missing or
// foreign version is never replaced with the current record.
func (r *AgentRegistry) GetVersion(ctx context.Context, tenant, agentID string, version int) (*registry.AgentRecord, error) {
	var data []byte
	err := r.pool.QueryRow(ctx, `
		SELECT version.spec
		FROM weave_agent_versions AS version
		JOIN weave_agents AS agent
		  ON agent.workspace_id=version.workspace_id
		 AND agent.id=version.agent_id
		WHERE version.workspace_id=$1
		  AND version.agent_id=$2
		  AND version.version=$3
	`, tenant, agentID, version).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("agent version %q@%d not found for tenant %q", agentID, version, tenant)
	}
	if err != nil {
		return nil, err
	}

	var rec registry.AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("corrupt agent version %q@%d: %w", agentID, version, err)
	}
	if rec.ID != agentID || rec.WorkspaceID != tenant || rec.Version != version || rec.Name == "" {
		return nil, fmt.Errorf("corrupt agent version %q@%d: frozen identity does not match version key", agentID, version)
	}
	return &rec, nil
}

// ResolveAgentVersionTx reads and locks one exact immutable AgentRecord using
// the caller-owned transaction. It never resolves through the mutable head.
func (r *AgentRegistry) ResolveAgentVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, agentID string,
	dependencyVersion *int64,
) (*registry.AgentRecord, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(agentID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		agentID != strings.TrimSpace(agentID) ||
		dependencyVersion == nil ||
		*dependencyVersion < 1 ||
		*dependencyVersion > frozen.MaxJCSSafeInteger {
		return nil, registry.NewResolverError(registry.CodeDependencyVersionRequired, nil)
	}

	var data []byte
	err := tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, agentID, *dependencyVersion).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.NewResolverError(registry.CodeDependencyVersionRequired, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("read exact agent version: %w", err)
	}

	var rec registry.AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, err)
	}
	if rec.ID != agentID ||
		rec.WorkspaceID != workspaceID ||
		int64(rec.Version) != *dependencyVersion ||
		strings.TrimSpace(rec.Name) == "" {
		return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, nil)
	}
	return &rec, nil
}

// ResolveAgentHeadVersionsTx locks the exact agent head rows of one workspace
// and returns their current version numbers using the caller-owned
// transaction. Missing or deleted agents fail closed, so a baseline can never
// pin a ghost roster reference.
func (r *AgentRegistry) ResolveAgentHeadVersionsTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	agentIDs []string,
) (map[string]int64, error) {
	if tx == nil || strings.TrimSpace(workspaceID) == "" || len(agentIDs) == 0 {
		return nil, registry.NewResolverError(registry.CodeDependencyUnenumerable, nil)
	}
	unique := make([]string, 0, len(agentIDs))
	seen := make(map[string]struct{}, len(agentIDs))
	for _, agentID := range agentIDs {
		if strings.TrimSpace(agentID) == "" || agentID != strings.TrimSpace(agentID) {
			return nil, registry.NewResolverError(registry.CodeDependencyUnenumerable, nil)
		}
		if _, exists := seen[agentID]; exists {
			continue
		}
		seen[agentID] = struct{}{}
		unique = append(unique, agentID)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, version
		FROM weave_agents
		WHERE workspace_id=$1 AND id=ANY($2::text[]) AND deleted=false
		ORDER BY id COLLATE "C"
		FOR SHARE
	`, workspaceID, unique)
	if err != nil {
		return nil, fmt.Errorf("lock roster agent heads: %w", err)
	}
	defer rows.Close()

	versions := make(map[string]int64, len(unique))
	for rows.Next() {
		var agentID string
		var version int64
		if err := rows.Scan(&agentID, &version); err != nil {
			return nil, fmt.Errorf("scan roster agent head: %w", err)
		}
		if version < 1 {
			return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, nil)
		}
		versions[agentID] = version
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read roster agent heads: %w", err)
	}
	for _, agentID := range unique {
		if _, exists := versions[agentID]; !exists {
			return nil, fmt.Errorf(
				"roster agent %q not found in workspace %q",
				agentID,
				workspaceID,
			)
		}
	}
	return versions, nil
}

// ResolveAgentVersionContentTx locks and reads one exact immutable AgentRecord
// spec plus its canonical content hash using the caller-owned transaction. It
// never resolves through the mutable head and mirrors the identity validation
// of ResolveAgentVersionTx.
func (r *AgentRegistry) ResolveAgentVersionContentTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, agentID string,
	version int64,
) (*registry.AgentVersionContent, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(agentID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		agentID != strings.TrimSpace(agentID) ||
		version < 1 ||
		version > frozen.MaxJCSSafeInteger {
		return nil, registry.NewResolverError(registry.CodeDependencyVersionRequired, nil)
	}

	var data []byte
	err := tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, agentID, version).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.NewResolverError(registry.CodeDependencyVersionRequired, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("read exact agent version content: %w", err)
	}

	var rec registry.AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, err)
	}
	if rec.ID != agentID ||
		rec.WorkspaceID != workspaceID ||
		int64(rec.Version) != version ||
		strings.TrimSpace(rec.Name) == "" {
		return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, nil)
	}
	canonical, err := frozen.CanonicalizeJSON(data)
	if err != nil {
		return nil, registry.NewResolverError(registry.CodeFrozenManifestMismatch, err)
	}
	digest := sha256.Sum256(canonical)
	return &registry.AgentVersionContent{
		AgentID:     agentID,
		Name:        rec.Name,
		Version:     rec.Version,
		ContentHash: hex.EncodeToString(digest[:]),
		Engine:      rec.Engine,
		RuntimeID:   rec.RuntimeID,
		Model:       rec.Model,
	}, nil
}

// List returns all non-deleted agents for a tenant.
func (r *AgentRegistry) List(ctx context.Context, tenant string) ([]registry.AgentRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, team_id, owner_user_id, display_name, role, spec, version, visibility
		FROM weave_agents
		WHERE workspace_id=$1 AND deleted=false
		ORDER BY name
	`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []registry.AgentRecord
	for rows.Next() {
		var visibility string
		rec, err := scanAgentRecord(rows, tenant, &visibility)
		if err != nil {
			return nil, err
		}
		records = append(records, *rec)
	}
	return records, rows.Err()
}

// ListWorkspaces returns the distinct workspace IDs that hold live agents.
// Used by the startup upgrade scan to diagnose pre-workspace-scoping
// deployments; not part of the request path.
func (r *AgentRegistry) ListWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT workspace_id
		FROM weave_agents
		WHERE deleted=false
		ORDER BY workspace_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	return workspaces, rows.Err()
}

// Put creates or updates an agent, bumping the version.
func (r *AgentRegistry) Put(ctx context.Context, tenant string, rec *registry.AgentRecord) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := r.PutTx(ctx, tx, tenant, rec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutTx creates or updates an agent using the caller's transaction.
// It shares Put's insert, conflict, version, and ownership behavior, but leaves
// commit and rollback to the caller.
func (r *AgentRegistry) PutTx(ctx context.Context, tx pgx.Tx, tenant string, rec *registry.AgentRecord) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name)
		VALUES ($1, $1, $1)
		ON CONFLICT DO NOTHING
	`, tenant); err != nil {
		return err
	}
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}

	now := time.Now().Truncate(time.Microsecond)
	var id string
	var version int
	var createdAt time.Time
	var existingOwnerUserID *string
	var existingTeamID *string
	var existingRole string
	agentExists := true
	err := tx.QueryRow(ctx, `
		SELECT id, version, created_at, owner_user_id, team_id, role
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2
		FOR UPDATE
	`, tenant, rec.Name).Scan(
		&id, &version, &createdAt, &existingOwnerUserID, &existingTeamID, &existingRole,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		agentExists = false
		id = uuid.NewString()
		version = 1
		createdAt = now
	case err != nil:
		return err
	default:
		version++
	}
	if ownerUserIDRequiresValidation(agentExists, existingOwnerUserID, rec.OwnerUserID) {
		isMember, err := r.workspaceMemberExists(ctx, tx, tenant, *rec.OwnerUserID)
		if err != nil {
			return err
		}
		if !isMember {
			return fmt.Errorf("%w: user %q is not a member of workspace %q", registry.ErrOwnerNotWorkspaceMember, *rec.OwnerUserID, tenant)
		}
	}

	if rec.Role == "" {
		rec.Role = "worker"
	}

	if err := frozen.ValidateToolLoopControl(rec.ToolLoopControl); err != nil {
		return err
	}
	if rec.ToolLoopControl != nil && ((rec.Engine != "" && rec.Engine != "loom") || (rec.GraphType != "" && rec.GraphType != "standard") || len(rec.SubAgents) > 0 || len(rec.Spec.SubAgents) > 0 || len(rec.Permissions.Ask) > 0) {
		return fmt.Errorf("controlled tool loops require standard Loom serial leaves")
	}
	if err := registry.ValidateSkillRefs(rec); err != nil {
		return err
	}
	if rec.Visibility == "" {
		rec.Visibility = registry.VisibilityPublic
	}
	if !registry.ValidAgentVisibility(rec.Visibility) {
		return fmt.Errorf(
			"invalid agent visibility %q: must be %s, %s, or %s",
			rec.Visibility,
			registry.VisibilityPublic,
			registry.VisibilityInternalTool,
			registry.VisibilityPlatform,
		)
	}
	if agentExists {
		var referencedAsWorker, referencedAsLead bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM weave_team_workers
				WHERE workspace_id=$1 AND worker_agent_id=$2
			), EXISTS (
				SELECT 1 FROM weave_teams
				WHERE workspace_id=$1 AND lead_avatar_id=$2
			)
		`, tenant, id).Scan(&referencedAsWorker, &referencedAsLead); err != nil {
			return err
		}
		if rec.Role != existingRole && (referencedAsWorker || referencedAsLead || existingTeamID != nil) {
			return fmt.Errorf("%w: agent %q", registry.ErrAgentRoleReferencedByTeam, rec.Name)
		}
		if referencedAsWorker {
			if err := registry.ValidateTeamWorkerAgentRecord(rec); err != nil {
				return err
			}
		}
	}

	rec.TeamID = ""
	if existingTeamID != nil {
		rec.TeamID = *existingTeamID
	}
	rec.ID = id
	rec.WorkspaceID = tenant
	rec.Version = version
	rec.CreatedAt = createdAt
	rec.UpdatedAt = now
	rec.Deleted = false

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if agentExists {
		tag, err := tx.Exec(ctx, `
			UPDATE weave_agents
			SET owner_user_id=$3,
				display_name=$4,
				role=$5,
				visibility=$6,
				spec=$7::jsonb,
				version=$8,
				updated_at=$9,
				deleted=false
			WHERE id=$1 AND workspace_id=$2 AND name=$10
		`, id, tenant, rec.OwnerUserID, rec.DisplayName, rec.Role, rec.Visibility, string(data), version, now, rec.Name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("update agent %q: locked identity changed", rec.Name)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_agents (
				id, workspace_id, team_id, owner_user_id, name, display_name, role,
				visibility, spec, version, deleted, created_at, updated_at
			)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9::jsonb, $10, false, $11, $12)
		`, id, tenant, rec.TeamID, rec.OwnerUserID, rec.Name, rec.DisplayName, rec.Role, rec.Visibility, string(data), version, createdAt, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agent_versions (workspace_id, agent_id, version, spec)
		VALUES ($1, $2, $3, $4::jsonb)
	`, tenant, id, version, string(data)); err != nil {
		return err
	}
	return nil
}

// IsWorkspaceMember reports whether a live user belongs to the workspace.
// The join checks both the membership row and the user's tenant boundary.
func (r *AgentRegistry) IsWorkspaceMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	return r.workspaceMemberExists(ctx, r.pool, workspaceID, userID)
}

func ownerUserIDRequiresValidation(agentExists bool, existingOwnerUserID, newOwnerUserID *string) bool {
	if newOwnerUserID == nil {
		return false
	}
	if !agentExists || existingOwnerUserID == nil {
		return true
	}
	return *existingOwnerUserID != *newOwnerUserID
}

// Delete soft-deletes an agent after excluding TeamWorker references.
func (r *AgentRegistry) Delete(ctx context.Context, tenant, name string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT id
		FROM weave_teams
		WHERE workspace_id=$1
		ORDER BY id
		FOR UPDATE
	`, tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var agentID string
	var legacyTeamID *string
	err = tx.QueryRow(ctx, `
		SELECT id, team_id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2
		FOR UPDATE
	`, tenant, name).Scan(&agentID, &legacyTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	if err != nil {
		return err
	}

	var referenced bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_team_workers
			WHERE workspace_id=$1 AND worker_agent_id=$2
		)
	`, tenant, agentID).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: agent %q", registry.ErrAgentReferencedByTeamWorker, name)
	}
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_teams
			WHERE workspace_id=$1 AND lead_avatar_id=$2
		)
	`, tenant, agentID).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: agent %q", registry.ErrAgentReferencedByTeamLead, name)
	}
	if legacyTeamID != nil {
		return fmt.Errorf("%w: agent %q team %q", registry.ErrAgentReferencedByTeamContext, name, *legacyTeamID)
	}

	result, err := tx.Exec(ctx, `
		UPDATE weave_agents
		SET deleted=true, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, tenant, agentID)
	if err != nil {
		return err
	}
	affected := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	return tx.Commit(ctx)
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanAgentRecord overlays the authoritative head-row projection onto the spec
// JSON. Get/List pass the optional authoritative visibility-column destination;
// wrappers such as managedAgentRow may continue appending their own tail fields.
func scanAgentRecord(row rowScanner, tenant string, visibilityColumn ...*string) (*registry.AgentRecord, error) {
	if len(visibilityColumn) > 1 || (len(visibilityColumn) == 1 && visibilityColumn[0] == nil) {
		return nil, fmt.Errorf("scan agent record: expected at most one non-nil visibility destination")
	}
	var (
		id          string
		name        string
		teamID      *string
		ownerUserID *string
		displayName string
		role        string
		data        []byte
		version     int
	)
	dest := []any{&id, &name, &teamID, &ownerUserID, &displayName, &role, &data, &version}
	if len(visibilityColumn) == 1 {
		dest = append(dest, visibilityColumn[0])
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	var rec registry.AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("corrupt agent record %q: %w", name, err)
	}
	rec.ID = id
	rec.WorkspaceID = tenant
	rec.TeamID = ""
	if teamID != nil {
		rec.TeamID = *teamID
	}
	rec.OwnerUserID = ownerUserID
	rec.Name = name
	rec.DisplayName = displayName
	rec.Role = role
	if len(visibilityColumn) == 1 {
		rec.Visibility = *visibilityColumn[0]
	}

	if rec.Role == "peer" {
		rec.Role = "worker"
	}
	rec.Version = version
	rec.Deleted = false
	return &rec, nil
}
