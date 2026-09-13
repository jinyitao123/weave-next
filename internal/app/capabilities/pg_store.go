package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// PGStore is the durable implementation of DraftStore and InvocationStore.
// It deliberately stores the public contract as JSONB so the application
// service remains independent from migration details.
type PGStore struct {
	pool  *pgxpool.Pool
	queue *taskqueue.Store
}

func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return NewPGStoreWithTaskQueue(pool, taskqueue.New(pool, nil, time.Minute))
}

func NewPGStoreWithTaskQueue(pool *pgxpool.Pool, queue *taskqueue.Store) *PGStore {
	return &PGStore{pool: pool, queue: queue}
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
	bindings := []frozen.FrozenMCPBinding{}
	byServer := map[string][]string{}
	for _, ref := range revision.Definition.Resources.Tools {
		byServer[ref.MCPServerID] = append(byServer[ref.MCPServerID], ref.ToolName)
	}
	for serverID, tools := range byServer {
		preview, resolveErr := mcpregistry.ResolveCurrentMCPToolsTx(ctx, tx, workspaceID, serverID, mcpregistry.MCPAgentPolicy{Filter: tools})
		if resolveErr != nil {
			return fmt.Errorf("freeze capability tool %s: %w", serverID, resolveErr)
		}
		writeTools := []string{}
		for _, tool := range preview.Tools {
			if !tool.ReadOnly {
				writeTools = append(writeTools, tool.Name)
			}
		}
		binding, resolveErr := mcpregistry.ResolveCurrentMCPToolsTx(ctx, tx, workspaceID, serverID, mcpregistry.MCPAgentPolicy{Filter: tools, WriteTools: writeTools})
		if resolveErr != nil {
			return fmt.Errorf("freeze capability tool %s: %w", serverID, resolveErr)
		}
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ServerID < bindings[j].ServerID })
	bindingsRaw, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
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
	if _, err := tx.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1) ON CONFLICT DO NOTHING`, invocation.WorkspaceID); err != nil {
		return Invocation{}, false, err
	}
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
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM weave_capability_invocations i JOIN weave_task_queue q
	 ON q.workspace_id=i.workspace_id AND q.id=i.task_id AND q.capability_invocation_id=i.invocation_id
	 WHERE i.workspace_id=$1 AND (q.status IN ('queued','running','cancel_requested') OR (q.status='completed' AND i.status='waiting') OR (q.status='failed' AND q.worker_id IS NOT NULL))
	 AND NOT (i.application_id=$2 AND i.request_id=$3)`, invocation.WorkspaceID, invocation.ApplicationID, invocation.RequestID).Scan(&active); err != nil {
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
	subject, err := execution.RequireSubject(ctx, invocation.WorkspaceID)
	if err != nil {
		return Invocation{}, false, err
	}
	if invocation.ActorUserID != "" && invocation.ActorUserID != subject.UserID {
		return Invocation{}, false, execution.ErrSubjectMismatch
	}
	invocation.ActorUserID = subject.UserID
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
		deadline := time.Now().Add(45 * time.Minute)
		payload, _ := json.Marshal(map[string]string{"invocation_id": invocation.InvocationID})
		if err := s.queue.EnqueueTx(ctx, tx, &taskqueue.Task{
			ID: taskID, WorkspaceID: invocation.WorkspaceID,
			IdentityKind: taskqueue.IdentityCapability, IdentitySchemaVersion: 2,
			Source: "capability", Kind: "capability_invocation", ContextKey: invocation.InvocationID,
			CapabilityInvocationID: invocation.InvocationID, Payload: payload,
			DeadlineAt: &deadline, OutcomeSensitive: true,
		}); err != nil {
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
	if invocation.TaskID != "" {
		physical, taskErr := s.queue.Get(ctx, workspaceID, invocation.TaskID)
		if taskErr != nil {
			return Invocation{}, taskErr
		}
		invocation.PhysicalUsage = physical.PhysicalUsage
		invocation.UnreportedAttempts = physical.UnreportedAttempts
		switch physical.Status {
		case taskqueue.StatusQueued:
			invocation.Status, invocation.ResultState, invocation.Result, invocation.Error = "queued", "unavailable", nil, ""
		case taskqueue.StatusRunning:
			invocation.Status = "running"
		case taskqueue.StatusCancelRequested:
			invocation.Status, invocation.ResultState = "cancel_requested", "unavailable"
		case taskqueue.StatusCompleted:
			if invocation.Status != "waiting" {
				invocation.Status, invocation.ResultState, invocation.Result, invocation.Error = "completed", "available", physical.Result, ""
			}
		case taskqueue.StatusFailed:
			if physical.WorkerID != "" {
				if invocation.Status != "cancel_requested" {
					invocation.Status = "reconciling"
				}
				invocation.ResultState, invocation.Result, invocation.Error = "unavailable", nil, ""
			} else {
				invocation.Status, invocation.ResultState, invocation.Result, invocation.Error = "failed", "unavailable", nil, physical.Error
			}
		case taskqueue.StatusCancelled:
			invocation.Status, invocation.ResultState, invocation.Result, invocation.Error = "cancelled", "unavailable", nil, ""
		case taskqueue.StatusTimedOut:
			invocation.Status, invocation.ResultState, invocation.Result, invocation.Error = "failed", "unavailable", nil, physical.Error
		}
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
	if taskID != "" {
		physical, queueErr := s.queue.GetTx(ctx, tx, workspaceID, taskID)
		if queueErr != nil {
			return Invocation{}, queueErr
		}
		switch physical.Status {
		case taskqueue.StatusCompleted:
			if status != "waiting" {
				status = "completed"
			}
		case taskqueue.StatusFailed:
			if physical.WorkerID != "" {
				status = "reconciling"
			} else {
				status = "failed"
			}
		case taskqueue.StatusCancelled:
			status = "cancelled"
		}
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
	if status == "running" || status == "reconciling" {
		next = "cancel_requested"
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status=$4, result_state='unavailable' WHERE workspace_id=$1 AND application_id=$2 AND invocation_id=$3`, workspaceID, applicationID, invocationID, next); err != nil {
		return Invocation{}, fmt.Errorf("cancel capability invocation: %w", err)
	}
	if taskID != "" {
		if err := s.queue.CancelTx(ctx, tx, workspaceID, taskID); err != nil {
			return Invocation{}, fmt.Errorf("cancel capability platform task: %w", err)
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
