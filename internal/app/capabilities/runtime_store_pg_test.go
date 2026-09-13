package capabilities

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
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
			originalCtx := execution.WithInvocationID(t.Context(), "activation-original")
			if err := store.RecordStepRun(originalCtx, task, "confirmed", "original-run"); err != nil {
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
				func() error {
					return store.RecordStepRun(execution.WithInvocationID(t.Context(), "activation-stale"), bad, "stale-step", "stale-run")
				},
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

func TestCapabilityStepRunReceiptsAreActivationAwareAndIdempotentRealPG(t *testing.T) {
	pool, store, invocation := executionFixture(t)
	task, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	first := execution.WithInvocationID(t.Context(), "activation-1")
	second := execution.WithInvocationID(t.Context(), "activation-2")
	for _, item := range []struct {
		ctx   context.Context
		runID string
	}{{first, "run-1"}, {first, "run-1"}, {first, "run-2"}, {second, "run-3"}} {
		if err := store.RecordStepRun(item.ctx, task, "loop-step", item.runID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordStepRun(second, task, "loop-step", "run-1"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("physical run rebound to another activation: %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_step_runs WHERE invocation_id=$1`, invocation.InvocationID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("receipts=%d err=%v", count, err)
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

func TestCapabilityExecutionDeadlineCarriesAcrossClaimsRealPG(t *testing.T) {
	pool, store, invocation := executionFixture(t)
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks
	 SET execution_budget_ms=10000,execution_consumed_ms=6000
	 WHERE task_id=$1`, invocation.TaskID); err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("first claim: %v %v", claimed, err)
	}
	remaining := time.Until(first.ExecutionDeadline)
	if remaining <= 3*time.Second || remaining > 5*time.Second {
		t.Fatalf("first remaining execution budget = %v", remaining)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks
	 SET claim_started_at=now()-interval '2 seconds'
	 WHERE task_id=$1`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	pause := &capability.PauseError{StepID: "human", Title: "confirm"}
	if _, err := store.CompleteTask(t.Context(), first, nil, pause); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks
	 SET status='queued' WHERE task_id=$1 AND status='waiting'`, first.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocations
	 SET status='queued' WHERE workspace_id=$1 AND invocation_id=$2 AND status='waiting'`, first.WorkspaceID, first.InvocationID); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("second claim: %v %v", claimed, err)
	}
	remaining = time.Until(second.ExecutionDeadline)
	if remaining <= time.Second || remaining > 3*time.Second {
		t.Fatalf("resumed execution budget was reset: %v", remaining)
	}
}

func TestCapabilityUnknownOutcomeRequiresReceiptBeforeRetryRealPG(t *testing.T) {
	_, store, invocation := executionFixture(t)
	first, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := store.AbandonTask(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetInvocation(t.Context(), invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID)
	if err != nil || stored.Status != "reconciling" {
		t.Fatalf("unknown outcome = %+v %v", stored, err)
	}
	if _, claimed, err := store.ClaimTask(t.Context()); err != nil || claimed {
		t.Fatalf("unknown outcome was dispatched again: %v %v", claimed, err)
	}
	if _, err := store.ReconcileTask(t.Context(), invocation.WorkspaceID, invocation.InvocationID, Reconciliation{Decision: ReconcileRetrySafe}); err == nil {
		t.Fatal("reconciliation without physical receipt succeeded")
	}
	reconciled, err := store.ReconcileTask(t.Context(), invocation.WorkspaceID, invocation.InvocationID, Reconciliation{
		Decision: ReconcileRetrySafe, ReceiptID: "runtime-stop-1",
	})
	if err != nil || reconciled.Status != "queued" {
		t.Fatalf("retry-safe reconciliation = %+v %v", reconciled, err)
	}
	second, claimed, err := store.ClaimTask(t.Context())
	if err != nil || !claimed {
		t.Fatalf("reconciled claim: %v %v", claimed, err)
	}
	if err := store.AbandonTask(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelInvocation(t.Context(), invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID)
	if err != nil || cancelled.Status != "cancel_requested" {
		t.Fatalf("cancel uncertain task = %+v %v", cancelled, err)
	}
	final, err := store.ReconcileTask(t.Context(), invocation.WorkspaceID, invocation.InvocationID, Reconciliation{
		Decision: ReconcileRetrySafe, ReceiptID: "runtime-stop-2",
	})
	if err != nil || final.Status != "cancelled" {
		t.Fatalf("cancel reconciliation = %+v %v", final, err)
	}
}
