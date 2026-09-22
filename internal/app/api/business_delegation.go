package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
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

func (s *Server) prepareBusinessDelegation(c echo.Context, actions []string) (*preparedBusinessDelegation, error) {
	if len(actions) == 0 {
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
		actions: append([]string(nil), actions...), expiresAt: time.Now().UTC().Add(forgeDelegationTTL),
	}, nil
}

func ensurePreparedActions(prepared *preparedBusinessDelegation, actions []string) bool {
	if len(actions) == 0 {
		return prepared == nil
	}
	if prepared == nil || len(prepared.actions) != len(actions) {
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
	resourcesJSON, _ := json.Marshal([]map[string]string{{"type": "dispatch-input", "id": inputRevisionID, "sha256": taskSHA}})
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
