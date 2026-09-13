package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const (
	CodeDependencyVersionRequired = "workflow_dependency_version_required"
	CodeCredentialUnavailable     = "workflow_credential_unavailable"
	CodeFrozenManifestMismatch    = "workflow_frozen_manifest_mismatch"
)

var (
	ErrDependencyVersionRequired = &Error{code: CodeDependencyVersionRequired}
	ErrCredentialUnavailable     = &Error{code: CodeCredentialUnavailable}
	ErrFrozenManifestMismatch    = &Error{code: CodeFrozenManifestMismatch}
)

// Error carries the stable machine code for runtime freeze failures.
type Error struct {
	code   string
	detail string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.detail == "" {
		return e.code
	}
	return e.code + ": " + e.detail
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.code == other.code
}

func coded(base *Error, detail string) error {
	return &Error{code: base.code, detail: detail}
}

// FrozenRuntimeRecord is the exact, secretless functional state used to build
// an engine-specific FrozenRuntimeBinding.
type FrozenRuntimeRecord struct {
	WorkspaceID     string                     `json:"workspace_id"`
	RuntimeID       string                     `json:"runtime_id"`
	RuntimeRevision int64                      `json:"runtime_revision"`
	Engines         []string                   `json:"engines"`
	AccessRef       frozen.CredentialReference `json:"access_ref"`
}

// ResolveCurrentRuntimeRevisionTx discovers and locks the current functional
// revision before resolving its exact, secretless runtime record.
func ResolveCurrentRuntimeRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, runtimeID string,
) (FrozenRuntimeRecord, error) {
	if tx == nil ||
		workspaceID == "" ||
		runtimeID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		runtimeID != strings.TrimSpace(runtimeID) {
		return FrozenRuntimeRecord{}, coded(
			ErrDependencyVersionRequired,
			"exact workspace and runtime identity are required",
		)
	}

	var functionalRevision int64
	if err := tx.QueryRow(ctx, `
		SELECT functional_revision
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, runtimeID).Scan(&functionalRevision); errors.Is(err, pgx.ErrNoRows) {
		return FrozenRuntimeRecord{}, coded(
			ErrDependencyVersionRequired,
			"current runtime functional revision is unavailable",
		)
	} else if err != nil {
		return FrozenRuntimeRecord{}, fmt.Errorf("discover runtime functional revision: %w", err)
	}
	if functionalRevision < 1 || functionalRevision > frozen.MaxJCSSafeInteger {
		return FrozenRuntimeRecord{}, coded(
			ErrFrozenManifestMismatch,
			"stored runtime functional revision is malformed",
		)
	}
	return ResolveRuntimeRevisionTx(
		ctx, tx, workspaceID, runtimeID, &functionalRevision,
	)
}

// ResolveRuntimeRevisionTx locks and reads one exact runtime functional
// revision using the caller-owned transaction.
func ResolveRuntimeRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, runtimeID string,
	functionalRevision *int64,
) (FrozenRuntimeRecord, error) {
	if tx == nil ||
		workspaceID == "" ||
		runtimeID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		runtimeID != strings.TrimSpace(runtimeID) ||
		functionalRevision == nil ||
		*functionalRevision < 1 ||
		*functionalRevision > frozen.MaxJCSSafeInteger {
		return FrozenRuntimeRecord{}, coded(
			ErrDependencyVersionRequired,
			"exact workspace, runtime identity, and functional revision are required",
		)
	}

	var (
		enginesJSON []byte
		enabled     bool
		revokedAt   *time.Time
		deletedAt   *time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT engines, enabled, revoked_at, deleted_at
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2 AND functional_revision=$3
		FOR SHARE
	`, workspaceID, runtimeID, *functionalRevision).Scan(
		&enginesJSON, &enabled, &revokedAt, &deletedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return FrozenRuntimeRecord{}, coded(
			ErrDependencyVersionRequired,
			"exact runtime functional revision is unavailable",
		)
	} else if err != nil {
		return FrozenRuntimeRecord{}, fmt.Errorf("resolve runtime functional revision: %w", err)
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return FrozenRuntimeRecord{}, coded(
			ErrCredentialUnavailable,
			"runtime access is closed",
		)
	}

	var decodedEngines []*string
	if err := json.Unmarshal(enginesJSON, &decodedEngines); err != nil || decodedEngines == nil {
		return FrozenRuntimeRecord{}, coded(
			ErrFrozenManifestMismatch,
			"stored runtime engines are malformed",
		)
	}
	engines := make([]string, len(decodedEngines))
	for index, engine := range decodedEngines {
		if engine == nil {
			return FrozenRuntimeRecord{}, coded(
				ErrFrozenManifestMismatch,
				"stored runtime engines are malformed",
			)
		}
		engines[index] = *engine
	}
	canonical, err := canonicalEngines(engines)
	if err != nil || !slices.Equal(engines, canonical) {
		return FrozenRuntimeRecord{}, coded(
			ErrFrozenManifestMismatch,
			"stored runtime engines are malformed",
		)
	}
	engines = canonical

	return FrozenRuntimeRecord{
		WorkspaceID:     workspaceID,
		RuntimeID:       runtimeID,
		RuntimeRevision: *functionalRevision,
		Engines:         engines,
		AccessRef: frozen.CredentialReference{
			Scope:             frozen.CredentialScopeWorkspaceService,
			ServiceID:         "runtime:" + runtimeID,
			SchemaVersion:     frozen.FrozenSchemaVersion,
			WorkspaceID:       workspaceID,
			Kind:              frozen.CredentialRuntimeAccess,
			ResourceID:        runtimeID,
			Slot:              "access",
			CredentialVersion: nil,
		},
	}, nil
}
