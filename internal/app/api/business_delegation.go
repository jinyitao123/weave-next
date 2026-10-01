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

// The Workbench Host sends the Forge-issued task delegation (decision 002):
// its credential, Forge's delegation identifier, and Forge's expiry. The
// employee's own desktop session is never accepted here.
const forgeDelegationHeader = "X-Weave-Forge-Authorization"
const forgeDelegationIDHeader = "X-Weave-Forge-Delegation-Id"
const forgeDelegationExpiresHeader = "X-Weave-Forge-Delegation-Expires"

// forgeDelegationMaxLifetime mirrors Forge's deployment cap plus clock skew;
// Forge remains the authority on expiry.
const forgeDelegationMaxLifetime = 24*time.Hour + 5*time.Minute

type forgeDelegationHeaders struct {
	authorization string
	delegationID  string
	expires       string
}

func forgeDelegationHeadersFrom(header http.Header) forgeDelegationHeaders {
	return forgeDelegationHeaders{
		authorization: header.Get(forgeDelegationHeader),
		delegationID:  header.Get(forgeDelegationIDHeader),
		expires:       header.Get(forgeDelegationExpiresHeader),
	}
}

func validForgeDelegationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
			return false
		}
	}
	return true
}

type preparedBusinessDelegation struct {
	identity          ExternalIdentity
	forgeDelegationID string
	ciphertext        string
	digest            string
	actions           []string
	resources         []dispatchInputResource
	record            *dispatchBusinessRecord
	expiresAt         time.Time
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

func (s *Server) prepareBusinessDelegation(ctx context.Context, workspaceID, userID string, headers forgeDelegationHeaders, actions []string, resources []dispatchInputResource, record *dispatchBusinessRecord) (*preparedBusinessDelegation, *businessDelegationPreparationError) {
	if len(actions) == 0 && len(resources) == 0 && record == nil {
		return nil, nil
	}
	authorization := strings.TrimSpace(headers.authorization)
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) || len(authorization) <= len(prefix) || len(authorization) > 64<<10 {
		return nil, businessDelegationRejected(http.StatusUnauthorized, "business_delegation_required", "Forge task delegation is required")
	}
	if s.ExternalIdentity == nil || s.GetPool() == nil {
		return nil, businessDelegationRejected(http.StatusServiceUnavailable, "business_delegation_unavailable", "Forge task delegation is unavailable")
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	forgeDelegationID := strings.TrimSpace(headers.delegationID)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(headers.expires))
	now := time.Now().UTC()
	if !validForgeDelegationID(forgeDelegationID) || expiresErr != nil || !expiresAt.After(now) || expiresAt.After(now.Add(forgeDelegationMaxLifetime)) {
		return nil, businessDelegationRejected(http.StatusUnauthorized, "business_delegation_required", "a Forge-issued task delegation is required")
	}
	identity, err := s.ExternalIdentity.Verify(ctx, bearer)
	if err != nil {
		return nil, businessDelegationRejected(http.StatusUnauthorized, "business_delegation_invalid", "Forge task delegation could not be verified")
	}
	if err := verifyForgeFiles(ctx, identity.BaseURL, bearer, resources); err != nil {
		return nil, businessDelegationRejected(http.StatusUnprocessableEntity, "business_resource_invalid", err.Error())
	}
	if identity.Organization != workspaceID {
		return nil, businessDelegationRejected(http.StatusForbidden, "business_delegation_identity_mismatch", "Forge task delegation belongs to another organization")
	}
	var boundUserID string
	err = s.GetPool().QueryRow(ctx, `SELECT user_id FROM weave_external_identities
		WHERE issuer=$1 AND subject=$2 AND workspace_id=$3`, identity.Issuer, identity.Subject, workspaceID).Scan(&boundUserID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && boundUserID != userID {
		return nil, businessDelegationRejected(http.StatusForbidden, "business_delegation_identity_mismatch", "Forge task delegation belongs to another employee")
	}
	if err != nil {
		return nil, businessDelegationStoreFailure(fmt.Errorf("read Forge identity binding: %w", err))
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return nil, businessDelegationRejected(http.StatusServiceUnavailable, "business_delegation_unavailable", "Forge task delegation encryption is unavailable")
	}
	ciphertext, err := secret.Seal(key, []byte(bearer))
	for index := range key {
		key[index] = 0
	}
	if err != nil {
		return nil, businessDelegationStoreFailure(fmt.Errorf("encrypt Forge task delegation: %w", err))
	}
	digest := sha256.Sum256([]byte(bearer))
	return &preparedBusinessDelegation{
		identity: identity, forgeDelegationID: forgeDelegationID, ciphertext: ciphertext, digest: hex.EncodeToString(digest[:]),
		actions: append([]string{}, actions...), resources: append([]dispatchInputResource(nil), resources...), record: record, expiresAt: expiresAt.UTC(),
	}, nil
}

func verifyForgeFiles(ctx context.Context, baseURL, bearer string, resources []dispatchInputResource) error {
	base, err := url.Parse(baseURL)
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
			verifyErr := businessaction.ReadVerifiedForgeOriginal(ctx, baseURL, bearerBytes, businessaction.ForgeOriginalReference{
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
	issuedAt := time.Now().UTC()
	tag, err := tx.Exec(ctx, `INSERT INTO weave_task_business_delegations
		(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,
		 credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,
		 forge_base_url,forge_delegation_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (workspace_id,input_revision_id) DO UPDATE SET
		 credential_ciphertext=EXCLUDED.credential_ciphertext,credential_sha256=EXCLUDED.credential_sha256,
		 expires_at=EXCLUDED.expires_at,refresh_generation=weave_task_business_delegations.refresh_generation+1
		WHERE weave_task_business_delegations.user_id=EXCLUDED.user_id
		 AND weave_task_business_delegations.issuer=EXCLUDED.issuer
		 AND weave_task_business_delegations.external_subject=EXCLUDED.external_subject
		 AND weave_task_business_delegations.external_organization=EXCLUDED.external_organization
		 AND weave_task_business_delegations.allowed_actions=EXCLUDED.allowed_actions
		 AND weave_task_business_delegations.resources=EXCLUDED.resources
		 AND weave_task_business_delegations.workflow_id=EXCLUDED.workflow_id
		 AND weave_task_business_delegations.workflow_version=EXCLUDED.workflow_version
		 AND weave_task_business_delegations.forge_base_url=EXCLUDED.forge_base_url
		 AND weave_task_business_delegations.forge_delegation_id=EXCLUDED.forge_delegation_id
		 AND weave_task_business_delegations.revoked_at IS NULL`,
		workspaceID, userID, inputRevisionID, delegationID, credentialRef,
		prepared.identity.Issuer, prepared.identity.Subject, prepared.identity.Organization,
		prepared.ciphertext, prepared.digest, string(actionsJSON), string(resourcesJSON), workflowID, version, issuedAt, prepared.expiresAt,
		prepared.identity.BaseURL, prepared.forgeDelegationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("Forge task delegation conflicts with frozen input facts")
	}
	return nil
}
