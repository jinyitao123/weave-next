package capabilities

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jinyitao123/weave/internal/kernel/capability"
)

// SetApplicationActiveLimit sets or, with nil, removes an application's own
// limit on capability invocations in flight. It counts invocations, not
// members or model calls, by the same in-flight definition as the workspace
// quota, which keeps applying. Lowering it evicts no work in flight; new
// admissions wait until enough of it ends. Like the priority ceiling, only
// administrators change it.
func (s *AccessStore) SetApplicationActiveLimit(ctx context.Context, workspace, actor, app string, limit *int) error {
	if limit != nil && (*limit < 1 || *limit > 20000) {
		return fmt.Errorf("%w: max_active_invocations must be between 1 and 20000", capability.ErrInvalidDefinition)
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
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2)`, workspace, app).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	scope := quotaScope{workspace, app}
	if _, _, _, err := lockQuotaScope(ctx, tx, scope); err != nil {
		return err
	}
	if limit == nil {
		if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_quota_slots WHERE workspace_id=$1 AND scope_application_id=$2`, workspace, app); err != nil {
			return fmt.Errorf("remove application quota slots: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM weave_capability_application_quotas WHERE workspace_id=$1 AND application_id=$2`, workspace, app); err != nil {
			return fmt.Errorf("remove application admission limit: %w", err)
		}
		if err := accessEvent(ctx, tx, workspace, app, actor, "application.active_limit_removed", ""); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var ready bool
	if err := tx.QueryRow(ctx, `INSERT INTO weave_capability_application_quotas(workspace_id,application_id,max_active_invocations)
	 VALUES($1,$2,$3) ON CONFLICT (workspace_id,application_id) DO UPDATE SET max_active_invocations=EXCLUDED.max_active_invocations,updated_at=now()
	 RETURNING slots_ready`, workspace, app, *limit).Scan(&ready); err != nil {
		return fmt.Errorf("set application admission limit: %w", err)
	}
	// Slots not yet created are created from the limit on first admission.
	if ready {
		if err := resizeQuotaSlots(ctx, tx, scope, *limit); err != nil {
			return err
		}
	}
	if err := accessEvent(ctx, tx, workspace, app, actor, "application.active_limit_changed", strconv.Itoa(*limit)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
