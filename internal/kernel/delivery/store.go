// Package delivery persists workspace-scoped, revisioned delivery targets.
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

const (
	CodeInvalidRequest         = "workflow_delivery_invalid_request"
	CodeTargetNotFound         = "workflow_delivery_target_not_found"
	CodeTargetClosed           = "workflow_delivery_target_closed"
	CodeCredentialUnavailable  = "workflow_credential_unavailable"
	CodeFrozenManifestMismatch = "workflow_frozen_manifest_mismatch"
)

var (
	ErrInvalidRequest         = &Error{code: CodeInvalidRequest}
	ErrTargetNotFound         = &Error{code: CodeTargetNotFound}
	ErrTargetClosed           = &Error{code: CodeTargetClosed}
	ErrCredentialUnavailable  = &Error{code: CodeCredentialUnavailable}
	ErrFrozenManifestMismatch = &Error{code: CodeFrozenManifestMismatch}
)

// Kind identifies the caller-facing purpose of a delivery target.
type Kind string

const (
	KindCallback Kind = "callback"
	KindTarget   Kind = "target"
)

// Config is full replacement input for a delivery target revision.
type Config struct {
	Kind           Kind              `json:"kind"`
	URL            string            `json:"url"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	Headers        map[string]string `json:"headers"`
}

// Target is the secretless mutable head of a delivery target.
type Target struct {
	WorkspaceID    string     `json:"workspace_id"`
	ID             string     `json:"id"`
	LatestRevision int64      `json:"latest_revision"`
	Enabled        bool       `json:"enabled"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Revision is an immutable, secretless functional configuration.
type Revision struct {
	WorkspaceID    string    `json:"workspace_id"`
	TargetID       string    `json:"target_id"`
	Revision       int64     `json:"revision"`
	Kind           Kind      `json:"kind"`
	Transport      string    `json:"transport"`
	URL            string    `json:"url"`
	Method         string    `json:"method"`
	ContentType    string    `json:"content_type"`
	TimeoutSeconds int64     `json:"timeout_seconds"`
	HeaderNames    []string  `json:"header_names"`
	ContentHash    string    `json:"content_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

// MutationResult reports the current head and immutable revision.
type MutationResult struct {
	Target   Target   `json:"target"`
	Revision Revision `json:"revision"`
	Advanced bool     `json:"advanced"`
}

// Error carries a stable workflow error code without hiding infrastructure
// errors behind a business classification.
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

// Store persists delivery targets and immutable functional revisions.
type Store struct {
	pool *pgxpool.Pool
	key  []byte
}

// New creates a delivery target store.
func New(pool *pgxpool.Pool, key []byte) *Store {
	return &Store{pool: pool, key: append([]byte(nil), key...)}
}

// Create atomically creates a target head and revision one.
func (s *Store) Create(
	ctx context.Context,
	workspaceID, targetID string,
	cfg Config,
) (MutationResult, error) {
	normalized, headerNames, err := normalizeConfig(workspaceID, targetID, cfg)
	if err != nil {
		return MutationResult{}, err
	}
	headerJSON, err := canonicalHeaderJSON(normalized.Headers)
	if err != nil {
		return MutationResult{}, coded(ErrInvalidRequest, "canonicalize delivery target headers")
	}
	ciphertext, err := secret.Seal(s.key, headerJSON)
	if err != nil {
		return MutationResult{}, coded(ErrCredentialUnavailable, "seal delivery target headers")
	}
	revision, err := newRevision(workspaceID, targetID, 1, normalized, headerNames)
	if err != nil {
		return MutationResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, fmt.Errorf("begin delivery target create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var target Target
	err = tx.QueryRow(ctx, `
		INSERT INTO weave_delivery_targets (
		  workspace_id, id, headers_cipher
		) VALUES ($1, $2, $3)
		RETURNING workspace_id, id, latest_revision, enabled,
		          revoked_at, deleted_at, created_at, updated_at
	`, workspaceID, targetID, ciphertext).Scan(
		&target.WorkspaceID, &target.ID, &target.LatestRevision, &target.Enabled,
		&target.RevokedAt, &target.DeletedAt, &target.CreatedAt, &target.UpdatedAt,
	)
	if err != nil {
		return MutationResult{}, fmt.Errorf("insert delivery target head: %w", err)
	}
	if err := insertRevision(ctx, tx, &revision); err != nil {
		return MutationResult{}, err
	}
	err = tx.QueryRow(ctx, `
		UPDATE weave_delivery_targets
		SET latest_revision=1, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		RETURNING latest_revision, updated_at
	`, workspaceID, targetID).Scan(&target.LatestRevision, &target.UpdatedAt)
	if err != nil {
		return MutationResult{}, fmt.Errorf("advance delivery target head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, fmt.Errorf("commit delivery target create: %w", err)
	}
	return MutationResult{Target: target, Revision: revision, Advanced: true}, nil
}

// Update creates an immutable revision only when functional fields change.
// Header values are intentionally ignored on a functional no-op; callers must
// use RotateHeaders to rotate secret material without changing the manifest.
func (s *Store) Update(
	ctx context.Context,
	workspaceID, targetID string,
	cfg Config,
) (MutationResult, error) {
	normalized, headerNames, err := normalizeConfig(workspaceID, targetID, cfg)
	if err != nil {
		return MutationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, fmt.Errorf("begin delivery target update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, _, err := getTarget(ctx, tx, workspaceID, targetID, true)
	if err != nil {
		return MutationResult{}, err
	}
	if !target.Enabled || target.RevokedAt != nil || target.DeletedAt != nil {
		return MutationResult{}, coded(
			ErrTargetClosed,
			"closed delivery target cannot be updated",
		)
	}
	current, err := getRevision(ctx, tx, workspaceID, targetID, target.LatestRevision)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Kind == normalized.Kind && current.URL == normalized.URL &&
		current.TimeoutSeconds == normalized.TimeoutSeconds &&
		slices.Equal(current.HeaderNames, headerNames) {
		if err := tx.Commit(ctx); err != nil {
			return MutationResult{}, fmt.Errorf("commit delivery target no-op: %w", err)
		}
		return MutationResult{Target: target, Revision: current}, nil
	}
	if target.LatestRevision >= frozen.MaxJCSSafeInteger {
		return MutationResult{}, coded(
			ErrInvalidRequest,
			"delivery target revision exceeds the JCS safe integer range",
		)
	}
	headerJSON, err := canonicalHeaderJSON(normalized.Headers)
	if err != nil {
		return MutationResult{}, coded(ErrInvalidRequest, "canonicalize delivery target headers")
	}
	ciphertext, err := secret.Seal(s.key, headerJSON)
	if err != nil {
		return MutationResult{}, coded(ErrCredentialUnavailable, "seal delivery target headers")
	}
	next, err := newRevision(
		workspaceID, targetID, target.LatestRevision+1, normalized, headerNames,
	)
	if err != nil {
		return MutationResult{}, err
	}
	if err := insertRevision(ctx, tx, &next); err != nil {
		return MutationResult{}, err
	}
	err = tx.QueryRow(ctx, `
		UPDATE weave_delivery_targets
		SET latest_revision=$3, headers_cipher=$4, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		RETURNING latest_revision, updated_at
	`, workspaceID, targetID, next.Revision, ciphertext).Scan(
		&target.LatestRevision, &target.UpdatedAt,
	)
	if err != nil {
		return MutationResult{}, fmt.Errorf("advance delivery target head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, fmt.Errorf("commit delivery target update: %w", err)
	}
	return MutationResult{Target: target, Revision: next, Advanced: true}, nil
}

// RotateHeaders replaces only secret header values. The header-name set is a
// functional field and therefore must exactly match the latest revision.
func (s *Store) RotateHeaders(
	ctx context.Context,
	workspaceID, targetID string,
	headers map[string]string,
) (MutationResult, error) {
	if workspaceID == "" || targetID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		targetID != strings.TrimSpace(targetID) {
		return MutationResult{}, coded(
			ErrInvalidRequest,
			"exact workspace and delivery target identity are required",
		)
	}
	normalizedHeaders, headerNames, err := normalizeHeaders(headers)
	if err != nil {
		return MutationResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MutationResult{}, fmt.Errorf("begin delivery header rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, currentCipher, err := getTarget(ctx, tx, workspaceID, targetID, true)
	if err != nil {
		return MutationResult{}, err
	}
	if !target.Enabled || target.RevokedAt != nil || target.DeletedAt != nil {
		return MutationResult{}, coded(
			ErrTargetClosed,
			"closed delivery target headers cannot be rotated",
		)
	}
	revision, err := getRevision(
		ctx, tx, workspaceID, targetID, target.LatestRevision,
	)
	if err != nil {
		return MutationResult{}, err
	}
	if !slices.Equal(revision.HeaderNames, headerNames) {
		return MutationResult{}, coded(
			ErrInvalidRequest,
			"delivery header rotation cannot change the header-name set",
		)
	}
	canonical, err := canonicalHeaderJSON(normalizedHeaders)
	if err != nil {
		return MutationResult{}, coded(ErrInvalidRequest, "canonicalize delivery target headers")
	}
	current, err := secret.Open(s.key, currentCipher)
	if err != nil {
		return MutationResult{}, coded(
			ErrCredentialUnavailable,
			"open current delivery target headers",
		)
	}
	if bytes.Equal(current, canonical) {
		if err := tx.Commit(ctx); err != nil {
			return MutationResult{}, fmt.Errorf("commit delivery header no-op: %w", err)
		}
		return MutationResult{Target: target, Revision: revision}, nil
	}
	ciphertext, err := secret.Seal(s.key, canonical)
	if err != nil {
		return MutationResult{}, coded(
			ErrCredentialUnavailable,
			"seal replacement delivery target headers",
		)
	}
	err = tx.QueryRow(ctx, `
		UPDATE weave_delivery_targets
		SET headers_cipher=$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		RETURNING updated_at
	`, workspaceID, targetID, ciphertext).Scan(&target.UpdatedAt)
	if err != nil {
		return MutationResult{}, fmt.Errorf("rotate delivery target headers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MutationResult{}, fmt.Errorf("commit delivery header rotation: %w", err)
	}
	return MutationResult{Target: target, Revision: revision}, nil
}

// Get returns an open delivery target without secret material.
func (s *Store) Get(
	ctx context.Context,
	workspaceID, targetID string,
) (Target, error) {
	if err := validateIdentity(workspaceID, targetID); err != nil {
		return Target{}, err
	}
	target, _, err := getTarget(ctx, s.pool, workspaceID, targetID, false)
	if err != nil {
		return Target{}, err
	}
	if !target.Enabled || target.RevokedAt != nil || target.DeletedAt != nil {
		return Target{}, coded(
			ErrTargetNotFound,
			"delivery target is unavailable in this workspace",
		)
	}
	return target, nil
}

// List returns all open delivery targets in one workspace.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Target, error) {
	if workspaceID == "" || workspaceID != strings.TrimSpace(workspaceID) {
		return nil, coded(ErrInvalidRequest, "exact workspace identity is required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, id, latest_revision, enabled,
		       revoked_at, deleted_at, created_at, updated_at
		FROM weave_delivery_targets
		WHERE workspace_id=$1 AND enabled
		  AND revoked_at IS NULL AND deleted_at IS NULL
		ORDER BY id COLLATE "C"
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list delivery targets: %w", err)
	}
	defer rows.Close()
	targets := make([]Target, 0)
	for rows.Next() {
		var target Target
		if err := rows.Scan(
			&target.WorkspaceID, &target.ID, &target.LatestRevision,
			&target.Enabled, &target.RevokedAt, &target.DeletedAt,
			&target.CreatedAt, &target.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan delivery target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read delivery targets: %w", err)
	}
	return targets, nil
}

// GetRevision returns one immutable historical revision without secrets.
func (s *Store) GetRevision(
	ctx context.Context,
	workspaceID, targetID string,
	revisionNumber int64,
) (Revision, error) {
	if err := validateIdentity(workspaceID, targetID); err != nil {
		return Revision{}, err
	}
	if revisionNumber < 1 || revisionNumber > frozen.MaxJCSSafeInteger {
		return Revision{}, coded(
			ErrInvalidRequest,
			"delivery target revision is outside the JCS safe integer range",
		)
	}
	return getRevision(ctx, s.pool, workspaceID, targetID, revisionNumber)
}

// Disable permanently disables a delivery target without changing revision.
func (s *Store) Disable(ctx context.Context, workspaceID, targetID string) error {
	if err := validateIdentity(workspaceID, targetID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_delivery_targets
		SET enabled=false, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		  AND enabled AND revoked_at IS NULL AND deleted_at IS NULL
	`, workspaceID, targetID)
	if err != nil {
		return fmt.Errorf("disable delivery target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.requireTargetExists(ctx, workspaceID, targetID)
	}
	return nil
}

// Revoke permanently revokes delivery access without changing revision.
func (s *Store) Revoke(ctx context.Context, workspaceID, targetID string) error {
	if err := validateIdentity(workspaceID, targetID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_delivery_targets
		SET enabled=false, revoked_at=now(), updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		  AND revoked_at IS NULL AND deleted_at IS NULL
	`, workspaceID, targetID)
	if err != nil {
		return fmt.Errorf("revoke delivery target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.requireTargetExists(ctx, workspaceID, targetID)
	}
	return nil
}

// Delete idempotently soft-closes delivery access and preserves revisions.
func (s *Store) Delete(ctx context.Context, workspaceID, targetID string) error {
	if err := validateIdentity(workspaceID, targetID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_delivery_targets
		SET enabled=false,
		    revoked_at=COALESCE(revoked_at, now()),
		    deleted_at=now(),
		    updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL
	`, workspaceID, targetID)
	if err != nil {
		return fmt.Errorf("delete delivery target: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.requireTargetExists(ctx, workspaceID, targetID)
	}
	return nil
}

func validateIdentity(workspaceID, targetID string) error {
	if workspaceID == "" || targetID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		targetID != strings.TrimSpace(targetID) {
		return coded(
			ErrInvalidRequest,
			"exact workspace and delivery target identity are required",
		)
	}
	return nil
}

func (s *Store) requireTargetExists(
	ctx context.Context,
	workspaceID, targetID string,
) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM weave_delivery_targets
		  WHERE workspace_id=$1 AND id=$2
		)
	`, workspaceID, targetID).Scan(&exists); err != nil {
		return fmt.Errorf("check delivery target existence: %w", err)
	}
	if !exists {
		return coded(
			ErrTargetNotFound,
			"delivery target is unavailable in this workspace",
		)
	}
	return nil
}

func normalizeConfig(
	workspaceID, targetID string,
	cfg Config,
) (Config, []string, error) {
	if workspaceID == "" || targetID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		targetID != strings.TrimSpace(targetID) {
		return Config{}, nil, coded(
			ErrInvalidRequest,
			"exact workspace and delivery target identity are required",
		)
	}
	if cfg.Kind != KindCallback && cfg.Kind != KindTarget {
		return Config{}, nil, coded(ErrInvalidRequest, "delivery target kind is invalid")
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 300 {
		return Config{}, nil, coded(
			ErrInvalidRequest,
			"delivery target timeout must be between 1 and 300 seconds",
		)
	}
	if err := validateURL(cfg.URL); err != nil {
		return Config{}, nil, err
	}

	headers, names, err := normalizeHeaders(cfg.Headers)
	if err != nil {
		return Config{}, nil, err
	}
	cfg.Headers = headers
	return cfg, names, nil
}

func normalizeHeaders(input map[string]string) (map[string]string, []string, error) {
	headers := make(map[string]string, len(input))
	names := make([]string, 0, len(input))
	for name, value := range input {
		if !validASCIIHTTPToken(name) {
			return nil, nil, coded(
				ErrInvalidRequest,
				"delivery target header names must be ASCII HTTP tokens",
			)
		}
		canonicalName := lowercaseASCII(name)
		if _, exists := headers[canonicalName]; exists {
			return nil, nil, coded(
				ErrInvalidRequest,
				"delivery target header names collide after lowercase normalization",
			)
		}
		headers[canonicalName] = value
		names = append(names, canonicalName)
	}
	sort.Strings(names)
	return headers, names, nil
}

func canonicalHeaderJSON(headers map[string]string) ([]byte, error) {
	raw, err := json.Marshal(headers)
	if err != nil {
		return nil, err
	}
	return frozen.CanonicalizeJSON(raw)
}

func validateURL(raw string) error {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return coded(ErrInvalidRequest, "delivery target URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" {
		return coded(
			ErrInvalidRequest,
			"delivery target URL must be absolute HTTP(S) without userinfo, query, or fragment",
		)
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return coded(ErrInvalidRequest, "delivery target URL port is invalid")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return coded(ErrInvalidRequest, "delivery target URL port is invalid")
		}
	}
	return nil
}

func validASCIIHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func lowercaseASCII(value string) string {
	folded := []byte(value)
	for index, char := range folded {
		if char >= 'A' && char <= 'Z' {
			folded[index] = char + ('a' - 'A')
		}
	}
	return string(folded)
}

func newRevision(
	workspaceID, targetID string,
	revisionNumber int64,
	cfg Config,
	headerNames []string,
) (Revision, error) {
	revision := Revision{
		WorkspaceID:    workspaceID,
		TargetID:       targetID,
		Revision:       revisionNumber,
		Kind:           cfg.Kind,
		Transport:      "http",
		URL:            cfg.URL,
		Method:         "POST",
		ContentType:    "application/json",
		TimeoutSeconds: cfg.TimeoutSeconds,
		HeaderNames:    append([]string{}, headerNames...),
	}
	hash, err := frozen.HashDTO(
		frozenTarget(revision),
		frozen.PreorderFrozenDeliveryTarget,
	)
	if err != nil {
		return Revision{}, coded(
			ErrFrozenManifestMismatch,
			"hash delivery target functional configuration",
		)
	}
	revision.ContentHash = hash
	return revision, nil
}

func frozenTarget(revision Revision) frozen.FrozenDeliveryTarget {
	bindings := make([]frozen.FrozenDeliveryCredentialBinding, 0, len(revision.HeaderNames))
	for _, name := range revision.HeaderNames {
		bindings = append(bindings, frozen.FrozenDeliveryCredentialBinding{
			HeaderName: name,
			CredentialRef: frozen.CredentialReference{
				Scope: frozen.CredentialScopeWorkspaceService, ServiceID: "delivery:" + revision.TargetID,
				SchemaVersion: frozen.FrozenSchemaVersion,
				WorkspaceID:   revision.WorkspaceID,
				Kind:          frozen.CredentialDeliveryTargetAccess,
				ResourceID:    revision.TargetID,
				Slot:          "header:" + name,
			},
		})
	}
	return frozen.FrozenDeliveryTarget{
		SchemaVersion:      frozen.FrozenSchemaVersion,
		WorkspaceID:        revision.WorkspaceID,
		TargetID:           revision.TargetID,
		TargetRevision:     revision.Revision,
		Kind:               string(revision.Kind),
		Transport:          revision.Transport,
		URL:                revision.URL,
		Method:             revision.Method,
		ContentType:        revision.ContentType,
		TimeoutSeconds:     revision.TimeoutSeconds,
		CredentialBindings: bindings,
		AccessRef: frozen.CredentialReference{
			Scope: frozen.CredentialScopeWorkspaceService, ServiceID: "delivery:" + revision.TargetID,
			SchemaVersion: frozen.FrozenSchemaVersion,
			WorkspaceID:   revision.WorkspaceID,
			Kind:          frozen.CredentialDeliveryTargetAccess,
			ResourceID:    revision.TargetID,
			Slot:          "access",
		},
	}
}

func insertRevision(ctx context.Context, tx pgx.Tx, revision *Revision) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO weave_delivery_target_revisions (
		  workspace_id, target_id, revision, kind, transport, url,
		  method, content_type, timeout_seconds, header_names, content_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at
	`, revision.WorkspaceID, revision.TargetID, revision.Revision,
		string(revision.Kind), revision.Transport, revision.URL, revision.Method,
		revision.ContentType, revision.TimeoutSeconds, revision.HeaderNames,
		revision.ContentHash,
	).Scan(&revision.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert delivery target revision: %w", err)
	}
	return nil
}

type deliveryQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getTarget(
	ctx context.Context,
	querier deliveryQuerier,
	workspaceID, targetID string,
	forUpdate bool,
) (Target, string, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var target Target
	var ciphertext string
	err := querier.QueryRow(ctx, `
		SELECT workspace_id, id, latest_revision, headers_cipher, enabled,
		       revoked_at, deleted_at, created_at, updated_at
		FROM weave_delivery_targets
		WHERE workspace_id=$1 AND id=$2`+lock,
		workspaceID, targetID,
	).Scan(
		&target.WorkspaceID, &target.ID, &target.LatestRevision, &ciphertext,
		&target.Enabled, &target.RevokedAt, &target.DeletedAt,
		&target.CreatedAt, &target.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, "", coded(
			ErrTargetNotFound,
			"delivery target is unavailable in this workspace",
		)
	}
	if err != nil {
		return Target{}, "", fmt.Errorf("read delivery target head: %w", err)
	}
	return target, ciphertext, nil
}

func getRevision(
	ctx context.Context,
	querier deliveryQuerier,
	workspaceID, targetID string,
	revisionNumber int64,
) (Revision, error) {
	var revision Revision
	err := querier.QueryRow(ctx, `
		SELECT workspace_id, target_id, revision, kind, transport, url,
		       method, content_type, timeout_seconds, header_names,
		       content_hash, created_at
		FROM weave_delivery_target_revisions
		WHERE workspace_id=$1 AND target_id=$2 AND revision=$3
	`, workspaceID, targetID, revisionNumber).Scan(
		&revision.WorkspaceID, &revision.TargetID, &revision.Revision,
		&revision.Kind, &revision.Transport, &revision.URL, &revision.Method,
		&revision.ContentType, &revision.TimeoutSeconds, &revision.HeaderNames,
		&revision.ContentHash, &revision.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, coded(
			ErrTargetNotFound,
			"exact delivery target revision is unavailable",
		)
	}
	if err != nil {
		return Revision{}, fmt.Errorf("read delivery target revision: %w", err)
	}
	if revision.Transport != "http" || revision.Method != "POST" ||
		revision.ContentType != "application/json" {
		return Revision{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target transport contract is invalid",
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
		return Revision{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target functional configuration is invalid",
		)
	}
	wantHash, err := frozen.HashDTO(
		frozenTarget(revision),
		frozen.PreorderFrozenDeliveryTarget,
	)
	if err != nil || wantHash != revision.ContentHash {
		return Revision{}, coded(
			ErrFrozenManifestMismatch,
			"stored delivery target content hash does not match",
		)
	}
	return revision, nil
}
