package runtimes

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

var _ credentials.TxReferenceSource = (*Store)(nil)

func (s *Store) ValidateReferenceTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := validateRuntimeReferenceInput(s, tx, ref); err != nil {
		return err
	}

	var state runtimeReferenceState
	err := tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID).Scan(
		&state.enabled, &state.revokedAt, &state.deletedAt,
	)
	if err != nil {
		return classifyRuntimeReferenceReadError(err)
	}
	return validateRuntimeReferenceState(state)
}

func (s *Store) ResolveRuntimeAccessTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
	expectedEngine string,
) (credentials.SecretMaterial, error) {
	if err := validateRuntimeReferenceInput(s, tx, ref); err != nil {
		return credentials.SecretMaterial{}, err
	}
	switch expectedEngine {
	case "claude", "codex", "opencode":
	default:
		return credentials.SecretMaterial{}, newRuntimeReferenceError(nil)
	}

	var (
		state     runtimeReferenceState
		hasEngine bool
	)
	err := tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at, engines ? $3
		FROM weave_runtimes
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID, expectedEngine).Scan(
		&state.enabled, &state.revokedAt, &state.deletedAt, &hasEngine,
	)
	if err != nil {
		return credentials.SecretMaterial{}, classifyRuntimeReferenceReadError(err)
	}
	if err := validateRuntimeReferenceState(state); err != nil {
		return credentials.SecretMaterial{}, err
	}
	if !hasEngine {
		return credentials.SecretMaterial{}, newRuntimeReferenceError(nil)
	}
	return credentials.NewSecretMaterial(nil, nil, nil), nil
}

type runtimeReferenceState struct {
	enabled   bool
	revokedAt *time.Time
	deletedAt *time.Time
}

func validateRuntimeReferenceInput(
	store *Store,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := credentials.ValidateReferenceV1(ref); err != nil {
		if errors.Is(err, credentials.ErrCredentialVersionUnsupported) {
			return err
		}
		return newRuntimeReferenceError(nil)
	}
	if ref.Kind != frozen.CredentialRuntimeAccess || ref.Slot != "access" {
		return newRuntimeReferenceError(nil)
	}
	if store == nil || store.pool == nil || nilRuntimeReferenceInterface(tx) {
		return newRuntimeReferenceError(nil)
	}
	return nil
}

func classifyRuntimeReferenceReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return newRuntimeReferenceError(nil)
	}
	return newRuntimeReferenceError(err)
}

func validateRuntimeReferenceState(state runtimeReferenceState) error {
	if !state.enabled || state.revokedAt != nil || state.deletedAt != nil {
		return newRuntimeReferenceError(nil)
	}
	return nil
}

func nilRuntimeReferenceInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type runtimeReferenceError struct {
	cause error
}

func newRuntimeReferenceError(cause error) error {
	return &runtimeReferenceError{cause: cause}
}

func (*runtimeReferenceError) Error() string {
	return credentials.CodeCredentialUnavailable
}

func (*runtimeReferenceError) Code() string {
	return credentials.CodeCredentialUnavailable
}

func (e *runtimeReferenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *runtimeReferenceError) Is(target error) bool {
	if e == nil {
		return false
	}
	coded, ok := target.(credentials.CodedError)
	return ok &&
		!nilRuntimeReferenceInterface(coded) &&
		coded.Code() == credentials.CodeCredentialUnavailable
}
