package capabilities

import (
	"errors"
	"testing"
	"time"
)

func TestCapabilityRuntimeBindingsRejectStaleClaimsRealPG(t *testing.T) {
	for _, mode := range []string{"token", "task", "workspace", "expired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			pool, store, invocation := executionFixture(t)
			task, claimed, err := store.ClaimTask(t.Context())
			if err != nil || !claimed {
				t.Fatalf("claim: %v %v", claimed, err)
			}
			if err := store.BindRuntime(t.Context(), task, "original-runtime"); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordStepRun(t.Context(), task, "confirmed", "original-run"); err != nil {
				t.Fatal(err)
			}
			bad := task
			switch mode {
			case "token":
				bad.ClaimToken = "stale"
			case "task":
				bad.TaskID = "different-task"
			case "workspace":
				bad.WorkspaceID = "different-workspace"
			case "expired":
				_, err = pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks SET deadline_at=now()-interval '1 second' WHERE task_id=$1`, task.TaskID)
			case "cancelled":
				_, err = store.CancelInvocation(t.Context(), "ws", "app", invocation.InvocationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, write := range []func() error{
				func() error { return store.BindRuntime(t.Context(), bad, "stale-runtime") },
				func() error { return store.RecordStepRun(t.Context(), bad, "stale-step", "stale-run") },
				func() error { return store.RenewTask(t.Context(), bad) },
			} {
				if err := write(); !errors.Is(err, ErrClaimLost) {
					t.Errorf("%s claim accepted: %v", mode, err)
				}
			}
			var runtimeID string
			if err := pool.QueryRow(t.Context(), `SELECT runtime_id FROM weave_capability_invocations WHERE invocation_id=$1`, invocation.InvocationID).Scan(&runtimeID); err != nil || runtimeID != "original-runtime" {
				t.Fatalf("runtime attribution changed: %q %v", runtimeID, err)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_step_runs WHERE invocation_id=$1`, invocation.InvocationID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("stale run persisted: %d %v", count, err)
			}
		})
	}
}

func TestCapabilityLiveClaimCanRenewRealPG(t *testing.T) {
	pool, store, _ := executionFixture(t)
	task, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks SET deadline_at=now()+interval '5 seconds' WHERE task_id=$1`, task.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	if err := pool.QueryRow(t.Context(), `SELECT deadline_at FROM weave_capability_invocation_tasks WHERE task_id=$1`, task.TaskID).Scan(&deadline); err != nil || time.Until(deadline) < time.Minute {
		t.Fatalf("live claim did not renew: %v %v", deadline, err)
	}
}
