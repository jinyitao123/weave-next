package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const keyPrefix = "wv_sk_"

// APIKey represents a stored API key record (never contains the raw key).
type APIKey struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Scopes    []string   `json:"scopes,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Store provides CRUD operations on the weave_api_keys table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new API key store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create generates a new API key and stores its hash. Returns the raw key (shown only once).
func (s *Store) Create(ctx context.Context, tenantID, name, role, createdBy string, scopes []string, expiresAt *time.Time) (*APIKey, string, error) {
	rawKey, err := generateKey()
	if err != nil {
		return nil, "", err
	}
	hash := hashKey(rawKey)
	id := uuid.NewString()
	now := time.Now()

	_, err = s.pool.Exec(ctx,
		`INSERT INTO weave_api_keys (id, tenant_id, name, key_hash, role, scopes, created_by, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, tenantID, name, hash, role, scopes, createdBy, expiresAt, now)
	if err != nil {
		return nil, "", fmt.Errorf("create api key: %w", err)
	}

	key := &APIKey{
		ID: id, TenantID: tenantID, Name: name, Role: role, Scopes: scopes,
		CreatedBy: createdBy, ExpiresAt: expiresAt, CreatedAt: now,
	}
	return key, rawKey, nil
}

// Validate checks a raw API key against stored hashes and returns the matching record.
func (s *Store) Validate(ctx context.Context, rawKey string) (*APIKey, error) {
	hash := hashKey(rawKey)
	var k APIKey
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, name, role, scopes, created_by, expires_at, last_used, created_at
		 FROM weave_api_keys WHERE key_hash=$1`, hash,
	).Scan(&k.ID, &k.TenantID, &k.Name, &k.Role, &k.Scopes, &k.CreatedBy, &k.ExpiresAt, &k.LastUsed, &k.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid api key")
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("api key expired")
	}
	return &k, nil
}

// TouchLastUsed updates the last_used timestamp (fire-and-forget).
func (s *Store) TouchLastUsed(ctx context.Context, id string) {
	_, _ = s.pool.Exec(ctx, `UPDATE weave_api_keys SET last_used=NOW() WHERE id=$1`, id)
}

// List returns all API keys for a tenant.
func (s *Store) List(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, name, role, scopes, created_by, expires_at, last_used, created_at
		 FROM weave_api_keys WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Name, &k.Role, &k.Scopes, &k.CreatedBy, &k.ExpiresAt, &k.LastUsed, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Delete removes an API key by tenant and ID.
func (s *Store) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM weave_api_keys WHERE id=$1 AND tenant_id=$2`, id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("api key %q not found", id)
	}
	return nil
}

// generateKey creates a random API key with the wv_sk_ prefix.
func generateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return keyPrefix + hex.EncodeToString(b), nil
}

// hashKey returns the sha256 hex digest of a raw key.
func hashKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}
