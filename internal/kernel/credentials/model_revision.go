package credentials

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const CodeDependencyVersionRequired = "workflow_dependency_version_required"

var ErrDependencyVersionRequired = &Error{code: CodeDependencyVersionRequired}

type modelProviderCandidate struct {
	credentialScope     frozen.CredentialScope
	credentialUserID    string
	credentialServiceID string
	providerID          string
	latestRevision      int64
	sourceKind          string
	sourceProviderID    *string
	enabled             bool
	revokedAt           *time.Time
	deletedAt           *time.Time
}

// GetProviderRevisionTx reads and locks one exact immutable provider revision
// using the caller-owned transaction.
func GetProviderRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, providerID string,
	revision int64,
) (ProviderRevision, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(providerID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		providerID != strings.TrimSpace(providerID) ||
		revision < 1 ||
		revision > frozen.MaxJCSSafeInteger {
		return ProviderRevision{}, coded(
			CodeDependencyVersionRequired,
			"exact workspace, provider, and revision identity are required",
		)
	}
	if _, _, err := getProviderHeadTx(ctx, tx, workspaceID, providerID, false); err != nil {
		return ProviderRevision{}, err
	}
	return getProviderRevisionTx(
		ctx, tx, workspaceID, providerID, revision, true,
		CodeDependencyVersionRequired,
	)
}

// ResolveModelRevisionTx resolves a model to the workspace's exact functional
// ProviderRevision and returns a secretless frozen binding. Workspace-owned
// declarations take precedence over explicit system mirrors.
func ResolveModelRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, modelID string,
) (frozen.FrozenModelBinding, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(modelID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		modelID != strings.TrimSpace(modelID) {
		return frozen.FrozenModelBinding{}, coded(
			CodeProviderRevisionRequired,
			"exact workspace and model identity are required",
		)
	}

	if _, err := execution.RequireSubject(ctx, workspaceID); err != nil {
		return frozen.FrozenModelBinding{}, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, latest_revision, source_kind, source_provider_id,
		       enabled, revoked_at, deleted_at,credential_scope,credential_user_id,credential_service_id
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND $2=ANY(models)
		FOR SHARE
	`, workspaceID, modelID)
	if err != nil {
		return frozen.FrozenModelBinding{}, fmt.Errorf("lock model provider heads: %w", err)
	}
	defer rows.Close()

	workspaceCandidates := make([]modelProviderCandidate, 0, 1)
	systemCandidates := make([]modelProviderCandidate, 0, 1)
	for rows.Next() {
		var candidate modelProviderCandidate
		if err := rows.Scan(
			&candidate.providerID, &candidate.latestRevision,
			&candidate.sourceKind, &candidate.sourceProviderID,
			&candidate.enabled, &candidate.revokedAt, &candidate.deletedAt, &candidate.credentialScope, &candidate.credentialUserID, &candidate.credentialServiceID,
		); err != nil {
			return frozen.FrozenModelBinding{}, fmt.Errorf("scan model provider head: %w", err)
		}
		ref := frozen.CredentialReference{SchemaVersion: frozen.FrozenSchemaVersion, WorkspaceID: workspaceID, Kind: frozen.CredentialProviderAPIKey, ResourceID: candidate.providerID, Slot: "api_key", Scope: candidate.credentialScope, UserID: candidate.credentialUserID, ServiceID: candidate.credentialServiceID}
		if AuthorizeReference(ctx, ref) != nil {
			continue
		}
		switch candidate.sourceKind {
		case "workspace":
			workspaceCandidates = append(workspaceCandidates, candidate)
		case "system_mirror":
			systemCandidates = append(systemCandidates, candidate)
		default:
			return frozen.FrozenModelBinding{}, coded(
				CodeFrozenManifestMismatch,
				"stored provider source kind is invalid",
			)
		}
	}
	if err := rows.Err(); err != nil {
		return frozen.FrozenModelBinding{}, fmt.Errorf("read model provider heads: %w", err)
	}

	personal := make([]modelProviderCandidate, 0)
	for _, candidate := range workspaceCandidates {
		if candidate.credentialScope == frozen.CredentialScopeUser {
			personal = append(personal, candidate)
		}
	}
	if len(personal) > 0 {
		workspaceCandidates = personal
	}
	candidates := workspaceCandidates
	if len(candidates) == 0 {
		candidates = systemCandidates
	}
	if len(candidates) == 0 {
		return frozen.FrozenModelBinding{}, coded(
			CodeProviderRevisionRequired,
			"model has no workspace provider revision or explicit system mirror",
		)
	}
	if len(candidates) != 1 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].providerID < candidates[j].providerID
		})
		return frozen.FrozenModelBinding{}, coded(
			CodeProviderRevisionRequired,
			"model is declared by multiple providers at the same priority",
		)
	}

	selected := candidates[0]
	if !selected.enabled || selected.revokedAt != nil || selected.deletedAt != nil {
		return frozen.FrozenModelBinding{}, coded(
			CodeCredentialUnavailable,
			"selected provider credential is closed",
		)
	}
	if selected.latestRevision < 1 {
		return frozen.FrozenModelBinding{}, coded(
			CodeProviderRevisionRequired,
			"selected provider has no immutable functional revision",
		)
	}
	if selected.sourceKind == "system_mirror" &&
		(selected.sourceProviderID == nil ||
			selected.providerID != "system/"+*selected.sourceProviderID) {
		return frozen.FrozenModelBinding{}, coded(
			CodeProviderRevisionRequired,
			"selected system provider is not an explicit mirror",
		)
	}

	revision, err := GetProviderRevisionTx(
		ctx, tx, workspaceID, selected.providerID, selected.latestRevision,
	)
	if err != nil {
		return frozen.FrozenModelBinding{}, err
	}
	index := sort.SearchStrings(revision.Models, modelID)
	if index >= len(revision.Models) || revision.Models[index] != modelID {
		return frozen.FrozenModelBinding{}, coded(
			CodeFrozenManifestMismatch,
			"provider head model membership does not match its exact revision",
		)
	}

	binding := frozen.FrozenModelBinding{
		SchemaVersion:    frozen.FrozenSchemaVersion,
		WorkspaceID:      workspaceID,
		ProviderID:       revision.ProviderID,
		ProviderRevision: revision.Revision,
		ModelID:          modelID,
		BaseURL:          revision.BaseURL,
		JSONObjectMode:   revision.JSONObjectMode,
		CredentialRef: frozen.CredentialReference{
			SchemaVersion: frozen.FrozenSchemaVersion,
			WorkspaceID:   workspaceID,
			Scope:         selected.credentialScope, UserID: selected.credentialUserID, ServiceID: selected.credentialServiceID,
			Kind:              frozen.CredentialProviderAPIKey,
			ResourceID:        revision.ProviderID,
			Slot:              "api_key",
			CredentialVersion: nil,
		},
	}
	contentHash, err := frozen.HashDTO(binding, frozen.PreorderFrozenModelBinding)
	if err != nil {
		if errors.Is(err, frozen.ErrFrozenContentHashMismatch) {
			return frozen.FrozenModelBinding{}, coded(
				CodeFrozenManifestMismatch,
				"frozen model binding hash input is invalid",
			)
		}
		return frozen.FrozenModelBinding{}, coded(
			CodeFrozenManifestMismatch,
			"canonicalize frozen model binding",
		)
	}
	binding.ContentHash = contentHash
	return binding, nil
}
