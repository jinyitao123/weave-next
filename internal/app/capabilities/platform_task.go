package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// PlatformTaskHandler adapts a capability business record to the one platform
// execution worker. It never claims, renews, cancels, or settles queue tasks.
type PlatformTaskHandler struct {
	Store    *PGStore
	Executor TaskExecutor
}

func (h PlatformTaskHandler) ExecuteTask(ctx context.Context, physical taskqueue.Task) (taskqueue.TaskResult, error) {
	if h.Store == nil || h.Executor == nil {
		return taskqueue.TaskResult{}, errors.New("capability execution dependencies are not configured")
	}
	task, err := h.Store.LoadPlatformTask(ctx, physical)
	if err != nil {
		return taskqueue.TaskResult{}, err
	}
	result, executeErr := executeSafely(ctx, h.Executor, task)
	usage, usageErr := h.Store.physicalUsage(ctx, task)
	if usageErr != nil {
		return taskqueue.TaskResult{}, usageErr
	}
	var pause *capability.PauseError
	if errors.As(executeErr, &pause) {
		if err := h.Store.recordHumanPause(ctx, task, pause); err != nil {
			return taskqueue.TaskResult{}, err
		}
		return taskqueue.TaskResult{Pause: true, Usage: usage}, nil
	}
	if executeErr == nil && !json.Valid(result) {
		return taskqueue.TaskResult{}, errors.New("invalid execution output")
	}
	return taskqueue.TaskResult{Result: result, Usage: usage}, executeErr
}

// physicalUsage closes one platform attempt only when every recorded runtime
// run has a terminal marker. Missing runtime receipts remain unreported.
func (s *PGStore) physicalUsage(ctx context.Context, task InvocationTask) (*execution.TerminalUsage, error) {
	var recordedRuns, terminalRuns int
	var inputTokens, outputTokens, toolCalls int64
	var costUSD float64
	err := s.pool.QueryRow(ctx, `WITH runs AS (
	 SELECT DISTINCT run_id FROM weave_capability_step_runs WHERE workspace_id=$1 AND invocation_id=$2 AND activation_id=$3
	) SELECT count(r.run_id),count(m.run_id),COALESCE(sum(m.usage_input_tokens),0),
	 COALESCE(sum(m.usage_output_tokens),0),COALESCE(sum(m.usage_cost_usd),0),COALESCE(sum(m.usage_tool_calls),0)
	 FROM runs r LEFT JOIN weave_run_terminal_markers m ON m.workspace_id=$1 AND m.run_id=r.run_id`,
		task.WorkspaceID, task.InvocationID, fmt.Sprintf("%s/%d", task.TaskID, task.ClaimEpoch)).Scan(&recordedRuns, &terminalRuns, &inputTokens, &outputTokens, &costUSD, &toolCalls)
	if err != nil {
		return nil, err
	}
	if recordedRuns != terminalRuns {
		return nil, nil
	}
	var directCalls int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM weave_capability_invocation_events
	 WHERE workspace_id=$1 AND invocation_id=$2 AND event_type='step_completed' AND step_kind='tool'`,
		task.WorkspaceID, task.InvocationID).Scan(&directCalls); err != nil {
		return nil, err
	}
	usage := execution.TerminalUsage{InputTokens: int(inputTokens), OutputTokens: int(outputTokens), CostUSD: costUSD, ToolCalls: int(toolCalls + directCalls)}
	return &usage, nil
}

// LoadPlatformTask resolves immutable capability inputs after the platform
// worker has claimed the exact task and restored its authenticated subject.
func (s *PGStore) LoadPlatformTask(ctx context.Context, physical taskqueue.Task) (InvocationTask, error) {
	if physical.IdentityKind != taskqueue.IdentityCapability || physical.Kind != "capability_invocation" ||
		physical.CapabilityInvocationID == "" || physical.ContextKey != physical.CapabilityInvocationID ||
		physical.Status != taskqueue.StatusRunning || physical.WorkerID == "" || physical.ClaimEpoch < 1 {
		return InvocationTask{}, ErrClaimLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InvocationTask{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	task := InvocationTask{TaskID: physical.ID, WorkspaceID: physical.WorkspaceID,
		InvocationID: physical.CapabilityInvocationID, ClaimEpoch: physical.ClaimEpoch, WorkerID: physical.WorkerID}
	var checkpointRaw, definitionRaw []byte
	var expectedHash, definitionHash, callerKind, applicationID string
	err = tx.QueryRow(ctx, `SELECT i.capability_id,i.revision,i.input,i.run_kind,i.definition_hash,i.caller_kind,
	 i.application_id,i.actor_user_id,i.used_steps,i.max_steps,i.checkpoint
	 FROM weave_capability_invocations i
	 WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3 AND i.status IN ('queued','running')
	 FOR UPDATE`, task.WorkspaceID, task.InvocationID, task.TaskID).Scan(
		&task.CapabilityID, &task.Revision, &task.Input, &task.RunKind, &expectedHash, &callerKind,
		&applicationID, &task.ActorUserID, &task.UsedSteps, &task.MaxSteps, &checkpointRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvocationTask{}, ErrClaimLost
	}
	if err != nil {
		return InvocationTask{}, err
	}
	if len(checkpointRaw) > 0 {
		_ = json.Unmarshal(checkpointRaw, &task.State)
	}
	if callerKind == "application" {
		if err := authorizeApplicationGrant(ctx, tx, Invocation{WorkspaceID: task.WorkspaceID, ApplicationID: applicationID, CapabilityID: task.CapabilityID, Revision: task.Revision}); err != nil {
			return InvocationTask{}, err
		}
	}
	var source pgx.Row
	if task.RunKind == "debug" {
		source = tx.QueryRow(ctx, `SELECT definition_hash,definition FROM weave_capability_debug_snapshots WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID)
	} else {
		source = tx.QueryRow(ctx, `SELECT definition_hash,definition FROM weave_capability_revisions WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, task.WorkspaceID, task.CapabilityID, task.Revision)
	}
	if err := source.Scan(&definitionHash, &definitionRaw); err != nil {
		return InvocationTask{}, fmt.Errorf("load capability revision for task: %w", err)
	}
	var definition capability.Definition
	if err := json.Unmarshal(definitionRaw, &definition); err != nil {
		return InvocationTask{}, err
	}
	var plan capability.Plan
	if task.RunKind == "debug" {
		plan, err = capability.CompileDebug(capability.DefinitionSnapshot{Definition: definition, DefinitionHash: definitionHash})
	} else {
		plan, err = capability.Compile(capability.PublishedRevision{SchemaVersion: capability.SchemaVersionV1, CapabilityID: task.CapabilityID, Revision: task.Revision, Definition: definition, DefinitionHash: definitionHash})
	}
	if err != nil || definitionHash != expectedHash || definition.CapabilityID != task.CapabilityID {
		return InvocationTask{}, capability.ErrInvalidRevision
	}
	task.Plan = plan
	if len(plan.Resources.Tools) > 0 {
		if task.RunKind != "published" {
			return InvocationTask{}, errors.New("debug capability tool execution is unavailable until publication")
		}
		var bindingsRaw []byte
		if err := tx.QueryRow(ctx, `SELECT bindings FROM weave_capability_revision_tool_bindings
		 WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, task.WorkspaceID, task.CapabilityID, task.Revision).Scan(&bindingsRaw); err != nil {
			return InvocationTask{}, err
		}
		if err := json.Unmarshal(bindingsRaw, &task.ToolBindings); err != nil {
			return InvocationTask{}, err
		}
		for i, binding := range task.ToolBindings {
			raw, _ := json.Marshal(binding)
			normalized, err := frozen.DecodeFrozenMCPBinding(raw)
			if err != nil {
				return InvocationTask{}, err
			}
			task.ToolBindings[i] = normalized
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='running',started_at=COALESCE(started_at,now()),error=NULL
	 WHERE workspace_id=$1 AND invocation_id=$2 AND task_id=$3 AND status IN ('queued','running')`, task.WorkspaceID, task.InvocationID, task.TaskID); err != nil {
		return InvocationTask{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InvocationTask{}, err
	}
	return task, nil
}

func (s *PGStore) recordHumanPause(ctx context.Context, task InvocationTask, pause *capability.PauseError) error {
	schema := pause.Schema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	changed, err := tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='waiting',result_state='unavailable',error=NULL
	 WHERE workspace_id=$1 AND invocation_id=$2 AND task_id=$3 AND status='running'`, task.WorkspaceID, task.InvocationID, task.TaskID)
	if err != nil {
		return err
	}
	if changed.RowsAffected() != 1 {
		return ErrClaimLost
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_capability_human_tasks(workspace_id,invocation_id,step_id,title,response_schema,status)
	 VALUES($1,$2,$3,$4,$5::jsonb,'waiting')
	 ON CONFLICT(workspace_id,invocation_id,step_id) DO UPDATE SET title=EXCLUDED.title,response_schema=EXCLUDED.response_schema,status='waiting',response=NULL,completed_at=NULL`,
		task.WorkspaceID, task.InvocationID, pause.StepID, pause.Title, string(schema)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TaskActive is the direct-tool claim gate. The platform lease, epoch and
// immutable invocation identity are all checked before each external effect.
func (s *PGStore) TaskActive(ctx context.Context, task InvocationTask) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
	 SELECT 1 FROM weave_task_queue t JOIN weave_capability_invocations i
	 ON i.workspace_id=t.workspace_id AND i.invocation_id=t.capability_invocation_id
	 WHERE t.id=$1 AND t.workspace_id=$2 AND t.capability_invocation_id=$3
	 AND t.worker_id=$4 AND t.claim_epoch=$5 AND t.status='running' AND i.status='running'
	 AND t.lease_expires_at>now() AND (t.deadline_at IS NULL OR t.deadline_at>now())
	 AND (i.caller_kind='developer' OR EXISTS(SELECT 1 FROM weave_capability_apps a
	 JOIN weave_capability_grants g ON g.workspace_id=a.workspace_id AND g.app_id=a.id
	 WHERE a.workspace_id=i.workspace_id AND a.id=i.application_id AND a.enabled AND g.enabled
	 AND g.capability_id=i.capability_id AND g.revision=i.revision)))`,
		task.TaskID, task.WorkspaceID, task.InvocationID, task.WorkerID, task.ClaimEpoch).Scan(&active)
	return active, err
}
