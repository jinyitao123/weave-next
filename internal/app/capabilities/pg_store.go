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
	var quotaMaxSteps, quotaMaxActive int
	if err := tx.QueryRow(ctx, `SELECT max_steps_per_invocation,max_active_invocations FROM weave_capability_quotas WHERE workspace_id=$1`, invocation.WorkspaceID).Scan(&quotaMaxSteps, &quotaMaxActive); errors.Is(err, pgx.ErrNoRows) {
		quotaMaxSteps, quotaMaxActive = 100, 20
	} else if err != nil {
		return Invocation{}, false, err
	}
	if invocation.MaxSteps <= 0 || invocation.MaxSteps > quotaMaxSteps {
		invocation.MaxSteps = quotaMaxSteps
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_capability_invocations WHERE workspace_id=$1 AND status IN ('queued','running','waiting','cancel_requested') AND NOT (application_id=$2 AND request_id=$3)`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID).Scan(&active); err != nil {
		return Invocation{}, false, err
	}
	if active >= quotaMaxActive {
		return Invocation{}, false, ErrQuotaExceeded
	}
	if invocation.CredentialID != "" {
		if invocation.RunKind != "published" || debug != nil {
			return Invocation{}, false, ErrAccessDenied
		}
		if err := authorizeApplicationInvocation(ctx, tx, invocation); err != nil {
			return Invocation{}, false, err
		}
	}
	if invocation.CallerKind == "" {
		invocation.CallerKind = "developer"
	}
	result, err := tx.Exec(ctx, `
		INSERT INTO weave_capability_invocations (
			workspace_id, application_id, request_id, invocation_id,
   capability_id, revision, input, status, result_state, task_id,run_kind,definition_hash,credential_id,caller_kind,actor_user_id,max_steps
  ) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,NULLIF($13,''),$14,$15,$16)
		ON CONFLICT (workspace_id, application_id, request_id) DO NOTHING
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID,
		invocation.InvocationID, invocation.CapabilityID, invocation.Revision,
		string(input), invocation.Status, invocation.ResultState, taskID, invocation.RunKind, invocation.DefinitionHash, invocation.CredentialID, invocation.CallerKind, invocation.ActorUserID, invocation.MaxSteps)
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
   capability_id, revision, input, status, result_state, COALESCE(task_id, ''),run_kind,definition_hash,caller_kind,COALESCE(credential_id,'')
		FROM weave_capability_invocations
		WHERE workspace_id=$1 AND application_id=$2 AND request_id=$3
	`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID).Scan(
		&stored.WorkspaceID, &stored.ApplicationID, &stored.RequestID, &stored.InvocationID,
		&stored.CapabilityID, &stored.Revision, &raw, &stored.Status, &stored.ResultState, &stored.TaskID, &stored.RunKind, &stored.DefinitionHash, &stored.CallerKind, &stored.CredentialID)
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
   capability_id, revision, input, status, result_state, result, COALESCE(error, ''),run_kind,definition_hash,caller_kind
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
		&invocation.Status, &invocation.ResultState, &invocation.Result, &invocation.Error, &invocation.RunKind, &invocation.DefinitionHash, &invocation.CallerKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, ErrInvocationNotFound
	}
	if err != nil {
		return Invocation{}, fmt.Errorf("get capability invocation: %w", err)
	}
	invocation.Input = raw
	var checkpointRaw []byte
	_ = s.pool.QueryRow(ctx, `SELECT actor_user_id,runtime_id,used_steps,max_steps,checkpoint FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2`, workspaceID, invocationID).Scan(&invocation.ActorUserID, &invocation.RuntimeID, &invocation.UsedSteps, &invocation.MaxSteps, &checkpointRaw)
	if len(checkpointRaw) > 0 {
		_ = json.Unmarshal(checkpointRaw, &invocation.Checkpoint)
	}
	var definitionRaw []byte
	if invocation.RunKind == "debug" {
		err = s.pool.QueryRow(ctx, `SELECT definition FROM weave_capability_debug_snapshots WHERE workspace_id=$1 AND invocation_id=$2`, workspaceID, invocationID).Scan(&definitionRaw)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT definition FROM weave_capability_revisions WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, workspaceID, invocation.CapabilityID, invocation.Revision).Scan(&definitionRaw)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, err
	}
	if len(definitionRaw) > 0 {
		var definition capability.Definition
		if err := json.Unmarshal(definitionRaw, &definition); err != nil {
			return Invocation{}, err
		}
		presentInvocation(&invocation, definition)
	}
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
		if err := tx.Commit(ctx); err != nil {
			return Invocation{}, err
		}
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
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='cancelled',claim_token=NULL,deadline_at=NULL WHERE task_id=$1 AND status IN ('queued','waiting')`, taskID); err != nil {
			return Invocation{}, fmt.Errorf("cancel capability invocation task: %w", err)
		}
	}
	if next == "cancelled" {
		if _, err := tx.Exec(ctx, `UPDATE weave_capability_human_tasks SET status='cancelled',completed_at=now() WHERE workspace_id=$1 AND invocation_id=$2 AND status='waiting'`, workspaceID, invocationID); err != nil {
			return Invocation{}, fmt.Errorf("cancel capability human task: %w", err)
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
	var callerKind, applicationID string
	err = tx.QueryRow(ctx, `
  SELECT task.task_id, task.workspace_id, task.invocation_id, task.capability_id, task.revision, task.payload,task.run_kind,i.definition_hash,i.caller_kind,i.application_id
  FROM weave_capability_invocations AS i
  JOIN weave_capability_invocation_tasks AS task ON task.workspace_id=i.workspace_id AND task.invocation_id=i.invocation_id AND task.task_id=i.task_id
  WHERE i.status='queued' AND task.status='queued'
  ORDER BY task.created_at, task.task_id LIMIT 1 FOR UPDATE OF i SKIP LOCKED
 `).Scan(&task.TaskID, &task.WorkspaceID, &task.InvocationID, &task.CapabilityID, &task.Revision, &raw, &task.RunKind, &expectedHash, &callerKind, &applicationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvocationTask{}, false, nil
	}
	if err != nil {
		return InvocationTask{}, false, fmt.Errorf("claim capability task: %w", err)
	}
	task.Input = raw
	var checkpointRaw []byte
	if err := tx.QueryRow(ctx, `SELECT checkpoint,actor_user_id,used_steps,max_steps FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID).Scan(&checkpointRaw, &task.ActorUserID, &task.UsedSteps, &task.MaxSteps); err != nil {
		return InvocationTask{}, false, err
	}
	if len(checkpointRaw) > 0 {
		_ = json.Unmarshal(checkpointRaw, &task.State)
	}
	if callerKind == "application" {
		err := authorizeApplicationGrant(ctx, tx, Invocation{WorkspaceID: task.WorkspaceID, ApplicationID: applicationID, CapabilityID: task.CapabilityID, Revision: task.Revision})
		if err != nil {
			if !errors.Is(err, ErrAccessDenied) {
				return InvocationTask{}, false, err
			}
			if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='failed' WHERE task_id=$1`, task.TaskID); err != nil {
				return InvocationTask{}, false, err
			}
			if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='failed',error='application_authorization_revoked',result_state='unavailable' WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID); err != nil {
				return InvocationTask{}, false, err
			}
			return InvocationTask{}, false, tx.Commit(ctx)
		}
	}
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
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='running',started_at=COALESCE(started_at,now()) WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID); err != nil {
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
	var pause *capability.PauseError
	if errors.As(executeErr, &pause) {
		status, resultState = "waiting", "unavailable"
	} else if executeErr != nil {
		status, resultState = "failed", "unavailable"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, fmt.Errorf("begin capability task completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current, appID, callerKind string
	if err := tx.QueryRow(ctx, `SELECT status,application_id,caller_kind FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2 AND task_id=$3 FOR UPDATE`, task.WorkspaceID, task.InvocationID, task.TaskID).Scan(&current, &appID, &callerKind); err != nil {
		return Invocation{}, ErrClaimLost
	}
	if current != "running" && current != "cancel_requested" {
		return Invocation{}, ErrClaimLost
	}
	if callerKind == "application" && current == "running" {
		if authErr := authorizeApplicationGrant(ctx, tx, Invocation{WorkspaceID: task.WorkspaceID, ApplicationID: appID, CapabilityID: task.CapabilityID, Revision: task.Revision}); authErr != nil {
			if !errors.Is(authErr, ErrAccessDenied) {
				return Invocation{}, authErr
			}
			executeErr = ErrAccessDenied
			status, resultState = "failed", "unavailable"
			result = nil
		}
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
	if pause != nil && status == "waiting" {
		schema := pause.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_human_tasks(workspace_id,invocation_id,step_id,title,response_schema,status) VALUES($1,$2,$3,$4,$5::jsonb,'waiting') ON CONFLICT(workspace_id,invocation_id,step_id) DO UPDATE SET title=EXCLUDED.title,response_schema=EXCLUDED.response_schema,status='waiting',response=NULL,completed_at=NULL`, task.WorkspaceID, task.InvocationID, pause.StepID, pause.Title, string(schema)); err != nil {
			return Invocation{}, err
		}
	}
	var errorText *string
	if executeErr != nil && pause == nil {
		value := executeErr.Error()
		errorText = &value
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$3, result_state=$4, result=$5::jsonb, error=$6,completed_at=CASE WHEN $3 IN ('completed','failed','cancelled') THEN now() ELSE NULL END WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID, status, resultState, resultValue, errorText); err != nil {
		return Invocation{}, fmt.Errorf("write capability invocation result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Invocation{}, fmt.Errorf("commit capability task completion: %w", err)
	}
	return s.GetInvocation(ctx, task.WorkspaceID, appID, task.InvocationID)
}
