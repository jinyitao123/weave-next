package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

const (
	CodeProviderRevisionRequired = "workflow_provider_revision_required"
	CodeCredentialUnavailable    = "workflow_credential_unavailable"
	CodeFrozenManifestMismatch   = "workflow_frozen_manifest_mismatch"
)

type Error struct {
	code   string
	detail string
}

var (
	ErrProviderRevisionRequired = &Error{code: CodeProviderRevisionRequired}
	ErrCredentialUnavailable    = &Error{code: CodeCredentialUnavailable}
	ErrFrozenManifestMismatch   = &Error{code: CodeFrozenManifestMismatch}
)

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

type ProviderHead struct {
	CredentialScope     frozen.CredentialScope `json:"credential_scope"`
	CredentialUserID    string                 `json:"credential_user_id,omitempty"`
	CredentialServiceID string                 `json:"credential_service_id,omitempty"`
	WorkspaceID         string                 `json:"workspace_id,omitempty"`
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	BaseURL             string                 `json:"base_url"`
	APIKey              string                 `json:"api_key,omitempty"`
	Models              []string               `json:"models"`
	JSONObjectMode      bool                   `json:"json_object_mode,omitempty"`
	LatestRevision      int64                  `json:"latest_revision"`
	SourceKind          string                 `json:"source_kind"`
	SourceProviderID    *string                `json:"source_provider_id,omitempty"`
	Enabled             bool                   `json:"enabled"`
	RevokedAt           *time.Time             `json:"revoked_at,omitempty"`
	DeletedAt           *time.Time             `json:"deleted_at,omitempty"`
}

type ProviderRevision struct {
	WorkspaceID    string    `json:"workspace_id"`
	ProviderID     string    `json:"provider_id"`
	Revision       int64     `json:"revision"`
	Name           string    `json:"name"`
	BaseURL        string    `json:"base_url"`
	Models         []string  `json:"models"`
	JSONObjectMode bool      `json:"json_object_mode"`
	ContentHash    string    `json:"content_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

type ProviderRevisionResult struct {
	Head     ProviderHead
	Revision ProviderRevision
	Advanced bool
}

type providerRevisionHashInput struct {
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	Models         []string `json:"models"`
	JSONObjectMode bool     `json:"json_object_mode"`
}

func (s *Store) UpsertRevision(
	ctx context.Context,
	workspaceID string,
	cfg llmrouter.ProviderConfig,
) (ProviderRevisionResult, error) {
	normalized, err := normalizeProviderConfig(workspaceID, cfg)
	if err != nil {
		return ProviderRevisionResult{}, err
	}
	if err := AuthorizeReference(ctx, providerConfigReference(workspaceID, normalized)); err != nil {
		return ProviderRevisionResult{}, err
	}
	contentHash, err := hashProviderRevision(normalized)
	if err != nil {
		return ProviderRevisionResult{}, coded(
			CodeFrozenManifestMismatch,
			"hash provider functional configuration",
		)
	}
	initialCipher, err := secret.Seal(s.key, nil)
	if err != nil {
		return ProviderRevisionResult{}, fmt.Errorf("seal empty provider credential: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProviderRevisionResult{}, fmt.Errorf("begin ProviderRevision upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_provider_credentials (
		  workspace_id, id, name, base_url, api_key_cipher, models,
		  json_object_mode,credential_scope,credential_user_id,credential_service_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7,$8,$9,$10)
		ON CONFLICT (workspace_id, id) DO NOTHING
	`, workspaceID, normalized.ID, normalized.Name, normalized.BaseURL,
		initialCipher, normalized.Models, normalized.JSONObjectMode, normalized.CredentialScope, normalized.CredentialUserID, normalized.CredentialServiceID,
	); err != nil {
		return ProviderRevisionResult{}, fmt.Errorf("create provider head: %w", err)
	}

	head, ciphertext, err := getProviderHeadTx(ctx, tx, workspaceID, normalized.ID, true)
	if err != nil {
		return ProviderRevisionResult{}, err
	}
	if head.CredentialScope != normalized.CredentialScope || head.CredentialUserID != normalized.CredentialUserID || head.CredentialServiceID != normalized.CredentialServiceID {
		return ProviderRevisionResult{}, coded(CodeCredentialUnavailable, "provider credential owner cannot change")
	}
	if !head.Enabled || head.RevokedAt != nil || head.DeletedAt != nil {
		return ProviderRevisionResult{}, coded(
			CodeCredentialUnavailable,
			"closed provider credentials cannot be revived by ordinary CRUD",
		)
	}
	if normalized.APIKey != "" && normalized.APIKey != MaskedKey {
		ciphertext, err = secret.Seal(s.key, []byte(normalized.APIKey))
		if err != nil {
			return ProviderRevisionResult{}, fmt.Errorf("seal provider credential: %w", err)
		}
	}

	var revision ProviderRevision
	advanced := head.LatestRevision == 0
	if head.LatestRevision > 0 {
		revision, err = getProviderRevisionTx(
			ctx, tx, workspaceID, normalized.ID, head.LatestRevision, true,
			CodeProviderRevisionRequired,
		)
		if err != nil {
			return ProviderRevisionResult{}, err
		}
		advanced = revision.ContentHash != contentHash
	}

	nextRevision := head.LatestRevision
	if advanced {
		if head.LatestRevision >= frozen.MaxJCSSafeInteger {
			return ProviderRevisionResult{}, coded(
				CodeProviderRevisionRequired,
				"provider revision exceeds the JCS safe integer range",
			)
		}
		nextRevision++
		revision = ProviderRevision{
			WorkspaceID:    workspaceID,
			ProviderID:     normalized.ID,
			Revision:       nextRevision,
			Name:           normalized.Name,
			BaseURL:        normalized.BaseURL,
			Models:         append([]string(nil), normalized.Models...),
			JSONObjectMode: normalized.JSONObjectMode,
			ContentHash:    contentHash,
		}
		modelsJSON, marshalErr := json.Marshal(revision.Models)
		if marshalErr != nil {
			return ProviderRevisionResult{}, coded(
				CodeFrozenManifestMismatch,
				"encode provider revision models",
			)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO weave_provider_revisions (
			  workspace_id, provider_id, revision, name, base_url, models,
			  json_object_mode, content_hash
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING created_at
		`, revision.WorkspaceID, revision.ProviderID, revision.Revision,
			revision.Name, revision.BaseURL, string(modelsJSON), revision.JSONObjectMode,
			revision.ContentHash,
		).Scan(&revision.CreatedAt)
		if err != nil {
			return ProviderRevisionResult{}, fmt.Errorf("insert ProviderRevision: %w", err)
		}
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
	`, workspaceID, normalized.ID, normalized.Name, normalized.BaseURL,
		ciphertext, normalized.Models, normalized.JSONObjectMode, nextRevision,
	); err != nil {
		return ProviderRevisionResult{}, fmt.Errorf("update provider head: %w", err)
	}

	head.Name = normalized.Name
	head.BaseURL = normalized.BaseURL
	head.APIKey = MaskedKey
	head.Models = append([]string(nil), normalized.Models...)
	head.JSONObjectMode = normalized.JSONObjectMode
	head.LatestRevision = nextRevision
	if err := tx.Commit(ctx); err != nil {
		return ProviderRevisionResult{}, fmt.Errorf("commit ProviderRevision upsert: %w", err)
	}
	return ProviderRevisionResult{
		Head:     head,
		Revision: revision,
		Advanced: advanced,
	}, nil
}

func (s *Store) GetHead(
	ctx context.Context,
	workspaceID, providerID string,
) (ProviderHead, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(providerID) == "" {
		return ProviderHead{}, coded(
			CodeProviderRevisionRequired,
			"exact workspace and provider identity are required",
		)
	}
	head, _, err := getProviderHeadTx(ctx, s.pool, workspaceID, providerID, false)
	return head, err
}

func (s *Store) ListMetadata(
	ctx context.Context,
	workspaceID string,
) ([]ProviderHead, error) {
	if _, err := execution.RequireSubject(ctx, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, id, name, base_url, models, json_object_mode,
		       latest_revision, source_kind, source_provider_id, enabled,
		       revoked_at, deleted_at,credential_scope,credential_user_id,credential_service_id
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

	heads := make([]ProviderHead, 0)
	for rows.Next() {
		var head ProviderHead
		if err := rows.Scan(
			&head.WorkspaceID, &head.ID, &head.Name, &head.BaseURL, &head.Models,
			&head.JSONObjectMode, &head.LatestRevision, &head.SourceKind,
			&head.SourceProviderID, &head.Enabled, &head.RevokedAt, &head.DeletedAt, &head.CredentialScope, &head.CredentialUserID, &head.CredentialServiceID,
		); err != nil {
			return nil, err
		}
		if AuthorizeReference(ctx, headReference(head)) != nil {
			continue
		}
		head.APIKey = MaskedKey
		heads = append(heads, head)
	}
	return heads, rows.Err()
}

func (s *Store) GetRevision(
	ctx context.Context,
	workspaceID, providerID string,
	revision int64,
) (ProviderRevision, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(providerID) == "" {
		return ProviderRevision{}, coded(
			CodeProviderRevisionRequired,
			"exact workspace and provider identity are required",
		)
	}
	if revision < 1 || revision > frozen.MaxJCSSafeInteger {
		return ProviderRevision{}, coded(
			CodeProviderRevisionRequired,
			"provider revision is outside the JCS safe integer range",
		)
	}
	if _, _, err := getProviderHeadTx(ctx, s.pool, workspaceID, providerID, false); err != nil {
		return ProviderRevision{}, err
	}
	return getProviderRevisionTx(
		ctx, s.pool, workspaceID, providerID, revision, false,
		CodeProviderRevisionRequired,
	)
}

// ResolveProviderRevisionTx locks the live provider head and one exact
// immutable functional revision using the caller-owned transaction. It never
// reads or returns provider credential ciphertext.
func (s *Store) ResolveProviderRevisionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, providerID string,
	dependencyVersion *int64,
) (ProviderRevision, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(providerID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		providerID != strings.TrimSpace(providerID) {
		return ProviderRevision{}, coded(
			CodeProviderRevisionRequired,
			"exact workspace and provider identity are required",
		)
	}
	if dependencyVersion == nil ||
		*dependencyVersion < 1 ||
		*dependencyVersion > frozen.MaxJCSSafeInteger {
		return ProviderRevision{}, coded(
			CodeDependencyVersionRequired,
			"exact provider dependency revision is required",
		)
	}

	if _, _, err := getProviderHeadTx(ctx, tx, workspaceID, providerID, false); err != nil {
		return ProviderRevision{}, err
	}
	var (
		storedWorkspaceID string
		storedProviderID  string
		latestRevision    int64
		sourceKind        string
		sourceProviderID  *string
		enabled           bool
		revokedAt         *time.Time
		deletedAt         *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, id, latest_revision, source_kind, source_provider_id,
		       enabled, revoked_at, deleted_at
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, providerID).Scan(
		&storedWorkspaceID,
		&storedProviderID,
		&latestRevision,
		&sourceKind,
		&sourceProviderID,
		&enabled,
		&revokedAt,
		&deletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if strings.HasPrefix(providerID, "system/") {
			return ProviderRevision{}, coded(
				CodeProviderRevisionRequired,
				"explicit system provider mirror is required",
			)
		}
		return ProviderRevision{}, coded(
			CodeCredentialUnavailable,
			"provider credential is unavailable",
		)
	}
	if err != nil {
		return ProviderRevision{}, fmt.Errorf("lock provider head: %w", err)
	}
	if storedWorkspaceID != workspaceID || storedProviderID != providerID {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider head identity does not match",
		)
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return ProviderRevision{}, coded(
			CodeCredentialUnavailable,
			"selected provider credential is closed",
		)
	}
	switch sourceKind {
	case "workspace":
		if sourceProviderID != nil || strings.HasPrefix(providerID, "system/") {
			return ProviderRevision{}, coded(
				CodeFrozenManifestMismatch,
				"stored workspace provider source identity is invalid",
			)
		}
	case "system_mirror":
		if sourceProviderID == nil ||
			strings.TrimSpace(*sourceProviderID) == "" ||
			providerID != "system/"+*sourceProviderID {
			return ProviderRevision{}, coded(
				CodeFrozenManifestMismatch,
				"stored system provider mirror identity is invalid",
			)
		}
	default:
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider source kind is invalid",
		)
	}
	if latestRevision < 1 ||
		latestRevision > frozen.MaxJCSSafeInteger ||
		*dependencyVersion > latestRevision {
		return ProviderRevision{}, coded(
			CodeDependencyVersionRequired,
			"exact ProviderRevision is unavailable",
		)
	}

	revision, err := getProviderRevisionTx(
		ctx,
		tx,
		workspaceID,
		providerID,
		*dependencyVersion,
		true,
		CodeDependencyVersionRequired,
	)
	if err != nil {
		return ProviderRevision{}, err
	}
	if revision.WorkspaceID != workspaceID ||
		revision.ProviderID != providerID ||
		revision.Revision != *dependencyVersion {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider revision identity does not match",
		)
	}
	return revision, nil
}

type providerQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getProviderHeadTx(
	ctx context.Context,
	querier providerQuerier,
	workspaceID, providerID string,
	forUpdate bool,
) (ProviderHead, string, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var head ProviderHead
	var ciphertext string
	err := querier.QueryRow(ctx, `
		SELECT workspace_id, id, name, base_url, api_key_cipher, models,
		       json_object_mode, latest_revision, source_kind, source_provider_id,
		       enabled, revoked_at, deleted_at,credential_scope,credential_user_id,credential_service_id
		FROM weave_provider_credentials
		WHERE workspace_id=$1 AND id=$2`+lock,
		workspaceID, providerID,
	).Scan(
		&head.WorkspaceID, &head.ID, &head.Name, &head.BaseURL, &ciphertext,
		&head.Models, &head.JSONObjectMode, &head.LatestRevision, &head.SourceKind,
		&head.SourceProviderID, &head.Enabled, &head.RevokedAt, &head.DeletedAt, &head.CredentialScope, &head.CredentialUserID, &head.CredentialServiceID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderHead{}, "", coded(
			CodeCredentialUnavailable,
			"provider credential is unavailable",
		)
	}
	if err != nil {
		return ProviderHead{}, "", fmt.Errorf("read provider head: %w", err)
	}
	if err := AuthorizeReference(ctx, headReference(head)); err != nil {
		return ProviderHead{}, "", err
	}
	head.APIKey = MaskedKey
	return head, ciphertext, nil
}

func getProviderRevisionTx(
	ctx context.Context,
	querier providerQuerier,
	workspaceID, providerID string,
	revision int64,
	forShare bool,
	missingCodes ...string,
) (ProviderRevision, error) {
	missingCode := CodeProviderRevisionRequired
	if len(missingCodes) > 0 {
		missingCode = missingCodes[0]
	}
	lock := ""
	if forShare {
		lock = " FOR SHARE"
	}
	if strings.HasPrefix(providerID, "system/") {
		var sourceKind string
		var sourceProviderID *string
		err := querier.QueryRow(ctx, `
			SELECT source_kind, source_provider_id
			FROM weave_provider_credentials
			WHERE workspace_id=$1 AND id=$2`+lock,
			workspaceID, providerID,
		).Scan(&sourceKind, &sourceProviderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ProviderRevision{}, coded(
				CodeProviderRevisionRequired,
				"reserved provider revision is not a valid system mirror",
			)
		}
		if err != nil {
			return ProviderRevision{}, fmt.Errorf("read mirrored provider source identity: %w", err)
		}
		if sourceKind != "system_mirror" || sourceProviderID == nil ||
			"system/"+*sourceProviderID != providerID {
			return ProviderRevision{}, coded(
				CodeProviderRevisionRequired,
				"reserved provider revision is not a valid system mirror",
			)
		}
	}
	var stored ProviderRevision
	var modelsJSON []byte
	err := querier.QueryRow(ctx, `
		SELECT workspace_id, provider_id, revision, name, base_url, models,
		       json_object_mode, content_hash, created_at
		FROM weave_provider_revisions
		WHERE workspace_id=$1 AND provider_id=$2 AND revision=$3`+lock,
		workspaceID, providerID, revision,
	).Scan(
		&stored.WorkspaceID, &stored.ProviderID, &stored.Revision, &stored.Name,
		&stored.BaseURL, &modelsJSON, &stored.JSONObjectMode, &stored.ContentHash,
		&stored.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderRevision{}, coded(
			missingCode,
			"exact ProviderRevision is unavailable",
		)
	}
	if err != nil {
		return ProviderRevision{}, fmt.Errorf("read ProviderRevision: %w", err)
	}
	if err := json.Unmarshal(modelsJSON, &stored.Models); err != nil || stored.Models == nil {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider models are invalid",
		)
	}
	storedConfig := llmrouter.ProviderConfig{
		ID:             providerID,
		Name:           stored.Name,
		BaseURL:        stored.BaseURL,
		Models:         stored.Models,
		JSONObjectMode: stored.JSONObjectMode,
	}
	var normalized llmrouter.ProviderConfig
	if strings.HasPrefix(providerID, "system/") {
		normalized, err = normalizeMirroredProviderConfig(workspaceID, storedConfig)
	} else {
		normalized, err = normalizeProviderConfig(workspaceID, storedConfig)
	}
	if err != nil {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider functional configuration is invalid",
		)
	}
	if len(normalized.Models) != len(stored.Models) {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider models are not normalized",
		)
	}
	for index := range normalized.Models {
		if normalized.Models[index] != stored.Models[index] {
			return ProviderRevision{}, coded(
				CodeFrozenManifestMismatch,
				"stored provider models are not normalized",
			)
		}
	}
	hash, err := hashProviderRevision(normalized)
	if err != nil || hash != stored.ContentHash {
		return ProviderRevision{}, coded(
			CodeFrozenManifestMismatch,
			"stored provider content hash does not match",
		)
	}
	return stored, nil
}

func normalizeProviderConfig(
	workspaceID string,
	cfg llmrouter.ProviderConfig,
) (llmrouter.ProviderConfig, error) {
	if strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(cfg.ID) == "" ||
		strings.TrimSpace(cfg.Name) == "" {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"workspace, provider, and name are required",
		)
	}
	if cfg.ID != strings.TrimSpace(cfg.ID) || strings.HasPrefix(cfg.ID, "system/") {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"provider identity is reserved or invalid",
		)
	}
	if cfg.BaseURL != strings.TrimSpace(cfg.BaseURL) {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"provider base URL is invalid",
		)
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"provider base URL must be absolute HTTP(S) without userinfo, query, or fragment",
		)
	}

	modelSet := make(map[string]struct{}, len(cfg.Models))
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model != "" {
			modelSet[model] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}
	sort.Strings(models)
	if len(models) == 0 {
		return llmrouter.ProviderConfig{}, coded(
			CodeProviderRevisionRequired,
			"at least one provider model is required",
		)
	}
	cfg.Models = models
	return cfg, nil
}

func hashProviderRevision(cfg llmrouter.ProviderConfig) (string, error) {
	raw, err := json.Marshal(providerRevisionHashInput{
		Name:           cfg.Name,
		BaseURL:        cfg.BaseURL,
		Models:         cfg.Models,
		JSONObjectMode: cfg.JSONObjectMode,
	})
	if err != nil {
		return "", err
	}
	return frozen.HashCanonicalJSON(raw)
}

func coded(code, detail string) *Error {
	return &Error{code: code, detail: detail}
}
