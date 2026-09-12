package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const (
	hardMaxInputBytes   = 1 << 20
	hardMaxOutputBytes  = 1 << 20
	hardMaxNestingDepth = 64
	hardMaxObjectFields = 4096
	hardMaxArrayItems   = 10000
	hardMaxStringBytes  = 1 << 18
	hardMaxTimeout      = 24 * 60 * 60
	hardMaxOutputTokens = 65536
)

var resultFieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type PublishReleaseRequest struct {
	WorkspaceID     string
	CapabilityID    string
	Version         int
	WorkflowID      string
	WorkflowVersion int
	ExecutionLimits ExecutionLimits
	ResultPolicy    ResultPolicy
	CreatedBy       string
}

func (store *Store) PublishRelease(ctx context.Context, request PublishReleaseRequest) (Release, error) {
	if store == nil || store.pool == nil || invalidIdentity(request.WorkspaceID) ||
		invalidIdentity(request.CapabilityID) || request.Version < 1 ||
		invalidIdentity(request.WorkflowID) || request.WorkflowVersion < 1 ||
		invalidIdentity(request.CreatedBy) {
		return Release{}, ErrInvalid
	}
	if err := validateExecutionLimits(request.ExecutionLimits); err != nil {
		return Release{}, err
	}
	policy, err := normalizeResultPolicy(request.ResultPolicy)
	if err != nil {
		return Release{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Release{}, fmt.Errorf("begin capability release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var capabilityEnabled bool
	var adminID string
	err = tx.QueryRow(ctx, `
		SELECT capability.enabled,admin.id
		FROM weave_capabilities AS capability
		JOIN weave_users AS admin ON admin.tenant_id=capability.workspace_id
		WHERE capability.workspace_id=$1 AND capability.id=$2
		  AND admin.id=$3 AND admin.disabled=false AND admin.role='admin'
		FOR UPDATE OF capability
	`, request.WorkspaceID, request.CapabilityID, request.CreatedBy).Scan(
		&capabilityEnabled, &adminID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("lock capability for release: %w", err)
	}
	if !capabilityEnabled {
		return Release{}, ErrDisabled
	}
	var nextVersion int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version),0)+1
		FROM weave_capability_releases
		WHERE workspace_id=$1 AND capability_id=$2
	`, request.WorkspaceID, request.CapabilityID).Scan(&nextVersion); err != nil {
		return Release{}, fmt.Errorf("read next capability release version: %w", err)
	}
	if request.Version != nextVersion {
		return Release{}, fmt.Errorf("%w: next version is %d", ErrReleaseConflict, nextVersion)
	}

	var artifact frozen.ArtifactEnvelopeV1
	var workflowStatus, versionStatus string
	var admissionBlocked bool
	err = tx.QueryRow(ctx, `
		SELECT artifact.workspace_id,artifact.workflow_id,artifact.workflow_version,
		       artifact.artifact_schema_version,artifact.canonicalization_algorithm,
		       artifact.canonicalization_version,artifact.hash_algorithm,
		       artifact.content_hash,artifact.payload,
		       workflow.status,version.status,admission.blocked
		FROM weave_published_artifact_contents AS artifact
		JOIN weave_team_workflows AS workflow
		  ON workflow.workspace_id=artifact.workspace_id AND workflow.id=artifact.workflow_id
		JOIN weave_team_workflow_versions AS version
		  ON version.workspace_id=artifact.workspace_id
		 AND version.workflow_id=artifact.workflow_id
		 AND version.version=artifact.workflow_version
		JOIN weave_workflow_version_admission_statuses AS admission
		  ON admission.workspace_id=artifact.workspace_id
		 AND admission.workflow_id=artifact.workflow_id
		 AND admission.workflow_version=artifact.workflow_version
		WHERE artifact.workspace_id=$1 AND artifact.workflow_id=$2
		  AND artifact.workflow_version=$3
		FOR SHARE OF workflow,version,admission
	`, request.WorkspaceID, request.WorkflowID, request.WorkflowVersion).Scan(
		&artifact.WorkspaceID, &artifact.WorkflowID, &artifact.WorkflowVersion,
		&artifact.ArtifactSchemaVersion, &artifact.CanonicalizationAlgorithm,
		&artifact.CanonicalizationVersion, &artifact.HashAlgorithm,
		&artifact.ContentHash, &artifact.Payload,
		&workflowStatus, &versionStatus, &admissionBlocked,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, fmt.Errorf("%w: published workflow artifact", ErrNotFound)
	}
	if err != nil {
		return Release{}, fmt.Errorf("read published workflow artifact: %w", err)
	}
	if workflowStatus != "active" || versionStatus != "published" || admissionBlocked {
		return Release{}, ErrDisabled
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(artifact)
	if err != nil {
		return Release{}, fmt.Errorf("%w: invalid published artifact: %v", ErrReleaseUnsafe, err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return Release{}, fmt.Errorf("%w: invalid workflow graph", ErrReleaseUnsafe)
	}
	if err := validateNoToolRelease(payload, graph); err != nil {
		return Release{}, err
	}
	if err := validateResultFields(graph.OutputContract, policy.ExposedFields); err != nil {
		return Release{}, err
	}
	inputContract, err := json.Marshal(graph.InputContract)
	if err != nil {
		return Release{}, fmt.Errorf("encode capability input contract: %w", err)
	}
	outputContract, err := json.Marshal(graph.OutputContract)
	if err != nil {
		return Release{}, fmt.Errorf("encode capability output contract: %w", err)
	}
	limitsJSON, _ := json.Marshal(request.ExecutionLimits)
	policyJSON, _ := json.Marshal(policy)
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	release := Release{
		WorkspaceID: request.WorkspaceID, CapabilityID: request.CapabilityID,
		Version: request.Version, ID: "release-" + uuid.NewString(),
		WorkflowID: request.WorkflowID, WorkflowVersion: request.WorkflowVersion,
		ArtifactContentHash: artifact.ContentHash,
		ContractDialect:     ContractDialectV1, Normalization: NormalizationV1,
		InputContract: inputContract, OutputContract: outputContract,
		InputMapping:    json.RawMessage(InputMappingV1),
		ExecutionLimits: request.ExecutionLimits, ResultPolicy: policy,
		Enabled: true, CreatedBy: adminID, CreatedAt: now,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_capability_releases(
			workspace_id,capability_id,version,id,workflow_id,workflow_version,
			artifact_content_hash,contract_dialect,normalization,input_contract,
			output_contract,input_mapping,execution_limits,result_policy,created_by,created_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,$14::jsonb,$15,$16)
	`, release.WorkspaceID, release.CapabilityID, release.Version, release.ID,
		release.WorkflowID, release.WorkflowVersion, release.ArtifactContentHash,
		release.ContractDialect, release.Normalization, string(release.InputContract),
		string(release.OutputContract), string(release.InputMapping), string(limitsJSON), string(policyJSON),
		release.CreatedBy, release.CreatedAt); err != nil {
		return Release{}, fmt.Errorf("insert capability release: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_capability_release_controls(
			workspace_id,capability_id,version,enabled,updated_by,updated_at
		) VALUES($1,$2,$3,true,$4,$5)
	`, release.WorkspaceID, release.CapabilityID, release.Version,
		release.CreatedBy, release.CreatedAt); err != nil {
		return Release{}, fmt.Errorf("insert capability release control: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Release{}, fmt.Errorf("commit capability release: %w", err)
	}
	return release, nil
}

func validateExecutionLimits(limits ExecutionLimits) error {
	valid := limits.MaxInputBytes >= 2 && limits.MaxInputBytes <= hardMaxInputBytes &&
		limits.MaxOutputBytes >= 2 && limits.MaxOutputBytes <= hardMaxOutputBytes &&
		limits.MaxNestingDepth >= 1 && limits.MaxNestingDepth <= hardMaxNestingDepth &&
		limits.MaxObjectFields >= 1 && limits.MaxObjectFields <= hardMaxObjectFields &&
		limits.MaxArrayItems >= 1 && limits.MaxArrayItems <= hardMaxArrayItems &&
		limits.MaxStringBytes >= 1 && limits.MaxStringBytes <= hardMaxStringBytes &&
		limits.QueueTimeoutSeconds >= 1 && limits.QueueTimeoutSeconds <= hardMaxTimeout &&
		limits.ExecutionTimeoutSeconds >= 1 && limits.ExecutionTimeoutSeconds <= hardMaxTimeout &&
		limits.MaxOutputTokens >= 1 && limits.MaxOutputTokens <= hardMaxOutputTokens
	if !valid {
		return fmt.Errorf("%w: execution limits are outside platform bounds", ErrInvalid)
	}
	return nil
}

func normalizeResultPolicy(policy ResultPolicy) (ResultPolicy, error) {
	if len(policy.ExposedFields) == 0 || len(policy.ExposedFields) > 64 {
		return ResultPolicy{}, fmt.Errorf("%w: result fields are required", ErrInvalid)
	}
	result := policy
	result.ExposedFields = append([]string(nil), policy.ExposedFields...)
	sort.Strings(result.ExposedFields)
	for index, field := range result.ExposedFields {
		if !resultFieldPattern.MatchString(field) || index > 0 && result.ExposedFields[index-1] == field {
			return ResultPolicy{}, fmt.Errorf("%w: result fields are invalid", ErrInvalid)
		}
	}
	return result, nil
}

func validateNoToolRelease(payload frozen.ArtifactPayloadV1, graph machine.GraphDefinition) error {
	if graph.InputContract.Type != machine.ValueJSON || len(graph.InputContract.Schema) == 0 ||
		graph.OutputContract.Type != machine.ValueJSON || len(graph.OutputContract.Schema) == 0 {
		return fmt.Errorf("%w: service input and output must use explicit JSON contracts", ErrReleaseUnsafe)
	}
	for _, node := range graph.Nodes {
		if node.Type == machine.NodeWait || node.Type == machine.NodeHandoff {
			return fmt.Errorf("%w: interactive workflow node %q", ErrReleaseUnsafe, node.Type)
		}
	}
	if len(payload.Bundles) == 0 || len(payload.DeliveryTargets) != 0 {
		return fmt.Errorf("%w: release requires local delivery and frozen workers", ErrReleaseUnsafe)
	}
	for _, bundle := range payload.Bundles {
		agent := bundle.Agent
		if agent.Engine != "codex" || agent.RuntimeID == "" ||
			!containsString(agent.Permissions.Deny, "*") ||
			len(agent.Permissions.Allow) != 0 || len(agent.Permissions.Ask) != 0 ||
			len(bundle.MCPBindings) != 0 || len(bundle.Skills) != 0 ||
			bundle.Capability.MayInvokeAgent || len(bundle.Capability.InteractiveStepIDs) != 0 ||
			len(bundle.Capability.InteractiveToolIDs) != 0 ||
			(agent.MemoryConfig != nil && (agent.MemoryConfig.Enabled || agent.MemoryConfig.AutoRemember)) {
			return fmt.Errorf("%w: every frozen worker must use native Codex with deny-all tools and no interactive dependencies", ErrReleaseUnsafe)
		}
	}
	return nil
}

func validateResultFields(contract machine.OutputContract, fields []string) error {
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(contract.Schema, &schema); err != nil ||
		schema.Type != "object" || len(schema.Properties) == 0 {
		return fmt.Errorf("%w: result policy requires an object output schema", ErrReleaseUnsafe)
	}
	for _, field := range fields {
		if _, ok := schema.Properties[field]; !ok {
			return fmt.Errorf("%w: exposed result field %q is not in the output contract", ErrInvalid, field)
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type GrantReleaseRequest struct {
	WorkspaceID    string
	AppID          string
	CapabilityID   string
	ReleaseVersion int
	MaxConcurrent  int
	GrantedBy      string
}

func (store *Store) GrantRelease(ctx context.Context, request GrantReleaseRequest) (Grant, error) {
	if store == nil || store.pool == nil || invalidIdentity(request.WorkspaceID) ||
		invalidIdentity(request.AppID) || invalidIdentity(request.CapabilityID) ||
		request.ReleaseVersion < 1 || request.MaxConcurrent < 1 ||
		request.MaxConcurrent > 1024 || invalidIdentity(request.GrantedBy) {
		return Grant{}, ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Grant{}, fmt.Errorf("begin capability grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var appLimit int
	err = tx.QueryRow(ctx, `
		SELECT app.max_concurrent_invocations
		FROM weave_service_apps AS app
		JOIN weave_capabilities AS capability
		  ON capability.workspace_id=app.workspace_id AND capability.id=$3
		JOIN weave_capability_release_controls AS control
		  ON control.workspace_id=capability.workspace_id
		 AND control.capability_id=capability.id AND control.version=$4
		JOIN weave_users AS admin ON admin.tenant_id=app.workspace_id
		WHERE app.workspace_id=$1 AND app.id=$2 AND app.enabled=true
		  AND capability.enabled=true AND control.enabled=true
		  AND admin.id=$5 AND admin.disabled=false AND admin.role='admin'
		FOR SHARE OF app,capability,control
	`, request.WorkspaceID, request.AppID, request.CapabilityID,
		request.ReleaseVersion, request.GrantedBy).Scan(&appLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	if err != nil {
		return Grant{}, fmt.Errorf("validate capability grant: %w", err)
	}
	if request.MaxConcurrent > appLimit {
		return Grant{}, fmt.Errorf("%w: grant concurrency exceeds app limit", ErrInvalid)
	}
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	grant := Grant{
		WorkspaceID: request.WorkspaceID, ID: "grant-" + uuid.NewString(),
		AppID: request.AppID, CapabilityID: request.CapabilityID,
		ReleaseVersion: request.ReleaseVersion, MaxConcurrent: request.MaxConcurrent,
		GrantedBy: request.GrantedBy, GrantedAt: now,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_service_app_release_grants(
			workspace_id,id,app_id,capability_id,release_version,max_concurrent,
			granted_by,granted_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
	`, grant.WorkspaceID, grant.ID, grant.AppID, grant.CapabilityID,
		grant.ReleaseVersion, grant.MaxConcurrent, grant.GrantedBy, grant.GrantedAt); err != nil {
		if strings.Contains(err.Error(), "weave_service_app_release_grants_active_key") {
			return Grant{}, ErrGrantConflict
		}
		return Grant{}, fmt.Errorf("insert capability grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, fmt.Errorf("commit capability grant: %w", err)
	}
	return grant, nil
}

func (store *Store) RevokeGrant(ctx context.Context, workspaceID, grantID, revokedBy string) error {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) ||
		invalidIdentity(grantID) || invalidIdentity(revokedBy) {
		return ErrInvalid
	}
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	tag, err := store.pool.Exec(ctx, `
		UPDATE weave_service_app_release_grants AS binding
		SET revoked_by=admin.id,revoked_at=$4
		FROM weave_users AS admin
		WHERE binding.workspace_id=$1 AND binding.id=$2 AND binding.revoked_at IS NULL
		  AND admin.tenant_id=$1 AND admin.id=$3
		  AND admin.disabled=false AND admin.role='admin'
	`, workspaceID, grantID, revokedBy, now)
	if err != nil {
		return fmt.Errorf("revoke capability grant: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrGrantUnavailable
	}
	return nil
}
