package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const (
	CodeSkillImportLegacyNotFound      = "workflow_skill_import_legacy_not_found"
	CodeSkillImportInvalid             = "workflow_skill_import_invalid"
	CodeSkillImportSourceChanged       = "workflow_skill_import_source_changed"
	CodeSkillImportIdempotencyConflict = "workflow_skill_import_idempotency_conflict"
	CodeSkillImportUnavailable         = "workflow_skill_import_unavailable"
	CodeSkillImportFailed              = "workflow_skill_import_failed"
)

var (
	ErrLegacySkillNotFound            = errors.New("legacy Skill not found")
	ErrSkillImportLegacyNotFound      = &Error{code: CodeSkillImportLegacyNotFound}
	ErrSkillImportInvalid             = &Error{code: CodeSkillImportInvalid}
	ErrSkillImportSourceChanged       = &Error{code: CodeSkillImportSourceChanged}
	ErrSkillImportIdempotencyConflict = &Error{code: CodeSkillImportIdempotencyConflict}
	ErrSkillImportUnavailable         = &Error{code: CodeSkillImportUnavailable}
	ErrSkillImportFailed              = &Error{code: CodeSkillImportFailed}
)

type LegacyReader interface {
	Get(context.Context, string, string) ([]byte, error)
}

type ImportRequest struct {
	SchemaVersion      int     `json:"schema_version"`
	IdempotencyKey     string  `json:"idempotency_key"`
	Reason             string  `json:"reason"`
	ExpectedSourceHash *string `json:"expected_source_hash,omitempty"`
}

type ImportResponse struct {
	SchemaVersion int    `json:"schema_version"`
	SkillID       string `json:"skill_id"`
	Version       int64  `json:"version"`
	Changed       bool   `json:"changed"`
	SourceHash    string `json:"source_hash"`
	ContentHash   string `json:"content_hash"`
}

type Importer struct {
	pool     *pgxpool.Pool
	versions *Store
	legacy   LegacyReader
}

func NewImporter(pool *pgxpool.Pool, versions *Store, legacy LegacyReader) *Importer {
	return &Importer{pool: pool, versions: versions, legacy: legacy}
}

type pgLegacyReader struct {
	pool *pgxpool.Pool
}

func NewPGLegacyReader(pool *pgxpool.Pool) LegacyReader {
	return &pgLegacyReader{pool: pool}
}

func (r *pgLegacyReader) Get(ctx context.Context, namespace, key string) ([]byte, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("legacy Skill storage unavailable")
	}
	var value []byte
	err := r.pool.QueryRow(ctx, `
		SELECT value FROM loom_store WHERE namespace=$1 AND key=$2
	`, namespace, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLegacySkillNotFound
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}

func (i *Importer) Import(ctx context.Context, workspaceID, skillID, operatorID string, request ImportRequest) (*ImportResponse, error) {
	requestHash, err := hashImportRequest(workspaceID, skillID, operatorID, request)
	if err != nil {
		return nil, err
	}
	if i == nil || i.pool == nil || i.versions == nil || i.legacy == nil {
		return nil, coded(CodeSkillImportUnavailable, "import service is unavailable")
	}
	if response, found, err := i.readReceipt(ctx, i.pool, workspaceID, request.IdempotencyKey, requestHash); found || err != nil {
		return response, err
	}

	namespace := "skill:" + workspaceID
	raw, err := i.legacy.Get(ctx, namespace, skillID)
	if err != nil {
		if errors.Is(err, ErrLegacySkillNotFound) {
			return nil, coded(CodeSkillImportLegacyNotFound, "legacy Skill does not exist")
		}
		return nil, coded(CodeSkillImportUnavailable, "legacy Skill storage is unavailable")
	}
	sourceHash := sha256Hex(raw)
	if request.ExpectedSourceHash != nil && *request.ExpectedSourceHash != sourceHash {
		return nil, coded(CodeSkillImportSourceChanged, "legacy Skill source hash changed")
	}
	legacy, err := decodeLegacySkill(raw, skillID)
	if err != nil {
		return nil, coded(CodeSkillImportInvalid, "legacy Skill is invalid")
	}

	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return nil, coded(CodeSkillImportFailed, "begin import transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockImportNamespace(ctx, tx, workspaceID); err != nil {
		return nil, coded(CodeSkillImportFailed, "lock Skill import namespace")
	}
	if response, found, err := i.readReceipt(ctx, tx, workspaceID, request.IdempotencyKey, requestHash); found || err != nil {
		if err == nil {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return nil, coded(CodeSkillImportFailed, "commit replay")
			}
		}
		return response, err
	}

	var latest int64
	var currentHash *string
	headErr := tx.QueryRow(ctx, `
		SELECT h.latest_version, v.content_hash
		FROM weave_skills h
		LEFT JOIN weave_skill_versions v
		  ON v.workspace_id=h.workspace_id AND v.skill_id=h.id AND v.version=h.latest_version
		WHERE h.workspace_id=$1 AND h.id=$2
		FOR UPDATE OF h
	`, workspaceID, skillID).Scan(&latest, &currentHash)
	if headErr != nil && !errors.Is(headErr, pgx.ErrNoRows) {
		return nil, coded(CodeSkillImportFailed, "lock Skill head")
	}

	input := CreateVersionInput{
		Name: legacy.Name, Description: legacy.Description, Body: legacy.Body,
		AlwaysActive: legacy.AlwaysActive, Resources: nil,
	}
	changed := true
	var version *SkillVersion
	if headErr == nil && latest > 0 && currentHash == nil {
		return nil, coded(CodeSkillImportFailed, "current SkillVersion is unavailable")
	}
	if headErr == nil && latest > 0 && currentHash != nil {
		candidate := &SkillVersion{
			WorkspaceID: workspaceID, SkillID: skillID, Version: latest,
			Name: legacy.Name, Description: legacy.Description, Body: legacy.Body,
			AlwaysActive: legacy.AlwaysActive, Resources: []frozen.FrozenSkillResource{},
		}
		candidateHash, hashErr := hashSkillVersion(candidate)
		if hashErr != nil {
			return nil, coded(CodeSkillImportFailed, "hash imported Skill")
		}
		if candidateHash == *currentHash {
			changed = false
			version = candidate
			version.ContentHash = *currentHash
		}
	}
	if changed {
		version, err = i.versions.CreateVersionTx(ctx, tx, workspaceID, skillID, input)
		if err != nil {
			return nil, coded(CodeSkillImportFailed, "create immutable SkillVersion")
		}
	}
	response := &ImportResponse{
		SchemaVersion: 1, SkillID: skillID, Version: version.Version,
		Changed: changed, SourceHash: sourceHash, ContentHash: version.ContentHash,
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return nil, coded(CodeSkillImportFailed, "encode import receipt")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_skill_import_receipts (
		  workspace_id, idempotency_key, request_hash, response
		) VALUES ($1, $2, $3, $4)
	`, workspaceID, request.IdempotencyKey, requestHash, responseJSON); err != nil {
		return nil, coded(CodeSkillImportFailed, "write import receipt")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_skill_import_audits (
		  workspace_id, audit_id, idempotency_key, skill_id, operator_id,
		  reason, request_hash, source_hash, version, changed
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, workspaceID, uuid.NewString(), request.IdempotencyKey, skillID, operatorID,
		request.Reason, requestHash, sourceHash, version.Version, changed); err != nil {
		return nil, coded(CodeSkillImportFailed, "write import audit")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, coded(CodeSkillImportFailed, "commit import")
	}
	return response, nil
}

type receiptQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (i *Importer) readReceipt(ctx context.Context, q receiptQuerier, workspaceID, key, requestHash string) (*ImportResponse, bool, error) {
	var storedHash string
	var raw []byte
	err := q.QueryRow(ctx, `
		SELECT request_hash, response
		FROM weave_skill_import_receipts
		WHERE workspace_id=$1 AND idempotency_key=$2
	`, workspaceID, key).Scan(&storedHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, coded(CodeSkillImportFailed, "read import receipt")
	}
	if storedHash != requestHash {
		return nil, true, coded(CodeSkillImportIdempotencyConflict, "idempotency key was used for another request")
	}
	var response ImportResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, true, coded(CodeSkillImportFailed, "stored import receipt is invalid")
	}
	return &response, true, nil
}

func hashImportRequest(workspaceID, skillID, operatorID string, request ImportRequest) (string, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(skillID) == "" ||
		strings.TrimSpace(operatorID) == "" || request.SchemaVersion != 1 ||
		strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" {
		return "", coded(CodeSkillImportInvalid, "invalid import request")
	}
	if request.ExpectedSourceHash != nil && !validSHA256(*request.ExpectedSourceHash) {
		return "", coded(CodeSkillImportInvalid, "invalid expected source hash")
	}
	payload := struct {
		SchemaVersion      int     `json:"schema_version"`
		LegacyNamespace    string  `json:"legacy_namespace"`
		LegacyKey          string  `json:"legacy_key"`
		Reason             string  `json:"reason"`
		ExpectedSourceHash *string `json:"expected_source_hash"`
		OperatorID         string  `json:"operator_id"`
	}{request.SchemaVersion, "skill:" + workspaceID, skillID, request.Reason, request.ExpectedSourceHash, operatorID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", coded(CodeSkillImportFailed, "hash import request")
	}
	return sha256Hex(raw), nil
}

type legacySkill struct {
	ID           string
	Name         string
	Description  string
	Body         string
	AlwaysActive bool
	Category     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func decodeLegacySkill(raw []byte, expectedID string) (*legacySkill, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("legacy Skill must be an object")
	}
	fields := make(map[string]json.RawMessage)
	allowed := map[string]bool{
		"id": true, "name": true, "description": true, "body": true,
		"always_active": true, "category": true, "created_at": true, "updated_at": true,
	}
	for decoder.More() {
		nameToken, err := decoder.Token()
		name, ok := nameToken.(string)
		if err != nil || !ok || !allowed[name] || fields[name] != nil {
			return nil, errors.New("invalid legacy Skill field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid legacy Skill object")
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, errors.New("trailing legacy Skill data")
	}
	var skill legacySkill
	for name, target := range map[string]any{
		"id": &skill.ID, "name": &skill.Name, "description": &skill.Description,
		"body": &skill.Body, "always_active": &skill.AlwaysActive,
		"category": &skill.Category, "created_at": &skill.CreatedAt, "updated_at": &skill.UpdatedAt,
	} {
		if value := fields[name]; value != nil {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, errors.New("legacy Skill fields may not be null")
			}
			if err := json.Unmarshal(value, target); err != nil {
				return nil, err
			}
		}
	}
	if skill.ID != expectedID || strings.TrimSpace(skill.Name) == "" || strings.TrimSpace(skill.Body) == "" {
		return nil, errors.New("legacy Skill identity or content is invalid")
	}
	return &skill, nil
}

func sha256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ImportCode(err error) string {
	var codedErr *Error
	if errors.As(err, &codedErr) {
		return codedErr.Code()
	}
	return CodeSkillImportFailed
}

// lockImportNamespace serializes receipt checks and first-version creation
// without locking a product workspace row. The caller owns workspace admission.
// This lock ends with the caller's transaction, including rollback/cancellation.
func lockImportNamespace(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "weave/skill-import/v1:"+workspaceID)
	return err
}
