package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

type InvocationEvent struct {
	Sequence int64           `json:"sequence"`
	Type     string          `json:"type"`
	StepID   string          `json:"step_id,omitempty"`
	StepKind string          `json:"step_kind,omitempty"`
	Detail   json.RawMessage `json:"detail"`
}
type HumanTask struct {
	InvocationID   string          `json:"invocation_id"`
	StepID         string          `json:"step_id"`
	Title          string          `json:"title"`
	ResponseSchema json.RawMessage `json:"response_schema"`
	Status         string          `json:"status"`
}
type Quota struct {
	MaxStepsPerInvocation int `json:"max_steps_per_invocation"`
	MaxActiveInvocations  int `json:"max_active_invocations"`
}

type PGExecutionObserver struct {
	Store *PGStore
	Task  InvocationTask
}

func (o PGExecutionObserver) Checkpoint(ctx context.Context, state capability.ExecutionState, event capability.ExecutionEvent) error {
	if o.Store == nil {
		return errors.New("capability checkpoint store unavailable")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(event)
	tx, err := o.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	increment := 0
	if event.Type == "step_completed" {
		increment = 1
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_capability_invocations i SET checkpoint=$5::jsonb,used_steps=used_steps+$6
 FROM weave_capability_invocation_tasks t WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.task_id=$3 AND t.task_id=$3 AND t.claim_token=$4 AND i.status='running' AND t.status='running' AND t.deadline_at>now() AND i.used_steps+$6<=i.max_steps`, o.Task.WorkspaceID, o.Task.InvocationID, o.Task.TaskID, o.Task.ClaimToken, string(raw), increment)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		var used, max int
		if err := tx.QueryRow(ctx, `SELECT used_steps,max_steps FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2`, o.Task.WorkspaceID, o.Task.InvocationID).Scan(&used, &max); err == nil && used+increment > max {
			return ErrQuotaExceeded
		}
		return ErrClaimLost
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_capability_invocation_events(workspace_id,invocation_id,event_type,step_id,step_kind,detail) VALUES($1,$2,$3,$4,$5,$6::jsonb)`, o.Task.WorkspaceID, o.Task.InvocationID, event.Type, event.StepID, string(event.StepKind), string(detail)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PGStore) ListInvocations(ctx context.Context, workspaceID, applicationID, actorUserID string) ([]Invocation, error) {
	rows, err := s.pool.Query(ctx, `SELECT application_id,invocation_id FROM weave_capability_invocations WHERE workspace_id=$1 AND application_id=$2 AND ($3='' OR actor_user_id=$3) ORDER BY created_at DESC LIMIT 100`, workspaceID, applicationID, actorUserID)
	if err != nil {
		return nil, err
	}
	type key struct{ app, id string }
	keys := []key{}
	for rows.Next() {
		var item key
		if err := rows.Scan(&item.app, &item.id); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	result := make([]Invocation, 0, len(keys))
	for _, item := range keys {
		v, err := s.GetInvocation(ctx, workspaceID, item.app, item.id)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}
func (s *PGStore) ListEvents(ctx context.Context, workspaceID, invocationID string) ([]InvocationEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT sequence,event_type,step_id,step_kind,detail FROM weave_capability_invocation_events WHERE workspace_id=$1 AND invocation_id=$2 ORDER BY sequence`, workspaceID, invocationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvocationEvent{}
	for rows.Next() {
		var v InvocationEvent
		if err := rows.Scan(&v.Sequence, &v.Type, &v.StepID, &v.StepKind, &v.Detail); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *PGStore) GetHumanTask(ctx context.Context, workspaceID, invocationID string) (HumanTask, error) {
	var v HumanTask
	v.InvocationID = invocationID
	err := s.pool.QueryRow(ctx, `SELECT step_id,title,response_schema,status FROM weave_capability_human_tasks WHERE workspace_id=$1 AND invocation_id=$2 AND status='waiting' ORDER BY created_at LIMIT 1`, workspaceID, invocationID).Scan(&v.StepID, &v.Title, &v.ResponseSchema, &v.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrInvocationNotFound
	}
	return v, err
}
func (s *PGStore) ResumeHuman(ctx context.Context, workspaceID, applicationID, invocationID, stepID string, response json.RawMessage) (Invocation, error) {
	if !json.Valid(response) {
		return Invocation{}, capability.ErrInvalidDefinition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Invocation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	var schema json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT i.checkpoint,h.response_schema FROM weave_capability_invocations i JOIN weave_capability_human_tasks h ON h.workspace_id=i.workspace_id AND h.invocation_id=i.invocation_id WHERE i.workspace_id=$1 AND i.application_id=$2 AND i.invocation_id=$3 AND i.status='waiting' AND h.step_id=$4 AND h.status='waiting' FOR UPDATE OF i,h`, workspaceID, applicationID, invocationID, stepID).Scan(&raw, &schema); errors.Is(err, pgx.ErrNoRows) {
		return Invocation{}, ErrInvocationNotFound
	} else if err != nil {
		return Invocation{}, err
	}
	if err := capability.ValidateValue(schema, response); err != nil {
		return Invocation{}, fmt.Errorf("%w: approval response: %v", capability.ErrInvalidDefinition, err)
	}
	var state capability.ExecutionState
	_ = json.Unmarshal(raw, &state)
	state, err = capability.ResumeHuman(state, stepID, response)
	if err != nil {
		return Invocation{}, err
	}
	raw, _ = json.Marshal(state)
	if _, err = tx.Exec(ctx, `UPDATE weave_capability_human_tasks SET status='completed',response=$4::jsonb,completed_at=now() WHERE workspace_id=$1 AND invocation_id=$2 AND step_id=$3 AND status='waiting'`, workspaceID, invocationID, stepID, string(response)); err != nil {
		return Invocation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_capability_invocation_tasks SET status='queued',claim_token=NULL,deadline_at=NULL WHERE workspace_id=$1 AND invocation_id=$2 AND status='waiting'`, workspaceID, invocationID); err != nil {
		return Invocation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_capability_invocations SET status='queued',checkpoint=$3::jsonb,error=NULL WHERE workspace_id=$1 AND invocation_id=$2`, workspaceID, invocationID, string(raw)); err != nil {
		return Invocation{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_capability_invocation_events(workspace_id,invocation_id,event_type,step_id,detail) VALUES($1,$2,'human_completed',$3,$4::jsonb)`, workspaceID, invocationID, stepID, string(response)); err != nil {
		return Invocation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Invocation{}, err
	}
	return s.GetInvocation(ctx, workspaceID, applicationID, invocationID)
}
func (s *PGStore) GetQuota(ctx context.Context, workspaceID string) (Quota, error) {
	var q Quota
	err := s.pool.QueryRow(ctx, `INSERT INTO weave_capability_quotas(workspace_id) VALUES($1) ON CONFLICT DO NOTHING RETURNING max_steps_per_invocation,max_active_invocations`, workspaceID).Scan(&q.MaxStepsPerInvocation, &q.MaxActiveInvocations)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT max_steps_per_invocation,max_active_invocations FROM weave_capability_quotas WHERE workspace_id=$1`, workspaceID).Scan(&q.MaxStepsPerInvocation, &q.MaxActiveInvocations)
	}
	return q, err
}
func (s *PGStore) SetQuota(ctx context.Context, workspaceID string, q Quota) (Quota, error) {
	if q.MaxStepsPerInvocation < 1 || q.MaxStepsPerInvocation > 1000 || q.MaxActiveInvocations < 1 || q.MaxActiveInvocations > 1000 {
		return Quota{}, capability.ErrInvalidDefinition
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO weave_capability_quotas(workspace_id,max_steps_per_invocation,max_active_invocations) VALUES($1,$2,$3) ON CONFLICT(workspace_id) DO UPDATE SET max_steps_per_invocation=EXCLUDED.max_steps_per_invocation,max_active_invocations=EXCLUDED.max_active_invocations,updated_at=now() RETURNING max_steps_per_invocation,max_active_invocations`, workspaceID, q.MaxStepsPerInvocation, q.MaxActiveInvocations).Scan(&q.MaxStepsPerInvocation, &q.MaxActiveInvocations)
	return q, err
}
