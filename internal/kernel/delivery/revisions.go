package delivery

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const CodeDependencyVersionRequired = "workflow_dependency_version_required"

var ErrDependencyVersionRequired = &Error{code: CodeDependencyVersionRequired}

// ResolveDeliveryRevisionTx locks and freezes one exact immutable delivery
// target revision using the caller-owned transaction.
func (s *Store) ResolveDeliveryRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, targetID string,
	dependencyVersion *int64,
) (frozen.FrozenDeliveryTarget, error) {
	if tx == nil ||
		workspaceID == "" ||
		targetID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		targetID != strings.TrimSpace(targetID) ||
		dependencyVersion == nil ||
		*dependencyVersion < 1 ||
		*dependencyVersion > frozen.MaxJCSSafeInteger {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrDependencyVersionRequired,
			"exact workspace, delivery target identity, and revision are required",
		)
	}
	return s.resolveDeliveryRevisionTx(
		ctx, tx, workspaceID, targetID, *dependencyVersion,
	)
}

// ResolveDeliveryHeadTx locks and freezes the current immutable delivery
// target revision using the caller-owned transaction.
func (s *Store) ResolveDeliveryHeadTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, targetID string,
) (frozen.FrozenDeliveryTarget, error) {
	if tx == nil ||
		workspaceID == "" ||
		targetID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		targetID != strings.TrimSpace(targetID) {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrDependencyVersionRequired,
			"exact workspace and delivery target identity are required",
		)
	}

	var latestRevision int64
	err := tx.QueryRow(ctx, `
		SELECT latest_revision
		FROM weave_delivery_targets
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, targetID).Scan(&latestRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrDependencyVersionRequired,
			"current delivery target revision is unavailable",
		)
	}
	if err != nil {
		return frozen.FrozenDeliveryTarget{}, fmt.Errorf(
			"resolve delivery target head: %w", err,
		)
	}
	if latestRevision < 1 || latestRevision > frozen.MaxJCSSafeInteger {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target head revision is invalid",
		)
	}
	return s.resolveDeliveryRevisionTx(
		ctx, tx, workspaceID, targetID, latestRevision,
	)
}

func (s *Store) resolveDeliveryRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, targetID string,
	dependencyVersion int64,
) (frozen.FrozenDeliveryTarget, error) {
	var (
		latestRevision int64
		enabled        bool
		revokedAt      *time.Time
		deletedAt      *time.Time
		revision       Revision
	)
	err := tx.QueryRow(ctx, `
		SELECT head.latest_revision, head.enabled, head.revoked_at, head.deleted_at,
		       revision.workspace_id, revision.target_id, revision.revision,
		       revision.kind, revision.transport, revision.url, revision.method,
		       revision.content_type, revision.timeout_seconds,
		       revision.header_names, revision.content_hash, revision.created_at
		FROM weave_delivery_targets AS head
		JOIN weave_delivery_target_revisions AS revision
		  ON revision.workspace_id=head.workspace_id
		 AND revision.target_id=head.id
		WHERE head.workspace_id=$1 AND head.id=$2
		  AND revision.workspace_id=$1 AND revision.target_id=$2
		  AND revision.revision=$3
		FOR SHARE OF head, revision
	`, workspaceID, targetID, dependencyVersion).Scan(
		&latestRevision, &enabled, &revokedAt, &deletedAt,
		&revision.WorkspaceID, &revision.TargetID, &revision.Revision,
		&revision.Kind, &revision.Transport, &revision.URL, &revision.Method,
		&revision.ContentType, &revision.TimeoutSeconds, &revision.HeaderNames,
		&revision.ContentHash, &revision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrDependencyVersionRequired,
			"exact delivery target revision is unavailable",
		)
	}
	if err != nil {
		return frozen.FrozenDeliveryTarget{}, fmt.Errorf(
			"resolve delivery target revision: %w", err,
		)
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrCredentialUnavailable,
			"delivery target access is closed",
		)
	}
	if latestRevision < dependencyVersion ||
		latestRevision < 1 ||
		latestRevision > frozen.MaxJCSSafeInteger ||
		revision.WorkspaceID != workspaceID ||
		revision.TargetID != targetID ||
		revision.Revision != dependencyVersion {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target revision identity is invalid",
		)
	}
	if revision.Transport != "http" ||
		revision.Method != "POST" ||
		revision.ContentType != "application/json" {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target transport contract is invalid",
		)
	}
	if revision.HeaderNames == nil {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target header names are not canonical",
		)
	}
	headers := make(map[string]string, len(revision.HeaderNames))
	for _, name := range revision.HeaderNames {
		headers[name] = ""
	}
	_, normalizedNames, err := normalizeConfig(workspaceID, targetID, Config{
		Kind:           revision.Kind,
		URL:            revision.URL,
		TimeoutSeconds: revision.TimeoutSeconds,
		Headers:        headers,
	})
	if err != nil || !slices.Equal(revision.HeaderNames, normalizedNames) {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target functional configuration is invalid",
		)
	}

	target := frozenTarget(revision)
	wantHash, err := frozen.HashDTO(target, frozen.PreorderFrozenDeliveryTarget)
	if err != nil || subtle.ConstantTimeCompare(
		[]byte(wantHash), []byte(revision.ContentHash),
	) != 1 {
		return frozen.FrozenDeliveryTarget{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target content hash does not match",
		)
	}
	target.ContentHash = revision.ContentHash
	return target, nil
}
