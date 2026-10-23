package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

// PGStore is the durable implementation of DraftStore and InvocationStore.
// It deliberately stores the public contract as JSONB so the application
// service remains independent from migration details.
type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

func (s *PGStore) ListDrafts(ctx context.Context, workspaceID string) ([]capability.Definition, error) {
	rows, err := s.pool.Query(ctx, `SELECT definition FROM weave_capability_definitions WHERE workspace_id=$1 ORDER BY updated_at DESC,capability_id LIMIT 100`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []capability.Definition{}
	for rows.Next() {
		var raw []byte
		var d capability.Definition
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *PGStore) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("capability postgres store is not configured")
	}
	return nil
}

func (s *PGStore) SaveDraft(ctx context.Context, workspaceID string, definition capability.Definition) error {
	if err := s.ready(); err != nil {
		return err
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		return fmt.Errorf("marshal capability draft: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO weave_capability_definitions (workspace_id, capability_id, definition, updated_at)
		VALUES ($1,$2,$3::jsonb,now())
		ON CONFLICT (workspace_id, capability_id) DO UPDATE
		SET definition=EXCLUDED.definition, updated_at=now()
	`, workspaceID, definition.CapabilityID, string(raw))
	if err != nil {
		return fmt.Errorf("save capability draft: %w", err)
	}
	return nil
}

func (s *PGStore) GetDraft(ctx context.Context, workspaceID, capabilityID string) (capability.Definition, error) {
	if err := s.ready(); err != nil {
		return capability.Definition{}, err
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT definition FROM weave_capability_definitions
		WHERE workspace_id=$1 AND capability_id=$2
	`, workspaceID, capabilityID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return capability.Definition{}, ErrNotFound
	}
	if err != nil {
		return capability.Definition{}, fmt.Errorf("get capability draft: %w", err)
	}
	var definition capability.Definition
	if err := json.Unmarshal(raw, &definition); err != nil {
		return capability.Definition{}, fmt.Errorf("decode capability draft: %w", err)
	}
	return definition, nil
}

func (s *PGStore) SaveRevision(ctx context.Context, workspaceID string, revision capability.PublishedRevision) error {
	if err := s.ready(); err != nil {
		return err
	}
	raw, err := json.Marshal(revision.Definition)
	if err != nil {
		return fmt.Errorf("marshal capability revision: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin capability publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var existingHash string
	existingErr := tx.QueryRow(ctx, `SELECT definition_hash FROM weave_capability_revisions
		WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, workspaceID, revision.CapabilityID, revision.Revision).Scan(&existingHash)
	if existingErr == nil {
		if existingHash != revision.DefinitionHash {
			return ErrRevisionConflict
		}
		var persisted bool
		if err := tx.QueryRow(ctx, `SELECT true FROM weave_capability_revision_tool_bindings
			WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, workspaceID, revision.CapabilityID, revision.Revision).Scan(&persisted); err != nil || !persisted {
			return ErrRevisionConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(existingErr, pgx.ErrNoRows) {
		return existingErr
	}
	// Only inline tool steps froze MCP bindings; a workflow capability declares
	// none, and the empty row still marks the publication as complete.
	bindingsRaw := []byte("[]")
	_, err = tx.Exec(ctx, `
		INSERT INTO weave_capability_revisions (workspace_id, capability_id, revision, definition_hash, definition)
		VALUES ($1,$2,$3,$4,$5::jsonb)
		ON CONFLICT (workspace_id, capability_id, revision) DO NOTHING
	`, workspaceID, revision.CapabilityID, revision.Revision, revision.DefinitionHash, string(raw))
	if err != nil {
		return fmt.Errorf("save capability revision: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_capability_revision_tool_bindings(workspace_id,capability_id,revision,bindings)
		VALUES($1,$2,$3,$4::jsonb) ON CONFLICT DO NOTHING`, workspaceID, revision.CapabilityID, revision.Revision, string(bindingsRaw))
	if err != nil {
		return fmt.Errorf("save capability tool bindings: %w", err)
	}
	var storedHash string
	var storedBindings []byte
	if err := tx.QueryRow(ctx, `SELECT r.definition_hash,b.bindings FROM weave_capability_revisions r JOIN weave_capability_revision_tool_bindings b
		USING(workspace_id,capability_id,revision) WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, workspaceID, revision.CapabilityID, revision.Revision).Scan(&storedHash, &storedBindings); err != nil {
		return err
	}
	canonicalStored, _ := frozen.CanonicalizeJSON(storedBindings)
	canonicalBindings, _ := frozen.CanonicalizeJSON(bindingsRaw)
	if storedHash != revision.DefinitionHash || string(canonicalStored) != string(canonicalBindings) {
		return ErrRevisionConflict
	}
	return tx.Commit(ctx)
}

func (s *PGStore) GetRevision(ctx context.Context, workspaceID, capabilityID string, revision int64) (capability.PublishedRevision, error) {
	if err := s.ready(); err != nil {
		return capability.PublishedRevision{}, err
	}
	var stored capability.PublishedRevision
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT capability_id, revision, definition_hash, definition
		FROM weave_capability_revisions
		WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3
	`, workspaceID, capabilityID, revision).Scan(&stored.CapabilityID, &stored.Revision, &stored.DefinitionHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return capability.PublishedRevision{}, ErrRevisionNotFound
	}
	if err != nil {
		return capability.PublishedRevision{}, fmt.Errorf("get capability revision: %w", err)
	}
	if err := json.Unmarshal(raw, &stored.Definition); err != nil {
		return capability.PublishedRevision{}, fmt.Errorf("decode capability revision: %w", err)
	}
	stored.SchemaVersion = capability.SchemaVersionV1
	return stored, nil
}
