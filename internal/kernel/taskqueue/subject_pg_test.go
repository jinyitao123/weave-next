package taskqueue_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"sync"
	"testing"
	"time"
)

func TestExecutionSubjectAndPhysicalFactsRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	alice := execution.Subject{WorkspaceID: "actor-workspace", UserID: "alice"}
	bob := alice
	bob.UserID = "bob"
	a, b := execution.WithSubject(ctx, alice), execution.WithSubject(ctx, bob)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name,created_at) VALUES('actor-workspace','actor-workspace','Actor tests',now())`); err != nil {
		t.Fatal(err)
	}
	agent := registry.AgentRecord{Name: "copy", Role: "worker"}
	if err := registry.New(pool).Put(ctx, alice.WorkspaceID, &agent); err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	deadline := time.Now().Add(time.Hour)
	newTask := func(id string) *taskqueue.Task {
		return &taskqueue.Task{ID: id, WorkspaceID: alice.WorkspaceID, Agent: agent.Name, AgentID: agent.ID, AgentVersion: agent.Version, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Kind: "subject_test", ContextKey: id, Source: "test", Payload: json.RawMessage(`{"user_id":"forged"}`), OutcomeSensitive: true, DeadlineAt: &deadline}
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, entry := range []struct {
		ctx context.Context
		id  string
	}{{a, "alice-task"}, {b, "bob-task"}} {
		wg.Add(1)
		go func(entry struct {
			ctx context.Context
			id  string
		}) { defer wg.Done(); failures <- tasks.Enqueue(entry.ctx, newTask(entry.id)) }(entry)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tasks.Get(b, alice.WorkspaceID, "alice-task"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross user read: %v", err)
	}
	if err := tasks.Cancel(b, alice.WorkspaceID, "alice-task"); err == nil {
		t.Fatal("cross user cancellation accepted")
	}
	list, total, err := tasks.List(b, alice.WorkspaceID, 10, 0)
	if err != nil || total != 1 || list[0].ID != "bob-task" {
		t.Fatalf("list=%+v %d %v", list, total, err)
	}
	forged := newTask("forged")
	forged.Subject = bob
	if err := tasks.Enqueue(a, forged); err == nil {
		t.Fatal("body actor override accepted")
	}
	child := newTask("child")
	child.ParentTaskID = "alice-task"
	if err := tasks.Enqueue(b, child); err == nil {
		t.Fatal("child actor changed")
	}
	if err := tasks.Enqueue(ctx, child); err != nil {
		t.Fatal(err)
	}
	if child.Subject != alice {
		t.Fatal("child lost parent actor")
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET actor_subject=jsonb_set(actor_subject,'{user_id}','"bob"') WHERE id='alice-task'`); err == nil {
		t.Fatal("actor was mutable")
	}
	task, err := tasks.Claim(ctx, "physical-1", taskqueue.ClaimFilter{Kind: "subject_test", IdentityKind: taskqueue.IdentityAgent})
	if err != nil || task == nil {
		t.Fatal(err)
	}
	owner := execution.WithSubject(ctx, task.Subject)
	other := b
	if task.Subject == bob {
		other = a
	}
	if _, err := taskqueue.BindTaskSubject(other, task); err == nil {
		t.Fatal("handler crossed actors")
	}
	usage := &execution.TerminalUsage{InputTokens: 7, OutputTokens: 3, CostUSD: 0.02, ToolCalls: 2}
	if err := tasks.RecordClaimUsage(ctx, task.ID, task.WorkerID, task.ClaimEpoch, usage); err != nil {
		t.Fatal(err)
	}
	if err := tasks.RecordClaimUsage(ctx, task.ID, task.WorkerID, task.ClaimEpoch, usage); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.RecoverStale(ctx); err != nil {
		t.Fatal(err)
	}
	reconcile := func(c context.Context, status string) (*taskqueue.Task, error) {
		tx, err := pool.Begin(c)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(c)
		r, err := tasks.ReconcileTaskTx(c, tx, alice.WorkspaceID, task.ID, task.ClaimEpoch, taskqueue.Reconciliation{Status: status})
		if err == nil {
			err = tx.Commit(c)
		}
		return r, err
	}
	if _, err := reconcile(owner, taskqueue.StatusQueued); err == nil {
		t.Fatal("resumed before stop receipt")
	}
	if err := tasks.AcknowledgeExecutionStopped(ctx, task.ID, task.WorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile(other, taskqueue.StatusQueued); err == nil {
		t.Fatal("another actor resumed task")
	}
	resumed, err := reconcile(owner, taskqueue.StatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Subject != task.Subject || resumed.PhysicalUsage.InputTokens != 7 || resumed.UnreportedAttempts != 0 || resumed.StoppedEpoch != 1 || !resumed.DeadlineAt.Equal(deadline.Truncate(time.Microsecond)) {
		t.Fatalf("physical facts reset: %+v", resumed)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET deadline_at=now()+interval '2 hour' WHERE id=$1`, task.ID); err == nil {
		t.Fatal("deadline mutable")
	}
	if err := tasks.CompleteClaimed(ctx, task.ID, task.WorkerID, json.RawMessage(`{"late":true}`), ""); err == nil {
		t.Fatal("late completion accepted")
	}
	if err := tasks.RecordClaimUsage(ctx, task.ID, "other-worker", task.ClaimEpoch, usage); err == nil {
		t.Fatal("another worker spent receipt accepted")
	}
	if err := tasks.Cancel(owner, alice.WorkspaceID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile(owner, taskqueue.StatusQueued); err == nil {
		t.Fatal("cancellation resurrected")
	}
}
