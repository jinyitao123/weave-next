package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const (
	CodeSkillVersionRequired      = "workflow_skill_version_required"
	CodeDependencyVersionRequired = "workflow_dependency_version_required"
	CodeFrozenManifestMismatch    = "workflow_frozen_manifest_mismatch"
)

type Error struct {
	code   string
	detail string
}

var (
	ErrSkillVersionRequired      = &Error{code: CodeSkillVersionRequired}
	ErrDependencyVersionRequired = &Error{code: CodeDependencyVersionRequired}
	ErrFrozenManifestMismatch    = &Error{code: CodeFrozenManifestMismatch}
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

type CreateVersionInput struct {
	Name         string
	Description  string
	Body         string
	AlwaysActive bool
	Resources    []frozen.FrozenSkillResource
}

type SkillVersion struct {
	WorkspaceID  string
	SkillID      string
	Version      int64
	Name         string
	Description  string
	Body         string
	AlwaysActive bool
	Resources    []frozen.FrozenSkillResource
	ContentHash  string
	CreatedAt    time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateVersion(
	ctx context.Context,
	workspaceID, skillID string,
	input CreateVersionInput,
) (*SkillVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin SkillVersion creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	version, err := s.CreateVersionTx(ctx, tx, workspaceID, skillID, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit SkillVersion creation: %w", err)
	}
	return version, nil
}

func (s *Store) CreateVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, skillID string,
	input CreateVersionInput,
) (*SkillVersion, error) {
	if strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(skillID) == "" ||
		strings.TrimSpace(input.Name) == "" {
		return nil, coded(CodeSkillVersionRequired, "workspace, skill, and name are required")
	}
	resources, err := normalizeResources(input.Resources)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_skills (
		  workspace_id, id, name, latest_version
		) VALUES ($1, $2, $3, 0)
		ON CONFLICT (workspace_id, id) DO NOTHING
	`, workspaceID, skillID, input.Name); err != nil {
		return nil, coded(CodeSkillVersionRequired, "create Skill head: "+err.Error())
	}

	var storedName string
	var latest int64
	err = tx.QueryRow(ctx, `
		SELECT name, latest_version
		FROM weave_skills
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, skillID).Scan(&storedName, &latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, coded(CodeSkillVersionRequired, "Skill head is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("lock Skill head: %w", err)
	}
	if storedName != input.Name {
		return nil, coded(CodeSkillVersionRequired, "Skill name does not match its immutable identity")
	}
	if latest < 0 || latest >= frozen.MaxJCSSafeInteger {
		return nil, coded(CodeDependencyVersionRequired, "Skill version is outside the JCS safe integer range")
	}
	next := latest + 1
	version := &SkillVersion{
		WorkspaceID:  workspaceID,
		SkillID:      skillID,
		Version:      next,
		Name:         input.Name,
		Description:  input.Description,
		Body:         input.Body,
		AlwaysActive: input.AlwaysActive,
		Resources:    resources,
	}
	version.ContentHash, err = hashSkillVersion(version)
	if err != nil {
		return nil, coded(CodeFrozenManifestMismatch, "hash SkillVersion: "+err.Error())
	}
	resourcesJSON, err := json.Marshal(version.Resources)
	if err != nil {
		return nil, coded(CodeFrozenManifestMismatch, "encode Skill resources")
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO weave_skill_versions (
		  workspace_id, skill_id, version, name, description, body,
		  always_active, resources, content_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING created_at
	`, version.WorkspaceID, version.SkillID, version.Version, version.Name,
		version.Description, version.Body, version.AlwaysActive, resourcesJSON,
		version.ContentHash,
	).Scan(&version.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert SkillVersion: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_skills
		SET latest_version=$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, skillID, next); err != nil {
		return nil, fmt.Errorf("advance Skill head: %w", err)
	}
	return cloneSkillVersion(version), nil
}

func (s *Store) GetVersion(
	ctx context.Context,
	workspaceID, skillID string,
	version int64,
) (*SkillVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin SkillVersion read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := s.GetVersionTx(ctx, tx, workspaceID, skillID, version)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit SkillVersion read: %w", err)
	}
	return got, nil
}

func (s *Store) GetVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, skillID string,
	version int64,
) (*SkillVersion, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(skillID) == "" {
		return nil, coded(CodeSkillVersionRequired, "exact workspace and Skill identity are required")
	}
	if version < 1 || version > frozen.MaxJCSSafeInteger {
		return nil, coded(CodeDependencyVersionRequired, "Skill version is outside the JCS safe integer range")
	}

	var stored SkillVersion
	var resourcesJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, skill_id, version, name, description, body,
		       always_active, resources, content_hash, created_at
		FROM weave_skill_versions
		WHERE workspace_id=$1 AND skill_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, skillID, version).Scan(
		&stored.WorkspaceID, &stored.SkillID, &stored.Version, &stored.Name,
		&stored.Description, &stored.Body, &stored.AlwaysActive, &resourcesJSON,
		&stored.ContentHash, &stored.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		var ignored string
		headErr := tx.QueryRow(ctx, `
			SELECT id FROM weave_skills
			WHERE workspace_id=$1 AND id=$2
			FOR SHARE
		`, workspaceID, skillID).Scan(&ignored)
		if errors.Is(headErr, pgx.ErrNoRows) {
			return nil, coded(CodeSkillVersionRequired, "immutable Skill is unavailable in the workspace")
		}
		if headErr != nil {
			return nil, fmt.Errorf("classify missing SkillVersion: %w", headErr)
		}
		return nil, coded(CodeDependencyVersionRequired, "exact SkillVersion is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("read SkillVersion: %w", err)
	}
	resources, err := decodeStoredResources(resourcesJSON)
	if err != nil {
		return nil, coded(CodeFrozenManifestMismatch, "stored Skill resources are invalid")
	}
	stored.Resources = resources
	hash, err := hashSkillVersion(&stored)
	if err != nil || hash != stored.ContentHash {
		return nil, coded(CodeFrozenManifestMismatch, "stored Skill content hash does not match")
	}
	return cloneSkillVersion(&stored), nil
}

// ResolveSkillVersionTx resolves one exact immutable SkillVersion through the
// caller-owned transaction using the shared nullable dependency-version shape.
func (s *Store) ResolveSkillVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, skillID string,
	dependencyVersion *int64,
) (*SkillVersion, error) {
	if tx == nil {
		return nil, coded(CodeSkillVersionRequired, "caller transaction is required")
	}
	if dependencyVersion == nil {
		return nil, coded(CodeDependencyVersionRequired, "exact SkillVersion is required")
	}
	return s.GetVersionTx(ctx, tx, workspaceID, skillID, *dependencyVersion)
}

func normalizeResources(
	resources []frozen.FrozenSkillResource,
) ([]frozen.FrozenSkillResource, error) {
	copied := append([]frozen.FrozenSkillResource(nil), resources...)
	if copied == nil {
		copied = []frozen.FrozenSkillResource{}
	}
	for _, resource := range copied {
		if resource.Kind != "script" && resource.Kind != "reference" {
			return nil, coded(CodeSkillVersionRequired, "unsupported Skill resource kind")
		}
		if resource.Encoding != "utf8" && resource.Encoding != "base64" {
			return nil, coded(CodeSkillVersionRequired, "unsupported Skill resource encoding")
		}
		if strings.TrimSpace(resource.MediaType) == "" || !safeResourcePath(resource.RelativePath) {
			return nil, coded(CodeSkillVersionRequired, "invalid Skill resource metadata")
		}
		hash, err := frozen.ComputeResourceContentHash(resource)
		if err != nil {
			return nil, coded(CodeSkillVersionRequired, "invalid Skill resource content")
		}
		if resource.ContentHash != hash {
			return nil, coded(CodeFrozenManifestMismatch, "Skill resource content hash does not match")
		}
	}
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].Kind != copied[j].Kind {
			return copied[i].Kind < copied[j].Kind
		}
		return copied[i].RelativePath < copied[j].RelativePath
	})
	for index := 1; index < len(copied); index++ {
		if copied[index-1].Kind == copied[index].Kind &&
			copied[index-1].RelativePath == copied[index].RelativePath {
			return nil, coded(CodeSkillVersionRequired, "duplicate Skill resource identity")
		}
	}
	return copied, nil
}

func safeResourcePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) ||
		path.Clean(value) != value {
		return false
	}
	segments := strings.Split(value, "/")
	if strings.Contains(segments[0], ":") {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func decodeStoredResources(raw []byte) ([]frozen.FrozenSkillResource, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New("stored Skill resources must be an array")
	}
	resources := make([]frozen.FrozenSkillResource, len(items))
	for index, item := range items {
		resource, err := frozen.DecodeFrozenSkillResource(item)
		if err != nil {
			return nil, err
		}
		resources[index] = resource
	}
	return normalizeResources(resources)
}

func hashSkillVersion(value *SkillVersion) (string, error) {
	version := value.Version
	return frozen.HashDTO(frozen.FrozenSkill{
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   value.WorkspaceID,
		Name:          value.Name,
		SourceType:    "registry_version",
		SkillID:       value.SkillID,
		SkillVersion:  &version,
		Description:   value.Description,
		Body:          value.Body,
		AlwaysActive:  value.AlwaysActive,
		Resources:     value.Resources,
	}, frozen.PreorderFrozenSkill)
}

func cloneSkillVersion(value *SkillVersion) *SkillVersion {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Resources = append([]frozen.FrozenSkillResource(nil), value.Resources...)
	return &cloned
}

func coded(code, detail string) *Error {
	return &Error{code: code, detail: detail}
}
