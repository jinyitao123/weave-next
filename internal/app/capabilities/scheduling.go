package capabilities

import (
	"context"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
)

func schedulingPriority(value string) (int, error) {
	switch value {
	case "batch":
		return -1, nil
	case "", "normal":
		return 0, nil
	case "interactive":
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: priority must be batch, normal or interactive", capability.ErrInvalidDefinition)
	}
}
func sameScheduling(left, right *execution.SchedulingHints) bool {
	a, b := execution.SchedulingHints{}, execution.SchedulingHints{}
	if left != nil {
		a = *left
	}
	if right != nil {
		b = *right
	}
	return a == b
}

// Only administrators can raise an application's admission priority ceiling.
func (s *AccessStore) SetApplicationMaxPriority(ctx context.Context, workspace, actor, app, value string) error {
	if _, err := schedulingPriority(value); err != nil {
		return err
	}
	tx, err := s.managerTx(ctx, workspace, actor)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT role IN ('admin','owner') FROM weave_users WHERE tenant_id=$1 AND id=$2 AND NOT disabled FOR SHARE`, workspace, actor).Scan(&allowed); err != nil || !allowed {
		return ErrAccessDenied
	}
	if value == "" {
		value = "normal"
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_capability_apps SET max_priority=$3 WHERE workspace_id=$1 AND id=$2`, workspace, app, value)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err := accessEvent(ctx, tx, workspace, app, actor, "application.priority_changed", value); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
