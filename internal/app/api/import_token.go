package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	importPreviewTokenVersion = 1
	// Preview confirmation is deliberately short-lived to limit replay risk.
	importPreviewTokenTTL   = 15 * time.Minute
	importPreviewKeyPurpose = "import-preview-v1:"
)

type importTokenConflict struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Field order is part of the token schema: encoding/json emits struct fields
// deterministically in declaration order.
type importTokenPayload struct {
	Version       int                  `json:"version"`
	ID            string               `json:"id"`
	Tenant        string               `json:"tenant"`
	ArchiveSHA256 string               `json:"archive_sha256"`
	Name          string               `json:"name"`
	Model         string               `json:"model"`
	Conflict      *importTokenConflict `json:"conflict,omitempty"`
	ExpiresAt     time.Time            `json:"expires_at"`
}

type importTokenError struct {
	Status  int
	Message string
}

func (e *importTokenError) Error() string { return e.Message }

type importTokenPoolProvider interface {
	Pool() *pgxpool.Pool
}

func (s *Server) importTokenPool() (*pgxpool.Pool, error) {
	provider, ok := s.Store.(importTokenPoolProvider)
	if !ok || provider.Pool() == nil {
		return nil, errors.New("import preview tokens require PostgreSQL storage")
	}
	return provider.Pool(), nil
}

func (s *Server) issueImportPreviewToken(ctx context.Context, tenant, archiveSHA256, name, model string, conflict *importConflictPreview) (string, time.Time, error) {
	pool, err := s.importTokenPool()
	if err != nil {
		return "", time.Time{}, err
	}
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(importPreviewTokenTTL).Truncate(time.Microsecond)
	payload := importTokenPayload{
		Version: importPreviewTokenVersion,
		ID:      base64.RawURLEncoding.EncodeToString(idBytes), Tenant: tenant,
		ArchiveSHA256: archiveSHA256, Name: name, Model: model, ExpiresAt: expiresAt,
	}
	if conflict != nil {
		payload.Conflict = &importTokenConflict{Name: conflict.Name, Version: conflict.CurrentVersion}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, err
	}
	payloadHash := sha256.Sum256(payloadJSON)
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name) VALUES ($1, $1, $1)
		ON CONFLICT DO NOTHING
	`, tenant); err != nil {
		return "", time.Time{}, err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_import_preview_tokens (id, workspace_id, archive_sha256, payload_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, payload.ID, tenant, archiveSHA256, hex.EncodeToString(payloadHash[:]), expiresAt); err != nil {
		return "", time.Time{}, err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signature := signImportPayload(s.Config.JWTSecret, payloadJSON)
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature), expiresAt, nil
}

func signImportPayload(secret string, payload []byte) []byte {
	derive := hmac.New(sha256.New, []byte(secret))
	_, _ = derive.Write([]byte(importPreviewKeyPurpose))
	key := derive.Sum(nil)
	sign := hmac.New(sha256.New, key)
	_, _ = sign.Write(payload)
	return sign.Sum(nil)
}

func (s *Server) parseImportPreviewToken(raw string) (*importTokenPayload, []byte, *importTokenError) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return nil, nil, &importTokenError{http.StatusBadRequest, "invalid preview_token; run import preview again"}
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, &importTokenError{http.StatusBadRequest, "invalid preview_token; run import preview again"}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, signImportPayload(s.Config.JWTSecret, payloadJSON)) {
		return nil, nil, &importTokenError{http.StatusBadRequest, "invalid preview_token; run import preview again"}
	}
	var payload importTokenPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil || payload.Version != importPreviewTokenVersion || payload.ID == "" {
		return nil, nil, &importTokenError{http.StatusBadRequest, "invalid preview_token; run import preview again"}
	}
	return &payload, payloadJSON, nil
}

func validateImportPayload(payload *importTokenPayload, tenant, archiveSHA256, name, model string) *importTokenError {
	expected := *payload
	expected.Tenant = tenant
	expected.ArchiveSHA256 = archiveSHA256
	expected.Name = name
	expected.Model = model
	want, err := json.Marshal(expected)
	if err != nil {
		return &importTokenError{http.StatusInternalServerError, "cannot validate preview token"}
	}
	got, err := json.Marshal(payload)
	if err != nil || !hmac.Equal(got, want) {
		return &importTokenError{http.StatusBadRequest, "预检后内容已变更，请重新预检"}
	}
	return nil
}

func lockImportToken(ctx context.Context, tx pgx.Tx, payload *importTokenPayload, payloadJSON []byte, tenant string) *importTokenError {
	var workspaceID, archiveSHA256, payloadHash string
	var expiresAt time.Time
	var consumedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, archive_sha256, payload_hash, expires_at, consumed_at
		FROM weave_import_preview_tokens WHERE id=$1 FOR UPDATE
	`, payload.ID).Scan(&workspaceID, &archiveSHA256, &payloadHash, &expiresAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &importTokenError{http.StatusBadRequest, "preview_token not found; run import preview again"}
	}
	if err != nil {
		return &importTokenError{http.StatusInternalServerError, err.Error()}
	}
	if workspaceID != tenant || payload.Tenant != tenant {
		return &importTokenError{http.StatusBadRequest, "preview_token belongs to another workspace"}
	}
	if consumedAt != nil {
		return &importTokenError{http.StatusGone, "preview_token has already been consumed"}
	}
	if !time.Now().Before(expiresAt) || !time.Now().Before(payload.ExpiresAt) {
		return &importTokenError{http.StatusGone, "preview_token has expired; run import preview again"}
	}
	hash := sha256.Sum256(payloadJSON)
	if archiveSHA256 != payload.ArchiveSHA256 || payloadHash != hex.EncodeToString(hash[:]) {
		return &importTokenError{http.StatusBadRequest, "invalid preview_token; run import preview again"}
	}
	return nil
}

func consumeImportToken(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `UPDATE weave_import_preview_tokens SET consumed_at=now() WHERE id=$1 AND consumed_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("preview token was not consumable")
	}
	return nil
}
