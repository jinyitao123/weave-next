package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

type SystemProviderSource interface {
	SystemProvider(id string) (llmrouter.ProviderConfig, bool)
}

// SystemProviderLister is the read-side counterpart of SystemProviderSource
// for endpoints that enumerate the process-configured providers. API keys in
// the returned configs are already masked by the source.
type SystemProviderLister interface {
	ListProviders() []llmrouter.ProviderConfig
}

type MirrorOutcome string

const (
	MirrorOutcomeCreated           MirrorOutcome = "created"
	MirrorOutcomeFunctionalUpdated MirrorOutcome = "functional_updated"
	MirrorOutcomeCredentialRotated MirrorOutcome = "credential_rotated"
	MirrorOutcomeNoop              MirrorOutcome = "noop"
)

type SystemProviderMirrorResult struct {
	ProviderID string        `json:"provider_id"`
	Revision   int64         `json:"revision"`
	Outcome    MirrorOutcome `json:"outcome"`
}

func (s *Store) MirrorSystemProvider(
	ctx context.Context,
	workspaceID, operatorID, systemProviderID string,
	source SystemProviderSource,
) (SystemProviderMirrorResult, error) {
	rawWorkspaceID := workspaceID
	rawOperatorID := operatorID
	rawSystemProviderID := systemProviderID
	workspaceID = strings.TrimSpace(rawWorkspaceID)
	operatorID = strings.TrimSpace(rawOperatorID)
	systemProviderID = strings.TrimSpace(rawSystemProviderID)
	if workspaceID == "" || operatorID == "" || systemProviderID == "" || source == nil ||
		workspaceID != rawWorkspaceID || operatorID != rawOperatorID ||
		systemProviderID != rawSystemProviderID ||
		strings.HasPrefix(systemProviderID, "system/") {
		return SystemProviderMirrorResult{}, coded(
			CodeProviderRevisionRequired,
			"exact workspace, operator, and system provider identity are required",
		)
	}
	sourceConfig, ok := source.SystemProvider(systemProviderID)
	if !ok || sourceConfig.ID != systemProviderID {
		return SystemProviderMirrorResult{}, coded(
			CodeProviderRevisionRequired,
			"exact system provider source is unavailable",
		)
	}
	if sourceConfig.APIKey == "" || sourceConfig.APIKey == MaskedKey {
		return SystemProviderMirrorResult{}, coded(
			CodeCredentialUnavailable,
			"system provider credential is unavailable",
		)
	}
	targetID := "system/" + systemProviderID
	sourceConfig.ID = targetID
	sourceConfig.CredentialScope = frozen.CredentialScopeWorkspaceService
	sourceConfig.CredentialUserID = ""
	sourceConfig.CredentialServiceID = "system-provider:" + systemProviderID
	if err := AuthorizeReference(ctx, providerConfigReference(workspaceID, sourceConfig)); err != nil {
		return SystemProviderMirrorResult{}, err
	}
	normalized, err := normalizeMirroredProviderConfig(workspaceID, sourceConfig)
	if err != nil {
		return SystemProviderMirrorResult{}, err
	}
	contentHash, err := hashProviderRevision(normalized)
	if err != nil {
		return SystemProviderMirrorResult{}, coded(
			CodeFrozenManifestMismatch,
			"hash mirrored provider functional configuration",
		)
	}
	sourceCiphertext, err := secret.Seal(s.key, []byte(normalized.APIKey))
	if err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("seal mirrored provider credential: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("begin system provider mirror: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, fmt.Sprintf("%d:%s%d:%s", len(workspaceID), workspaceID, len(targetID), targetID)); err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("lock system provider mirror identity: %w", err)
	}

	var existingSourceKind string
	err = tx.QueryRow(ctx, `
		SELECT source_kind
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, targetID).Scan(&existingSourceKind)
	switch {
	case err == nil && existingSourceKind != "system_mirror":
		return SystemProviderMirrorResult{}, coded(
			CodeProviderRevisionRequired,
			"reserved system provider identity is already occupied",
		)
	case err != nil && !isNoRows(err):
		return SystemProviderMirrorResult{}, fmt.Errorf("lock mirrored provider head: %w", err)
	}

	created := isNoRows(err)
	if created {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_provider_credentials (
			  workspace_id, id, name, base_url, api_key_cipher, models,
			  json_object_mode, latest_revision, source_kind, source_provider_id,credential_scope,credential_user_id,credential_service_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 'system_mirror', $8,$9,$10,$11)
		`, workspaceID, targetID, normalized.Name, normalized.BaseURL,
			sourceCiphertext, normalized.Models, normalized.JSONObjectMode,
			systemProviderID, normalized.CredentialScope, normalized.CredentialUserID, normalized.CredentialServiceID,
		); err != nil {
			return SystemProviderMirrorResult{}, fmt.Errorf("create mirrored provider head: %w", err)
		}
	}

	head, storedCiphertext, err := getProviderHeadTx(ctx, tx, workspaceID, targetID, !created)
	if err != nil {
		return SystemProviderMirrorResult{}, err
	}
	if head.SourceKind != "system_mirror" || head.SourceProviderID == nil ||
		*head.SourceProviderID != systemProviderID {
		return SystemProviderMirrorResult{}, coded(
			CodeProviderRevisionRequired,
			"mirrored provider source identity conflicts with the requested source",
		)
	}
	if !head.Enabled || head.RevokedAt != nil || head.DeletedAt != nil {
		return SystemProviderMirrorResult{}, coded(
			CodeCredentialUnavailable,
			"closed system provider mirror cannot be revived",
		)
	}
	storedKey, err := secret.Open(s.key, storedCiphertext)
	// This operation is an explicit admin mirror of the process-configured
	// system provider. If an earlier master-key change or ciphertext corruption
	// stranded the workspace mirror, the configured source credential is the
	// only recoverable authority; reseal it below while preserving the frozen
	// provider identity and functional revision. Closed mirrors are rejected
	// above and are never revived here.
	keyChanged := err != nil || !bytes.Equal(storedKey, []byte(normalized.APIKey))

	var revision ProviderRevision
	functionalChanged := head.LatestRevision == 0
	if head.LatestRevision > 0 {
		revision, err = getProviderRevisionTx(
			ctx, tx, workspaceID, targetID, head.LatestRevision, true,
		)
		if err != nil {
			return SystemProviderMirrorResult{}, err
		}
		functionalChanged = revision.ContentHash != contentHash
	}

	beforeRevision := head.LatestRevision
	afterRevision := beforeRevision
	outcome := MirrorOutcomeNoop
	switch {
	case created:
		outcome = MirrorOutcomeCreated
	case functionalChanged:
		outcome = MirrorOutcomeFunctionalUpdated
	case keyChanged:
		outcome = MirrorOutcomeCredentialRotated
	}

	if functionalChanged {
		if beforeRevision >= frozen.MaxJCSSafeInteger {
			return SystemProviderMirrorResult{}, coded(
				CodeProviderRevisionRequired,
				"provider revision exceeds the JCS safe integer range",
			)
		}
		afterRevision++
		revision = ProviderRevision{
			WorkspaceID:    workspaceID,
			ProviderID:     targetID,
			Revision:       afterRevision,
			Name:           normalized.Name,
			BaseURL:        normalized.BaseURL,
			Models:         append([]string(nil), normalized.Models...),
			JSONObjectMode: normalized.JSONObjectMode,
			ContentHash:    contentHash,
		}
		modelsJSON, err := json.Marshal(revision.Models)
		if err != nil {
			return SystemProviderMirrorResult{}, coded(
				CodeFrozenManifestMismatch,
				"encode mirrored provider models",
			)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_provider_revisions (
			  workspace_id, provider_id, revision, name, base_url, models,
			  json_object_mode, content_hash
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, revision.WorkspaceID, revision.ProviderID, revision.Revision,
			revision.Name, revision.BaseURL, string(modelsJSON), revision.JSONObjectMode,
			revision.ContentHash,
		); err != nil {
			return SystemProviderMirrorResult{}, fmt.Errorf("insert mirrored ProviderRevision: %w", err)
		}
	}

	nextCiphertext := storedCiphertext
	if keyChanged {
		nextCiphertext = sourceCiphertext
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_provider_credentials
		SET name=$3,
		    base_url=$4,
		    api_key_cipher=$5,
		    models=$6,
		    json_object_mode=$7,
		    latest_revision=$8,
		    updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, targetID, normalized.Name, normalized.BaseURL,
		nextCiphertext, normalized.Models, normalized.JSONObjectMode, afterRevision,
	); err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("update mirrored provider head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_system_provider_mirror_audits (
		  workspace_id, audit_id, provider_id, system_provider_id, operator_id,
		  outcome, before_revision, after_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, workspaceID, uuid.NewString(), targetID, systemProviderID, operatorID,
		string(outcome), beforeRevision, afterRevision,
	); err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("append system provider mirror audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SystemProviderMirrorResult{}, fmt.Errorf("commit system provider mirror: %w", err)
	}
	return SystemProviderMirrorResult{
		ProviderID: targetID,
		Revision:   afterRevision,
		Outcome:    outcome,
	}, nil
}

func normalizeMirroredProviderConfig(
	workspaceID string,
	cfg llmrouter.ProviderConfig,
) (llmrouter.ProviderConfig, error) {
	if !strings.HasPrefix(cfg.ID, "system/") || strings.TrimPrefix(cfg.ID, "system/") == "" {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"mirrored provider target identity is invalid",
		)
	}
	targetID := cfg.ID
	cfg.ID = strings.TrimPrefix(cfg.ID, "system/")
	normalized, err := normalizeProviderConfig(workspaceID, cfg)
	if err != nil {
		return llmrouter.ProviderConfig{}, err
	}
	normalized.ID = targetID
	return normalized, nil
}

func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}
