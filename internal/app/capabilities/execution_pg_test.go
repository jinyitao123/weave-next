package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func executionFixture(t *testing.T) (*pgxpool.Pool, *PGStore, Invocation) {
	t.Helper()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap", Name: "cap", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "r"}}, Steps: []capability.Step{{ID: "s", Name: "s", RoleID: "r", Kind: capability.StepWorker, Instruction: "run"}}}
	if err := service.SaveDraft(t.Context(), DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(t.Context(), "ws", "cap", 1); err != nil {
		t.Fatal(err)
	}
	i, _, err := service.Invoke(t.Context(), InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", CapabilityID: "cap", Revision: 1, RequestID: "req", Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	return pool, store, i
}

func TestCapabilityConcurrentClaimAndLateCompletionRealPG(t *testing.T) {
	_, store, i := executionFixture(t)
	var wg sync.WaitGroup
	claims := make(chan InvocationTask, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, ok, err := store.ClaimTask(t.Context())
			if err != nil {
				errs <- err
			}
			if ok {
				claims <- task
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%d", len(claims))
	}
	task := <-claims
	cancelled, err := store.CancelInvocation(t.Context(), "ws", "app", i.InvocationID)
	if err != nil || cancelled.Status != "cancel_requested" {
		t.Fatalf("cancellation=%+v err=%v", cancelled, err)
	}
	completed, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{"late":true}`), nil)
	if err != nil || completed.Status != "cancelled" || len(completed.Result) > 0 {
		t.Fatalf("late output survived cancellation: %+v %v", completed, err)
	}
	if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{}`), nil); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("duplicate finish=%v", err)
	}
}

func TestCapabilityExpiryAndFailureRealPG(t *testing.T) {
	for _, kind := range []string{"failure", "expired", "wrong_token"} {
		t.Run(kind, func(t *testing.T) {
			pool, store, i := executionFixture(t)
			task, ok, err := store.ClaimTask(t.Context())
			if err != nil || !ok {
				t.Fatal(err)
			}
			switch kind {
			case "failure":
				failed, err := store.CompleteTask(t.Context(), task, nil, errors.New("provider failed"))
				if err != nil || failed.Status != "failed" || failed.ResultState != "unavailable" || len(failed.Result) > 0 {
					t.Fatalf("failure=%+v %v", failed, err)
				}
			case "expired":
				if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_invocation_tasks SET deadline_at=now()-interval '1 second' WHERE task_id=$1`, task.TaskID); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := store.ClaimTask(t.Context()); err != nil || claimed {
					t.Fatalf("expired task was retried: %v %v", claimed, err)
				}
				if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{}`), nil); !errors.Is(err, ErrClaimLost) {
					t.Fatalf("expired write=%v", err)
				}
				found, err := store.GetInvocation(t.Context(), "ws", "app", i.InvocationID)
				if err != nil || found.Status != "failed" {
					t.Fatalf("orphan remains: %+v %v", found, err)
				}
			case "wrong_token":
				task.ClaimToken = "wrong"
				if _, err := store.CompleteTask(t.Context(), task, json.RawMessage(`{}`), nil); !errors.Is(err, ErrClaimLost) {
					t.Fatalf("forged finish=%v", err)
				}
			}
		})
	}
}

func TestCapabilityRestartReplayOwnershipAndRevisionFreezeRealPG(t *testing.T) {
	pool, store, i := executionFixture(t)
	separate, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer separate.Close()
	restarted := NewPGStore(separate)
	if _, err := restarted.GetInvocation(t.Context(), "ws", "", i.InvocationID); !errors.Is(err, ErrInvocationNotFound) {
		t.Fatal("empty app broadened ownership")
	}
	if _, err := restarted.GetInvocation(t.Context(), "ws", "other", i.InvocationID); !errors.Is(err, ErrInvocationNotFound) {
		t.Fatal("other app read invocation")
	}
	result, replayed, err := NewService(restarted, restarted).Invoke(t.Context(), InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", CapabilityID: "cap", Revision: 1, RequestID: "req", Input: json.RawMessage(`{ "x": 1.0 }`)})
	if err != nil || !replayed || result.InvocationID != i.InvocationID {
		t.Fatalf("restart replay: %+v %v %v", result, replayed, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_capability_revisions SET definition='{}' WHERE workspace_id='ws'`); err == nil {
		t.Fatal("database allowed revision rewrite")
	}
	current, err := store.GetRevision(t.Context(), "ws", "cap", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Validate(); err != nil {
		t.Fatal(err)
	}
}

type waitExecutor struct{ started chan struct{} }

func (e waitExecutor) Execute(ctx context.Context, _ InvocationTask) (json.RawMessage, error) {
	close(e.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCapabilityWorkerPropagatesPersistedCancelRealPG(t *testing.T) {
	_, store, i := executionFixture(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := RunOne(t.Context(), store, waitExecutor{started}); done <- err }()
	<-started
	if _, err := store.CancelInvocation(t.Context(), "ws", "app", i.InvocationID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.GetInvocation(t.Context(), "ws", "app", i.InvocationID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled=%+v %v", cancelled, err)
	}
}
