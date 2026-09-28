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
	"github.com/labstack/echo/v4"
)

const forgeDelegationHeader = "X-Weave-Forge-Authorization"
const forgeDelegationTTL = 30 * time.Minute

type preparedBusinessDelegation struct {
	identity   ExternalIdentity
	ciphertext string
	digest     string
	actions    []string
	resources  []dispatchInputResource
	record     *dispatchBusinessRecord
	expiresAt  time.Time
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

func (s *Server) prepareBusinessDelegation(c echo.Context, actions []string, resources []dispatchInputResource, record *dispatchBusinessRecord) (*preparedBusinessDelegation, error) {
	if len(actions) == 0 && len(resources) == 0 && record == nil {
		return nil, nil
	}
	authorization := strings.TrimSpace(c.Request().Header.Get(forgeDelegationHeader))
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) || len(authorization) <= len(prefix) || len(authorization) > 64<<10 {
		return nil, workflowError(c, http.StatusUnauthorized, "business_delegation_required", "Forge task delegation is required")
	}
	if s.ExternalIdentity == nil || s.GetPool() == nil {
		return nil, workflowError(c, http.StatusServiceUnavailable, "business_delegation_unavailable", "Forge task delegation is unavailable")
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	identity, err := s.ExternalIdentity.Verify(c.Request().Context(), bearer)
	if err != nil {
		return nil, workflowError(c, http.StatusUnauthorized, "business_delegation_invalid", "Forge task delegation could not be verified")
	}
	if err := verifyForgeFiles(c.Request().Context(), identity.Issuer, bearer, resources); err != nil {
		return nil, workflowError(c, http.StatusUnprocessableEntity, "business_resource_invalid", err.Error())
	}
	workspaceID, userID := getTenant(c), getUserID(c)
	if identity.Organization != workspaceID {
		return nil, workflowError(c, http.StatusForbidden, "business_delegation_identity_mismatch", "Forge task delegation belongs to another organization")
	}
	var boundUserID string
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT user_id FROM weave_external_identities
		WHERE issuer=$1 AND subject=$2 AND workspace_id=$3`, identity.Issuer, identity.Subject, workspaceID).Scan(&boundUserID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && boundUserID != userID {
		return nil, workflowError(c, http.StatusForbidden, "business_delegation_identity_mismatch", "Forge task delegation belongs to another employee")
	}
	if err != nil {
		return nil, workflowStoreFailure(c, fmt.Errorf("read Forge identity binding: %w", err))
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return nil, workflowError(c, http.StatusServiceUnavailable, "business_delegation_unavailable", "Forge task delegation encryption is unavailable")
	}
	ciphertext, err := secret.Seal(key, []byte(bearer))
	for index := range key {
		key[index] = 0
	}
	if err != nil {
		return nil, workflowStoreFailure(c, fmt.Errorf("encrypt Forge task delegation: %w", err))
	}
	digest := sha256.Sum256([]byte(bearer))
	return &preparedBusinessDelegation{
		identity: identity, ciphertext: ciphertext, digest: hex.EncodeToString(digest[:]),
		actions: append([]string{}, actions...), resources: append([]dispatchInputResource(nil), resources...), record: record, expiresAt: time.Now().UTC().Add(forgeDelegationTTL),
	}, nil
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
	issuedAt := time.Now().UTC()
	tag, err := tx.Exec(ctx, `INSERT INTO weave_task_business_delegations
		(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,
		 credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13,$14,$15,$16)
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
		 AND weave_task_business_delegations.revoked_at IS NULL`,
		workspaceID, userID, inputRevisionID, delegationID, credentialRef,
		prepared.identity.Issuer, prepared.identity.Subject, prepared.identity.Organization,
		prepared.ciphertext, prepared.digest, string(actionsJSON), string(resourcesJSON), workflowID, version, issuedAt, prepared.expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("Forge task delegation conflicts with frozen input facts")
	}
	return nil
}
