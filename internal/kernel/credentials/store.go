// Package credentials persists workspace-scoped encrypted model credentials.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// MaskedKey is returned by credential APIs instead of secret material.
const MaskedKey = "••••••••"

// ErrDecrypt marks credential rows whose ciphertext cannot be opened with the
// current WEAVE_SECRET_KEY (rotated/lost key or a corrupt row).
var ErrDecrypt = errors.New("credential decrypt failed")

// Store persists credentials encrypted with a process-provided key.
type Store struct {
	pool *pgxpool.Pool
	key  []byte
}

// New creates a credential store.
func New(pool *pgxpool.Pool, key []byte) *Store {
	return &Store{pool: pool, key: append([]byte(nil), key...)}
}

// List returns decrypted provider configurations for one workspace.
func (s *Store) List(ctx context.Context, workspaceID string) ([]llmrouter.ProviderConfig, error) {
	if _, err := execution.RequireSubject(ctx, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, base_url, api_key_cipher, models, json_object_mode,credential_scope,credential_user_id,credential_service_id,protocol
		FROM weave_provider_credentials
		WHERE workspace_id=$1
		  AND enabled
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		ORDER BY id
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	configs := make([]llmrouter.ProviderConfig, 0)
	for rows.Next() {
		cfg, err := s.scanProvider(ctx, workspaceID, rows)
		if err != nil {
			if errors.Is(err, ErrCredentialUnavailable) {
				continue
			}
			if errors.Is(err, ErrDecrypt) {
				// One undecryptable row (rotated WEAVE_SECRET_KEY, corrupt
				// ciphertext) must not take the workspace's other providers —
				// and the env-configured models — down with it: skip the row
				// and keep serving.
				slog.Warn("skipping provider credential that cannot be decrypted",
					"workspace", workspaceID, "error", err)
				continue
			}
			return nil, err
		}
		configs = append(configs, cfg)
	}
	return configs, rows.Err()
}

func (s *Store) Upsert(ctx context.Context, workspaceID string, cfg llmrouter.ProviderConfig) error {
	_, err := s.UpsertRevision(ctx, workspaceID, cfg)
	return err
}

// Delete idempotently closes a provider's live security state. Immutable
// functional revisions remain available for frozen publication and audit.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	if _, err := s.GetHead(ctx, workspaceID, id); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_provider_credentials
		SET enabled=false,
		    revoked_at=COALESCE(revoked_at, now()),
		    deleted_at=COALESCE(deleted_at, now()),
		    updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanProvider(ctx context.Context, workspaceID string, row rowScanner) (llmrouter.ProviderConfig, error) {
	var cfg llmrouter.ProviderConfig
	var ciphertext string
	if err := row.Scan(&cfg.ID, &cfg.Name, &cfg.BaseURL, &ciphertext, &cfg.Models, &cfg.JSONObjectMode, &cfg.CredentialScope, &cfg.CredentialUserID, &cfg.CredentialServiceID, &cfg.Protocol); err != nil {
		return llmrouter.ProviderConfig{}, err
	}
	if err := AuthorizeReference(ctx, providerConfigReference(workspaceID, cfg)); err != nil {
		return llmrouter.ProviderConfig{}, err
	}
	plaintext, err := secret.Open(s.key, ciphertext)
	if err != nil {
		return llmrouter.ProviderConfig{}, fmt.Errorf("%w: provider %q: %v", ErrDecrypt, cfg.ID, err)
	}
	cfg.APIKey = string(plaintext)
	return cfg, nil
}
