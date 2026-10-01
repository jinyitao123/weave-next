package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

const forgeDelegationHeader = "X-Weave-Forge-Authorization"

type preparedBusinessDelegation struct {
	identity     ExternalIdentity
	forgeBaseURL string
	ciphertext   string
	digest       string
	actions      []string
	resources    []dispatchInputResource
	record       *dispatchBusinessRecord
	expiresAt    time.Time
	issuedAt     time.Time
	grantID      string
	generation   int64
	scopeSHA     string
}

type businessDelegationPreparationError struct {
	status  int
	code    string
	message string
	cause   error
}

func (e *businessDelegationPreparationError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.message
}

func businessDelegationRejected(status int, code, message string) *businessDelegationPreparationError {
	return &businessDelegationPreparationError{status: status, code: code, message: message}
}

func businessDelegationStoreFailure(err error) *businessDelegationPreparationError {
	return &businessDelegationPreparationError{
		status: http.StatusInternalServerError, code: "workflow_store_failed", message: "workflow store failed", cause: err,
	}
}

func publishedBusinessActions(payload frozen.ArtifactPayloadV1) []string {
	set := map[string]struct{}{}
	for _, bundle := range payload.Bundles {
		for _, capabilityID := range bundle.Agent.BusinessCapabilityIDs {
			if strings.HasPrefix(capabilityID, "forge:action:") {
				set[capabilityID] = struct{}{}
			}
		}
	}
	actions := make([]string, 0, len(set))
	for capabilityID := range set {
		actions = append(actions, capabilityID)
	}
	sort.Strings(actions)
	return actions
}

func (s *Server) publishedBusinessActions(ctx context.Context, workspaceID, workflowID string, version int) ([]string, error) {
	if s.WorkflowArtifacts == nil || workflowID == "" || version < 1 {
		return nil, nil
	}
	artifact, err := s.WorkflowArtifacts.GetArtifact(ctx, workspaceID, workflowID, version)
	if err != nil {
		return nil, err
	}
	payload, err := frozen.DecodeArtifactPayloadV1(artifact.Payload)
	if err != nil {
		return nil, err
	}
	return publishedBusinessActions(payload), nil
}

func loadPublishedBusinessActionsTx(ctx context.Context, tx pgx.Tx, workspaceID, workflowID string, version int) ([]string, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM weave_published_artifact_contents
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3`, workspaceID, workflowID, version).Scan(&raw); err != nil {
		return nil, err
	}
	payload, err := frozen.DecodeArtifactPayloadV1(raw)
	if err != nil {
		return nil, err
	}
	return publishedBusinessActions(payload), nil
}

func (s *Server) prepareBusinessDelegation(ctx context.Context, workspaceID, userID, authorization, inputRevisionID, registrationID, taskSHA, workflowID string, version int, actions []string, resources []dispatchInputResource, record *dispatchBusinessRecord) (*preparedBusinessDelegation, *businessDelegationPreparationError) {
	if len(actions) == 0 && len(resources) == 0 && record == nil {
		return nil, nil
	}
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) <= 7 || len(authorization) > 64<<10 {
		return nil, businessDelegationRejected(401, "business_delegation_required", "Forge task token is required")
	}
	origin, err := s.forgeTaskIssuer()
	if err != nil || s.GetPool() == nil {
		return nil, businessDelegationRejected(503, "business_delegation_unavailable", "Forge task authority is unavailable")
	}
	token := []byte(strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")))
	defer clear(token)
	grant, err := businessaction.ReadTaskDelegationGrant(ctx, origin, token)
	if errors.Is(err, businessaction.ErrDelegationExpired) {
		return nil, businessDelegationRejected(401, "business_delegation_expired", "This work authorization expired; renew the original input")
	}
	if err != nil {
		return nil, businessDelegationRejected(401, "business_delegation_invalid", "Forge task token could not be verified")
	}
	var boundUser, nativeOrg string
	err = s.GetPool().QueryRow(ctx, `SELECT user_id,native_organization FROM weave_external_identities WHERE issuer=$1 AND subject=$2 AND workspace_id=$3`, grant.IdentityIssuer, grant.Subject.ID, workspaceID).Scan(&boundUser, &nativeOrg)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (boundUser != userID || nativeOrg == "" || nativeOrg != grant.Subject.OrganizationID) {
		return nil, businessDelegationRejected(403, "business_delegation_identity_mismatch", "Task authorization does not match the original employee and native organization")
	}
	if err != nil {
		return nil, businessDelegationStoreFailure(err)
	}
	expected := businessaction.TaskDelegationScope{InputRevisionID: inputRevisionID, RegistrationID: registrationID, TaskSHA256: taskSHA, WorkflowID: workflowID, WorkflowVersion: version, AllowedActions: actions, Resources: taskScopeResources(resources)}
	if record != nil {
		expected.BusinessRecord = &businessaction.TaskBusinessRecord{ObjectName: record.ObjectName, RecordID: record.RecordID}
	}
	if !businessaction.TaskScopeMatches(grant.Scope, expected) {
		return nil, businessDelegationRejected(403, "business_delegation_scope_mismatch", "Task authorization differs from the frozen work scope")
	}
	if err := verifyTaskGrantFiles(ctx, grant.Issuer, token, resources); err != nil {
		return nil, businessDelegationRejected(422, "business_resource_invalid", "Frozen task materials could not be verified")
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return nil, businessDelegationRejected(503, "business_delegation_unavailable", "Task authorization encryption is unavailable")
	}
	ciphertext, err := secret.Seal(key, token)
	clear(key)
	if err != nil {
		return nil, businessDelegationStoreFailure(err)
	}
	digest := sha256.Sum256(token)
	return &preparedBusinessDelegation{identity: ExternalIdentity{Issuer: grant.IdentityIssuer, BaseURL: grant.Issuer, Subject: grant.Subject.ID, Organization: workspaceID, NativeOrganization: nativeOrg}, forgeBaseURL: grant.Issuer, ciphertext: ciphertext, digest: hex.EncodeToString(digest[:]), actions: actions, resources: resources, record: record, expiresAt: grant.ExpiresAt, issuedAt: grant.IssuedAt, grantID: grant.GrantID, generation: grant.Generation, scopeSHA: grant.ScopeSHA256}, nil
}

func (s *Server) forgeTaskIssuer() (string, error) {
	if s.Config == nil {
		return "", errors.New("Forge origin is not configured")
	}
	origin, err := url.Parse(s.Config.ForgeSessionURL)
	if err != nil || origin.Host == "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return "", errors.New("Forge origin is not configured")
	}
	origin.Path, origin.RawPath, origin.RawQuery, origin.Fragment = "", "", "", ""
	return origin.String(), nil
}
func taskScopeResources(resources []dispatchInputResource) []businessaction.TaskDelegationResource {
	result := make([]businessaction.TaskDelegationResource, 0, len(resources))
	for _, r := range resources {
		result = append(result, businessaction.TaskDelegationResource{Type: r.Type, SourceKind: r.SourceKind, RequestID: r.RequestID, MaterialID: r.MaterialID, ID: r.ID, Name: r.Name, MediaType: r.MediaType, Bytes: r.Bytes, SHA256: r.SHA256})
	}
	return result
}
func verifyTaskGrantFiles(ctx context.Context, issuer string, token []byte, resources []dispatchInputResource) error {
	for _, resource := range resources {
		if err := businessaction.ReadVerifiedTaskFile(ctx, issuer, token, businessaction.TaskDelegationResource{Type: resource.Type, ID: resource.ID, Bytes: resource.Bytes, SHA256: resource.SHA256}); err != nil {
			return err
		}
	}
	return nil
}

func verifyForgeFiles(ctx context.Context, issuer, bearer string, resources []dispatchInputResource) error {
	base, err := url.Parse(issuer)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return errors.New("Forge material issuer is invalid")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	for _, resource := range resources {
		originalRoute := isDispatchBinaryMaterialType(resource.MediaType)
		if originalRoute {
			if resource.SourceKind != "owner" && resource.SourceKind != "approval" ||
				resource.SourceKind == "approval" && resource.RequestID == "" ||
				resource.SourceKind == "owner" && resource.RequestID != "" {
				return fmt.Errorf("Forge material %q has no valid frozen source route", resource.Name)
			}
			bearerBytes := []byte(bearer)
			verifyErr := businessaction.ReadVerifiedForgeOriginal(ctx, issuer, bearerBytes, businessaction.ForgeOriginalReference{
				SourceKind: resource.SourceKind, RequestID: resource.RequestID, FileID: resource.ID,
				MediaType: resource.MediaType, Bytes: resource.Bytes, SHA256: resource.SHA256,
			})
			clear(bearerBytes)
			if verifyErr != nil {
				return fmt.Errorf("Forge original material %q cannot be verified", resource.Name)
			}
			continue
		}
		fileURL := *base
		fileURL.Path = "/api/v1/storage/files/" + url.PathEscape(resource.ID)
		fileURL.RawPath, fileURL.RawQuery, fileURL.Fragment = "", "", ""
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, fileURL.String(), nil)
		if reqErr != nil {
			return errors.New("Forge material request is invalid")
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		response, requestErr := client.Do(req)
		if requestErr != nil {
			return fmt.Errorf("Forge material %q is unavailable", resource.Name)
		}
		limited := http.MaxBytesReader(nil, response.Body, dispatchInputResourceMaxBytes+1)
		content, readErr := io.ReadAll(limited)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || readErr != nil || int64(len(content)) != resource.Bytes {
			clear(content)
			return fmt.Errorf("Forge material %q cannot be read at the frozen version", resource.Name)
		}
		digest := sha256.Sum256(content)
		actualSHA256 := hex.EncodeToString(digest[:])
		if actualSHA256 != resource.SHA256 {
			clear(content)
			return fmt.Errorf("Forge material %q does not match the frozen SHA-256", resource.Name)
		}
		clear(content)
	}
	return nil
}

func isDispatchBinaryMaterialType(mediaType string) bool {
	return mediaType == "application/pdf" || mediaType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

func ensurePreparedActions(prepared *preparedBusinessDelegation, actions []string) bool {
	if prepared == nil {
		return len(actions) == 0
	}
	if len(prepared.actions) != len(actions) {
		return false
	}
	for index := range actions {
		if actions[index] != prepared.actions[index] {
			return false
		}
	}
	return true
}

func persistBusinessDelegationTx(ctx context.Context, tx pgx.Tx, prepared *preparedBusinessDelegation, workspaceID, userID, inputRevisionID, taskSHA, workflowID string, version int) error {
	if prepared == nil {
		return nil
	}
	delegationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("weave-forge-task-delegation\x1f"+workspaceID+"\x1f"+userID+"\x1f"+inputRevisionID))
	credentialRef := "forge-task:" + delegationID.String()
	actionsJSON, _ := json.Marshal(prepared.actions)
	resources := make([]any, 0, len(prepared.resources)+1)
	resources = append(resources, map[string]any{"type": "dispatch-input", "id": inputRevisionID, "sha256": taskSHA})
	for _, item := range prepared.resources {
		resources = append(resources, item)
	}
	if prepared.record != nil {
		resources = append(resources, prepared.record.resource())
	}
	resourcesJSON, _ := json.Marshal(resources)
	issuedAt := prepared.issuedAt
	tag, err := tx.Exec(ctx, `INSERT INTO weave_task_business_delegations
		(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,
		 credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,grant_id,scope_sha256,refresh_generation,forge_base_url,forge_delegation_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (workspace_id,input_revision_id) DO UPDATE SET
			 credential_ciphertext=CASE WHEN EXCLUDED.refresh_generation>weave_task_business_delegations.refresh_generation
			   THEN EXCLUDED.credential_ciphertext ELSE weave_task_business_delegations.credential_ciphertext END,
			 credential_sha256=EXCLUDED.credential_sha256,
			 issued_at=EXCLUDED.issued_at,expires_at=EXCLUDED.expires_at,grant_id=EXCLUDED.grant_id,scope_sha256=EXCLUDED.scope_sha256,
			 refresh_generation=EXCLUDED.refresh_generation,forge_base_url=EXCLUDED.forge_base_url
		WHERE weave_task_business_delegations.user_id=EXCLUDED.user_id
		 AND weave_task_business_delegations.issuer=EXCLUDED.issuer
		 AND weave_task_business_delegations.external_subject=EXCLUDED.external_subject
		 AND weave_task_business_delegations.external_organization=EXCLUDED.external_organization
		 AND weave_task_business_delegations.forge_delegation_id=EXCLUDED.forge_delegation_id
		 AND weave_task_business_delegations.grant_id=EXCLUDED.grant_id
		 AND weave_task_business_delegations.scope_sha256=EXCLUDED.scope_sha256
		 AND weave_task_business_delegations.allowed_actions=EXCLUDED.allowed_actions
		 AND weave_task_business_delegations.resources=EXCLUDED.resources
		 AND weave_task_business_delegations.workflow_id=EXCLUDED.workflow_id
		 AND weave_task_business_delegations.workflow_version=EXCLUDED.workflow_version
		 AND weave_task_business_delegations.revoked_at IS NULL
		 AND (EXCLUDED.refresh_generation>weave_task_business_delegations.refresh_generation
		   OR (EXCLUDED.refresh_generation=weave_task_business_delegations.refresh_generation
		     AND EXCLUDED.credential_sha256=weave_task_business_delegations.credential_sha256
		     AND EXCLUDED.forge_base_url=weave_task_business_delegations.forge_base_url
		     AND EXCLUDED.issued_at=weave_task_business_delegations.issued_at
		     AND EXCLUDED.expires_at=weave_task_business_delegations.expires_at))`,
		workspaceID, userID, inputRevisionID, delegationID, credentialRef,
		prepared.identity.Issuer, prepared.identity.Subject, prepared.identity.NativeOrganization,
		prepared.ciphertext, prepared.digest, string(actionsJSON), string(resourcesJSON), workflowID, version, issuedAt, prepared.expiresAt,
		prepared.grantID, prepared.scopeSHA, prepared.generation, prepared.forgeBaseURL, prepared.grantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("Forge task delegation conflicts with frozen input facts")
	}
	return nil
}
