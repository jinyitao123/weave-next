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
		return ErrRevisionConflict
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
	return s.claimInvocation(ctx, invocation, nil)
}

func (s *PGStore) ClaimDebugInvocation(ctx context.Context, invocation Invocation, snapshot capability.DefinitionSnapshot) (Invocation, bool, error) {
	if _, err := capability.CompileDebug(snapshot); err != nil {
		return Invocation{}, false, err
	}
	if invocation.RunKind != "debug" || invocation.Revision != 0 || invocation.CapabilityID != snapshot.Definition.CapabilityID || invocation.DefinitionHash != snapshot.DefinitionHash {
		return Invocation{}, false, capability.ErrInvalidDefinition
	}
	return s.claimInvocation(ctx, invocation, &snapshot)
}

func (s *PGStore) claimInvocation(ctx context.Context, invocation Invocation, debug *capability.DefinitionSnapshot) (Invocation, bool, error) {
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
   capability_id, revision, input, status, result_state, task_id,run_kind,definition_hash
  ) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12)
		ON CONFLICT (workspace_id, application_id, request_id) DO NOTHING
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID,
		invocation.InvocationID, invocation.CapabilityID, invocation.Revision,
		string(input), invocation.Status, invocation.ResultState, taskID, invocation.RunKind, invocation.DefinitionHash)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("claim invocation: %w", err)
	}
	created := result.RowsAffected() == 1
	if created {
		if debug != nil {
			raw, err := json.Marshal(debug.Definition)
			if err != nil {
				return Invocation{}, false, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_debug_snapshots(workspace_id,invocation_id,definition_hash,definition) VALUES($1,$2,$3,$4::jsonb)`, invocation.WorkspaceID, invocation.InvocationID, debug.DefinitionHash, string(raw)); err != nil {
				return Invocation{}, false, err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_capability_invocation_tasks (
    task_id, workspace_id, invocation_id, capability_id, revision, payload,run_kind
   ) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)
   `, taskID, invocation.WorkspaceID, invocation.InvocationID, invocation.CapabilityID, invocation.Revision, string(input), invocation.RunKind); err != nil {
			return Invocation{}, false, fmt.Errorf("enqueue capability invocation: %w", err)
		}
	}
	var stored Invocation
	var raw []byte
	err = tx.QueryRow(ctx, `
		SELECT workspace_id, application_id, request_id, invocation_id,
   capability_id, revision, input, status, result_state, COALESCE(task_id, ''),run_kind,definition_hash
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND application_id=$2 AND request_id=$3
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID).Scan(
		&stored.WorkspaceID, &stored.ApplicationID, &stored.RequestID, &stored.InvocationID,
		&stored.CapabilityID, &stored.Revision, &raw, &stored.Status, &stored.ResultState, &stored.TaskID, &stored.RunKind, &stored.DefinitionHash)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("read invocation claim: %w", err)
	}
	stored.Input = raw
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, false, fmt.Errorf("commit invocation claim: %w", err)
	}
	storedInput, canonicalErr := frozen.CanonicalizeJSON(stored.Input)
	if canonicalErr != nil || stored.CapabilityID != invocation.CapabilityID || stored.Revision != invocation.Revision || string(storedInput) != string(invocation.Input) || stored.RunKind != invocation.RunKind || stored.DefinitionHash != invocation.DefinitionHash {
		return Invocation{}, false, ErrIdempotencyConflict
	}
	return stored, !created, nil
}

func (s *PGStore) GetInvocation(ctx context.Context, workspaceID, applicationID, invocationID string) (Invocation, error) {
	if workspaceID == "" || applicationID == "" || invocationID == "" {
		return Invocation{}, ErrInvocationNotFound
	}
	if err := s.ready(); err != nil {
		return Invocation{}, err
	}
	var invocation Invocation
	var raw []byte
	query := `
  SELECT workspace_id, application_id, request_id, invocation_id, COALESCE(task_id,''),
   capability_id, revision, input, status, result_state, result, COALESCE(error, ''),run_kind,definition_hash
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND invocation_id=$2`
	args := []any{workspaceID, invocationID}
	if applicationID != "" {
		query = query + " AND application_id=$3"
		args = []any{workspaceID, invocationID, applicationID}
	}
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&invocation.WorkspaceID, &invocation.ApplicationID, &invocation.RequestID, &invocation.InvocationID,
		&invocation.TaskID, &invocation.CapabilityID, &invocation.Revision, &raw,
		&invocation.Status, &invocation.ResultState, &invocation.Result, &invocation.Error, &invocation.RunKind, &invocation.DefinitionHash)
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
	if status == "cancelled" || status == "cancel_requested" {
		return s.GetInvocation(ctx, workspaceID, applicationID, invocationID)
	}
	if status == "completed" || status == "failed" {
		return Invocation{}, ErrInvocationTerminal
	}
	next := "cancelled"
	if status == "running" {
		next = "cancel_requested"
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$4, result_state='unavailable' WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3`, workspaceID, applicationID, invocationID, next); err != nil {
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

func (s *PGStore) ClaimTask(ctx context.Context) (InvocationTask, bool, error) {
	if err := s.ready(); err != nil {
		return InvocationTask{}, false, err
	}
	if err := s.expireTasks(ctx); err != nil {
		return InvocationTask{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InvocationTask{}, false, fmt.Errorf("begin capability task claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var task InvocationTask
	var raw, definitionRaw []byte
	var definitionHash string
	var expectedHash string
	err = tx.QueryRow(ctx, `
  SELECT task.task_id, task.workspace_id, task.invocation_id, task.capability_id, task.revision, task.payload,task.run_kind,i.definition_hash
  FROM weave_capability_invocations AS i
  JOIN weave_capability_invocation_tasks AS task ON task.workspace_id=i.workspace_id AND task.invocation_id=i.invocation_id AND task.task_id=i.task_id
  WHERE i.status='queued' AND task.status='queued'
  ORDER BY task.created_at, task.task_id LIMIT 1 FOR UPDATE OF i SKIP LOCKED
 `).Scan(&task.TaskID, &task.WorkspaceID, &task.InvocationID, &task.CapabilityID, &task.Revision, &raw, &task.RunKind, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvocationTask{}, false, nil
	}
	if err != nil {
		return InvocationTask{}, false, fmt.Errorf("claim capability task: %w", err)
	}
	task.Input = raw
	task.ClaimToken = uuid.NewString()
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='running',claim_token=$2,deadline_at=now()+interval '150 seconds' WHERE task_id=$1`, task.TaskID, task.ClaimToken); err != nil {
		return InvocationTask{}, false, err
	}
	var source pgx.Row
	if task.RunKind == "debug" {
		source = tx.QueryRow(ctx, `SELECT definition_hash,definition FROM weave_capability_debug_snapshots WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID)
	} else {
		source = tx.QueryRow(ctx, `SELECT definition_hash, definition FROM weave_capability_revisions WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, task.WorkspaceID, task.CapabilityID, task.Revision)
	}
	if err := source.Scan(&definitionHash, &definitionRaw); err != nil {
		return InvocationTask{}, false, fmt.Errorf("load capability revision for task: %w", err)
	}
	var definition capability.Definition
	if err := json.Unmarshal(definitionRaw, &definition); err != nil {
		return InvocationTask{}, false, fmt.Errorf("decode capability revision for task: %w", err)
	}
	plan, err := capability.Compile(capability.PublishedRevision{SchemaVersion: capability.SchemaVersionV1, CapabilityID: task.CapabilityID, Revision: task.Revision, Definition: definition, DefinitionHash: definitionHash})
	if task.RunKind == "debug" {
		plan, err = capability.CompileDebug(capability.DefinitionSnapshot{Definition: definition, DefinitionHash: definitionHash})
	}
	if definitionHash != expectedHash || definition.CapabilityID != task.CapabilityID {
		err = capability.ErrInvalidRevision
	}
	if err != nil {
		if _, writeErr := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='failed' WHERE task_id=$1`, task.TaskID); writeErr != nil {
			return InvocationTask{}, false, writeErr
		}
		if _, writeErr := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='failed',error='capability_plan_invalid',result_state='unavailable' WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID); writeErr != nil {
			return InvocationTask{}, false, writeErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return InvocationTask{}, false, commitErr
		}
		return InvocationTask{}, false, fmt.Errorf("compile capability task: %w", err)
	}
	task.Plan = plan
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='running' WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID); err != nil {
		return InvocationTask{}, false, fmt.Errorf("mark invocation running: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InvocationTask{}, false, fmt.Errorf("commit capability task claim: %w", err)
	}
	return task, true, nil
}

func (s *PGStore) CompleteTask(ctx context.Context, task InvocationTask, result json.RawMessage, executeErr error) (Invocation, error) {
	if err := s.ready(); err != nil {
		return Invocation{}, err
	}
	status, resultState := "completed", "available"
	if executeErr != nil {
		status, resultState = "failed", "unavailable"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, fmt.Errorf("begin capability task completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current, appID string
	if err := tx.QueryRow(ctx, `SELECT status,application_id FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2 AND task_id=$3 FOR UPDATE`, task.WorkspaceID, task.InvocationID, task.TaskID).Scan(&current, &appID); err != nil {
		return Invocation{}, ErrClaimLost
	}
	if current != "running" && current != "cancel_requested" {
		return Invocation{}, ErrClaimLost
	}
	if current == "cancel_requested" {
		status, resultState = "cancelled", "unavailable"
		result = nil
	}
	if executeErr == nil && status == "completed" && !json.Valid(result) {
		executeErr = errors.New("invalid execution output")
		status, resultState = "failed", "unavailable"
	}
	var resultValue any
	if status == "completed" {
		resultValue = string(result)
	}
	changed, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status=$2 WHERE task_id=$1 AND workspace_id=$3 AND invocation_id=$4 AND status='running' AND claim_token=$5 AND deadline_at>now()`, task.TaskID, status, task.WorkspaceID, task.InvocationID, task.ClaimToken)
	if err != nil {
		return Invocation{}, fmt.Errorf("complete capability task: %w", err)
	}
	if changed.RowsAffected() != 1 {
		return Invocation{}, ErrClaimLost
	}
	var errorText *string
	if executeErr != nil {
		value := executeErr.Error()
		errorText = &value
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$3, result_state=$4, result=$5::jsonb, error=$6 WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID, status, resultState, resultValue, errorText); err != nil {
		return Invocation{}, fmt.Errorf("write capability invocation result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, fmt.Errorf("commit capability task completion: %w", err)
	}
	return s.GetInvocation(ctx, task.WorkspaceID, appID, task.InvocationID)
}
