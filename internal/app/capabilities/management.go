package capabilities

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PublishableWorkflow is an exact published workflow version that an
// administrator can evaluate for a new capability release.
type PublishableWorkflow struct {
	WorkflowID      string `json:"workflow_id"`
	WorkflowName    string `json:"workflow_name"`
	WorkflowVersion int    `json:"workflow_version"`
	TeamID          string `json:"team_id"`
	TeamName        string `json:"team_name"`
}

// ManagementSnapshot is the workspace-scoped state needed by the Workbench
// capability-management page. Credential hashes and invocation inputs never
// cross this read model.
type ManagementSnapshot struct {
	Apps         []ServiceApp          `json:"apps"`
	Credentials  []Credential          `json:"credentials"`
	Capabilities []Capability          `json:"capabilities"`
	Releases     []Release             `json:"releases"`
	Grants       []Grant               `json:"grants"`
	Invocations  []InvocationStatus    `json:"invocations"`
	Workflows    []PublishableWorkflow `json:"publishable_workflows"`
}

func (store *Store) ManagementSnapshot(
	ctx context.Context,
	workspaceID string,
	invocations *InvocationService,
) (ManagementSnapshot, error) {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) || invocations == nil {
		return ManagementSnapshot{}, ErrInvalid
	}
	snapshot := ManagementSnapshot{
		Apps: []ServiceApp{}, Credentials: []Credential{}, Capabilities: []Capability{},
		Releases: []Release{}, Grants: []Grant{}, Invocations: []InvocationStatus{},
		Workflows: []PublishableWorkflow{},
	}
	rows, err := store.pool.Query(ctx, `
		SELECT workspace_id,id,name,description,enabled,max_concurrent_invocations,
		       created_by,created_at,updated_at
		FROM weave_service_apps WHERE workspace_id=$1 ORDER BY created_at,id
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list service apps: %w", err)
	}
	for rows.Next() {
		var item ServiceApp
		if err := rows.Scan(&item.WorkspaceID, &item.ID, &item.Name, &item.Description,
			&item.Enabled, &item.MaxConcurrentInvocations, &item.CreatedBy,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan service app: %w", err)
		}
		snapshot.Apps = append(snapshot.Apps, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read service apps: %w", err)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT workspace_id,id,app_id,name,scopes,expires_at,revoked_at,last_used_at,
		       created_by,created_at
		FROM weave_service_app_credentials WHERE workspace_id=$1 ORDER BY created_at,id
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list service app credentials: %w", err)
	}
	for rows.Next() {
		var item Credential
		if err := rows.Scan(&item.WorkspaceID, &item.ID, &item.AppID, &item.Name,
			&item.Scopes, &item.ExpiresAt, &item.RevokedAt, &item.LastUsedAt,
			&item.CreatedBy, &item.CreatedAt); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan service app credential: %w", err)
		}
		snapshot.Credentials = append(snapshot.Credentials, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read service app credentials: %w", err)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT workspace_id,id,key,name,description,enabled,created_by,created_at,updated_at
		FROM weave_capabilities WHERE workspace_id=$1 ORDER BY created_at,id
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list capabilities: %w", err)
	}
	for rows.Next() {
		var item Capability
		if err := rows.Scan(&item.WorkspaceID, &item.ID, &item.Key, &item.Name,
			&item.Description, &item.Enabled, &item.CreatedBy, &item.CreatedAt,
			&item.UpdatedAt); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan capability: %w", err)
		}
		snapshot.Capabilities = append(snapshot.Capabilities, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read capabilities: %w", err)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT release.workspace_id,release.capability_id,release.version,release.id,
		       release.workflow_id,release.workflow_version,release.artifact_content_hash,
		       release.contract_dialect,release.normalization,release.input_contract,
		       release.output_contract,release.input_mapping,release.execution_limits,
		       release.result_policy,control.enabled,release.created_by,release.created_at
		FROM weave_capability_releases AS release
		JOIN weave_capability_release_controls AS control
		  ON control.workspace_id=release.workspace_id
		 AND control.capability_id=release.capability_id AND control.version=release.version
		WHERE release.workspace_id=$1
		ORDER BY release.capability_id,release.version DESC
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list capability releases: %w", err)
	}
	for rows.Next() {
		var item Release
		var limitsJSON, policyJSON json.RawMessage
		if err := rows.Scan(&item.WorkspaceID, &item.CapabilityID, &item.Version, &item.ID,
			&item.WorkflowID, &item.WorkflowVersion, &item.ArtifactContentHash,
			&item.ContractDialect, &item.Normalization, &item.InputContract,
			&item.OutputContract, &item.InputMapping, &limitsJSON, &policyJSON,
			&item.Enabled, &item.CreatedBy, &item.CreatedAt); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan capability release: %w", err)
		}
		if decodeExactJSON(limitsJSON, &item.ExecutionLimits) != nil ||
			decodeExactJSON(policyJSON, &item.ResultPolicy) != nil {
			rows.Close()
			return ManagementSnapshot{}, ErrReleaseUnsafe
		}
		snapshot.Releases = append(snapshot.Releases, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read capability releases: %w", err)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT workspace_id,id,app_id,capability_id,release_version,max_concurrent,
		       granted_by,granted_at,revoked_by,revoked_at
		FROM weave_service_app_release_grants WHERE workspace_id=$1
		ORDER BY granted_at,id
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list capability grants: %w", err)
	}
	for rows.Next() {
		var item Grant
		if err := rows.Scan(&item.WorkspaceID, &item.ID, &item.AppID, &item.CapabilityID,
			&item.ReleaseVersion, &item.MaxConcurrent, &item.GrantedBy, &item.GrantedAt,
			&item.RevokedBy, &item.RevokedAt); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan capability grant: %w", err)
		}
		snapshot.Grants = append(snapshot.Grants, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read capability grants: %w", err)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT invocation_id,app_id FROM weave_capability_invocations
		WHERE workspace_id=$1 ORDER BY accepted_at DESC,invocation_id LIMIT 100
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list capability invocations: %w", err)
	}
	type invocationIdentity struct{ invocationID, appID string }
	identities := make([]invocationIdentity, 0, 100)
	for rows.Next() {
		var item invocationIdentity
		if err := rows.Scan(&item.invocationID, &item.appID); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan capability invocation identity: %w", err)
		}
		identities = append(identities, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read capability invocation identities: %w", err)
	}
	for _, identity := range identities {
		status, err := invocations.getInvocationStatus(ctx, workspaceID, identity.appID, identity.invocationID)
		if err != nil {
			return ManagementSnapshot{}, err
		}
		snapshot.Invocations = append(snapshot.Invocations, status)
	}

	rows, err = store.pool.Query(ctx, `
		SELECT workflow.id,workflow.name,version.version,team.id,team.name
		FROM weave_team_workflows AS workflow
		JOIN weave_team_workflow_versions AS version
		  ON version.workspace_id=workflow.workspace_id AND version.workflow_id=workflow.id
		JOIN weave_published_artifact_contents AS artifact
		  ON artifact.workspace_id=version.workspace_id
		 AND artifact.workflow_id=version.workflow_id AND artifact.workflow_version=version.version
		JOIN weave_workflow_version_admission_statuses AS admission
		  ON admission.workspace_id=version.workspace_id
		 AND admission.workflow_id=version.workflow_id AND admission.workflow_version=version.version
		JOIN weave_teams AS team
		  ON team.workspace_id=workflow.workspace_id AND team.id=workflow.team_id
		WHERE workflow.workspace_id=$1 AND workflow.status='active'
		  AND version.status='published' AND admission.blocked=false
		ORDER BY team.name,workflow.name,version.version DESC
	`, workspaceID)
	if err != nil {
		return ManagementSnapshot{}, fmt.Errorf("list publishable workflows: %w", err)
	}
	for rows.Next() {
		var item PublishableWorkflow
		if err := rows.Scan(&item.WorkflowID, &item.WorkflowName, &item.WorkflowVersion,
			&item.TeamID, &item.TeamName); err != nil {
			rows.Close()
			return ManagementSnapshot{}, fmt.Errorf("scan publishable workflow: %w", err)
		}
		snapshot.Workflows = append(snapshot.Workflows, item)
	}
	if err := closeRows(rows); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("read publishable workflows: %w", err)
	}
	return snapshot, nil
}

func closeRows(rows pgx.Rows) error {
	rows.Close()
	return rows.Err()
}

func (store *Store) SetCapabilityEnabled(
	ctx context.Context, workspaceID, capabilityID, updatedBy string, enabled bool,
) error {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) ||
		invalidIdentity(capabilityID) || invalidIdentity(updatedBy) {
		return ErrInvalid
	}
	tag, err := store.pool.Exec(ctx, `
		UPDATE weave_capabilities AS capability SET enabled=$4,updated_at=$5
		FROM weave_users AS admin
		WHERE capability.workspace_id=$1 AND capability.id=$2
		  AND admin.tenant_id=$1 AND admin.id=$3
		  AND admin.disabled=false AND admin.role='admin'
	`, workspaceID, capabilityID, updatedBy, enabled, store.clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("set capability enabled: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) SetReleaseEnabled(
	ctx context.Context, workspaceID, capabilityID string, version int,
	updatedBy string, enabled bool,
) error {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) ||
		invalidIdentity(capabilityID) || version < 1 || invalidIdentity(updatedBy) {
		return ErrInvalid
	}
	tag, err := store.pool.Exec(ctx, `
		UPDATE weave_capability_release_controls AS control
		SET enabled=$5,updated_by=admin.id,updated_at=$6
		FROM weave_users AS admin
		WHERE control.workspace_id=$1 AND control.capability_id=$2 AND control.version=$3
		  AND admin.tenant_id=$1 AND admin.id=$4
		  AND admin.disabled=false AND admin.role='admin'
	`, workspaceID, capabilityID, version, updatedBy, enabled, store.clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("set capability release enabled: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
