package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// PGStore is the durable implementation of DraftStore and InvocationStore.
// It deliberately stores the public contract as JSONB so the application
// service remains independent from migration details.
type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

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
	_, err = s.pool.Exec(ctx, `
		INSERT INTO weave_capability_revisions (workspace_id, capability_id, revision, definition_hash, definition)
		VALUES ($1,$2,$3,$4,$5::jsonb)
		ON CONFLICT (workspace_id, capability_id, revision) DO NOTHING
	`, workspaceID, revision.CapabilityID, revision.Revision, revision.DefinitionHash, string(raw))
	if err != nil {
		return fmt.Errorf("save capability revision: %w", err)
	}
	stored, err := s.GetRevision(ctx, workspaceID, revision.CapabilityID, revision.Revision)
	if err != nil {
		return err
	}
	if stored.DefinitionHash != revision.DefinitionHash {
		return fmt.Errorf("capability revision %s/%d already exists with a different definition", revision.CapabilityID, revision.Revision)
	}
	return nil
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

func (s *PGStore) ClaimInvocation(ctx context.Context, invocation Invocation) (Invocation, bool, error) {
	if err := s.ready(); err != nil {
		return Invocation{}, false, err
	}
	input, err := json.Marshal(json.RawMessage(invocation.Input))
	if err != nil {
		return Invocation{}, false, fmt.Errorf("marshal invocation input: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("begin invocation claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	taskID := "cap-task-" + uuid.NewString()
	result, err := tx.Exec(ctx, `
		INSERT INTO weave_capability_invocations (
			workspace_id, application_id, request_id, invocation_id,
			capability_id, revision, input, status, result_state, task_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10)
		ON CONFLICT (workspace_id, application_id, request_id) DO NOTHING
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID,
		invocation.InvocationID, invocation.CapabilityID, invocation.Revision,
		string(input), invocation.Status, invocation.ResultState, taskID)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("claim invocation: %w", err)
	}
	created := result.RowsAffected() == 1
	if created {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_capability_invocation_tasks (
				task_id, workspace_id, invocation_id, capability_id, revision, payload
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb)
			`, taskID, invocation.WorkspaceID, invocation.InvocationID, invocation.CapabilityID, invocation.Revision, string(input)); err != nil {
			return Invocation{}, false, fmt.Errorf("enqueue capability invocation: %w", err)
		}
	}
	var stored Invocation
	var raw []byte
	err = tx.QueryRow(ctx, `
		SELECT workspace_id, application_id, request_id, invocation_id,
			capability_id, revision, input, status, result_state, COALESCE(task_id, '')
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND application_id=$2 AND request_id=$3
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID).Scan(
		&stored.WorkspaceID, &stored.ApplicationID, &stored.RequestID, &stored.InvocationID,
		&stored.CapabilityID, &stored.Revision, &raw, &stored.Status, &stored.ResultState, &stored.TaskID)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("read invocation claim: %w", err)
	}
	stored.Input = raw
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, false, fmt.Errorf("commit invocation claim: %w", err)
	}
	storedInput, canonicalErr := frozen.CanonicalizeJSON(stored.Input)
	if canonicalErr != nil || stored.CapabilityID != invocation.CapabilityID || stored.Revision != invocation.Revision || string(storedInput) != string(invocation.Input) {
		return Invocation{}, false, ErrIdempotencyConflict
	}
	return stored, !created, nil
}

func (s *PGStore) GetInvocation(ctx context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	if err := s.ready(); err != nil {
		return Invocation{}, err
	}
	var invocation Invocation
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, application_id, request_id, invocation_id, task_id,
			capability_id, revision, input, status, result_state
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3
	`, workspaceID, applicationID, invocationID).Scan(
		&invocation.WorkspaceID, &invocation.ApplicationID, &invocation.RequestID, &invocation.InvocationID,
		&invocation.TaskID, &invocation.CapabilityID, &invocation.Revision, &raw,
		&invocation.Status, &invocation.ResultState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, ErrInvocationNotFound
	}
	if err != nil {
		return Invocation{}, fmt.Errorf("get capability invocation: %w", err)
	}
	invocation.Input = raw
	return invocation, nil
}

func (s *PGStore) CancelInvocation(ctx context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	if err := s.ready(); err != nil {
		return Invocation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, fmt.Errorf("begin capability cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, taskID string
	if err := tx.QueryRow(ctx, `
		SELECT status, COALESCE(task_id, '') FROM weave_capability_invocations
		WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3 FOR UPDATE
	`, workspaceID, applicationID, invocationID).Scan(&status, &taskID); errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, ErrInvocationNotFound
	} else if err != nil {
		return Invocation{}, fmt.Errorf("lock capability invocation: %w", err)
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		return Invocation{}, ErrInvocationTerminal
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='cancelled', result_state='unavailable' WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3`, workspaceID, applicationID, invocationID); err != nil {
		return Invocation{}, fmt.Errorf("cancel capability invocation: %w", err)
	}
	if taskID != "" {
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='cancelled' WHERE task_id=$1 AND status='queued'`, taskID); err != nil {
			return Invocation{}, fmt.Errorf("cancel capability invocation task: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, fmt.Errorf("commit capability cancellation: %w", err)
	}
	return s.GetInvocation(ctx, workspaceID, applicationID, invocationID)
}
