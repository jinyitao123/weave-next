package credentials

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

const providerReferenceUnavailableDetail = "provider credential is unavailable"

var _ TxReferenceSource = (*Store)(nil)

func (s *Store) ValidateReferenceTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := validateProviderReferenceSourceInput(s, tx, ref); err != nil {
		return err
	}

	var state providerReferenceState
	err := tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID).Scan(
		&state.enabled, &state.revokedAt, &state.deletedAt,
	)
	if err != nil {
		return classifyProviderReferenceReadError(err)
	}
	return validateProviderReferenceState(state)
}

func (s *Store) ResolveProviderAPIKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) (string, error) {
	if err := validateProviderReferenceSourceInput(s, tx, ref); err != nil {
		return "", err
	}

	var ciphertext string
	var state providerReferenceState
	err := tx.QueryRow(ctx, `
		SELECT api_key_cipher, enabled, revoked_at, deleted_at
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, ref.WorkspaceID, ref.ResourceID).Scan(
		&ciphertext, &state.enabled, &state.revokedAt, &state.deletedAt,
	)
	if err != nil {
		return "", classifyProviderReferenceReadError(err)
	}
	if err := validateProviderReferenceState(state); err != nil {
		return "", err
	}

	plaintext, err := secret.Open(s.key, ciphertext)
	if err != nil {
		return "", coded(CodeCredentialUnavailable, providerReferenceUnavailableDetail)
	}
	apiKey := string(plaintext)
	for index := range plaintext {
		plaintext[index] = 0
	}
	return apiKey, nil
}

type providerReferenceState struct {
	enabled   bool
	revokedAt *time.Time
	deletedAt *time.Time
}

func validateProviderReferenceSourceInput(
	store *Store,
	tx pgx.Tx,
	ref frozen.CredentialReference,
) error {
	if err := ValidateReferenceV1(ref); err != nil {
		return err
	}
	if ref.Kind != frozen.CredentialProviderAPIKey || ref.Slot != "api_key" {
		return coded(CodeCredentialUnavailable, providerReferenceUnavailableDetail)
	}
	if store == nil || store.pool == nil || nilInterface(tx) {
		return coded(CodeCredentialUnavailable, providerReferenceUnavailableDetail)
	}
	return nil
}

func classifyProviderReferenceReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return coded(CodeCredentialUnavailable, providerReferenceUnavailableDetail)
	}
	return wrapReferenceSourceError(err)
}

func validateProviderReferenceState(state providerReferenceState) error {
	if !state.enabled || state.revokedAt != nil || state.deletedAt != nil {
		return coded(CodeCredentialUnavailable, providerReferenceUnavailableDetail)
	}
	return nil
}
