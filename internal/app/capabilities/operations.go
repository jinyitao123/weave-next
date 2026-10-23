package capabilities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

type Quota struct {
	MaxStepsPerInvocation int `json:"max_steps_per_invocation"`
	MaxActiveInvocations  int `json:"max_active_invocations"`
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
	if q.MaxStepsPerInvocation < 1 || q.MaxStepsPerInvocation > 1000 || q.MaxActiveInvocations < 1 || q.MaxActiveInvocations > 20000 {
		return Quota{}, capability.ErrInvalidDefinition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Quota{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, ready, err := lockWorkspaceCapabilityQuota(ctx, tx, workspaceID)
	if err != nil {
		return Quota{}, err
	}
	err = tx.QueryRow(ctx, `UPDATE weave_capability_quotas SET max_steps_per_invocation=$2,max_active_invocations=$3,updated_at=now() WHERE workspace_id=$1 RETURNING max_steps_per_invocation,max_active_invocations`, workspaceID, q.MaxStepsPerInvocation, q.MaxActiveInvocations).Scan(&q.MaxStepsPerInvocation, &q.MaxActiveInvocations)
	if err != nil {
		return Quota{}, err
	}
	// Slots not yet created are created from the new limit on first admission.
	if ready {
		if err := resizeQuotaSlots(ctx, tx, quotaScope{WorkspaceID: workspaceID}, q.MaxActiveInvocations); err != nil {
			return Quota{}, err
		}
	}
	return q, tx.Commit(ctx)
}
