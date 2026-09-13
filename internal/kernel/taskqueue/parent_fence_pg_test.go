package taskqueue_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestParentStopFencesAdmissionClaimAndRecoveryRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	base := context.Background()
	if err := db.Migrate(base, pool); err != nil {
		t.Fatal(err)
	}
	subject := execution.Subject{WorkspaceID: "parent-fence", UserID: "alice"}
	ctx := execution.WithSubject(base, subject)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, subject.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	agent := registry.AgentRecord{Name: "parent-fence-agent", Role: "worker"}
	if err := registry.New(pool).Put(ctx, subject.WorkspaceID, &agent); err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	parentDeadline := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	newTask := func(id, kind string) *taskqueue.Task {
		return &taskqueue.Task{ID: id, WorkspaceID: subject.WorkspaceID, Agent: agent.Name,
			AgentID: agent.ID, AgentVersion: agent.Version, IdentityKind: taskqueue.IdentityAgent,
			IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
			Kind: kind, Source: "test", ContextKey: id, Payload: json.RawMessage(`{"input":true}`),
			OutcomeSensitive: true, DeadlineAt: &parentDeadline}
	}

	// Admission and cancellation lock the same parent. Whichever wins, no child
	// remains claimable after the cancellation commits.
	for attempt := 0; attempt < 12; attempt++ {
		parentID := "admit-parent-" + string(rune('a'+attempt))
		childID := "admit-child-" + string(rune('a'+attempt))
		if err := tasks.Enqueue(ctx, newTask(parentID, "admit-parent")); err != nil {
			t.Fatal(err)
		}
		child := newTask(childID, "admit-child")
		child.ParentTaskID = parentID
		later := parentDeadline.Add(time.Hour)
		child.DeadlineAt = &later
		start := make(chan struct{})
		var wg sync.WaitGroup
		var admitErr, cancelErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; admitErr = tasks.Enqueue(ctx, child) }()
		go func() { defer wg.Done(); <-start; cancelErr = tasks.Cancel(ctx, subject.WorkspaceID, parentID) }()
		close(start)
		wg.Wait()
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		if admitErr == nil {
			stored, err := tasks.Get(ctx, subject.WorkspaceID, childID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != taskqueue.StatusCancelled || !stored.DeadlineAt.Equal(parentDeadline) {
				t.Fatalf("admitted child escaped stop fence: %+v", stored)
			}
		}
	}

	// A claim racing a parent stop either loses admission or retains its physical
	// owner until the worker supplies usage and an actual stop receipt.
	parent := newTask("claim-parent", "claim-parent")
	if err := tasks.Enqueue(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := newTask("claim-child", "claim-child")
	child.ParentTaskID = parent.ID
	if err := tasks.Enqueue(ctx, child); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var claimed *taskqueue.Task
	var claimErr, cancelErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		claimed, claimErr = tasks.Claim(ctx, "claim-worker", taskqueue.ClaimFilter{Kind: child.Kind, IdentityKind: child.IdentityKind})
	}()
	go func() { defer wg.Done(); <-start; cancelErr = tasks.Cancel(ctx, subject.WorkspaceID, parent.ID) }()
	close(start)
	wg.Wait()
	if claimErr != nil || cancelErr != nil {
		t.Fatalf("claim/cancel race failed: %v / %v", claimErr, cancelErr)
	}
	stored, err := tasks.Get(ctx, subject.WorkspaceID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		if stored.Status != taskqueue.StatusCancelled {
			t.Fatalf("unclaimed child not cancelled: %+v", stored)
		}
	} else {
		if stored.Status != taskqueue.StatusCancelRequested || stored.WorkerID != claimed.WorkerID {
			t.Fatalf("claimed child fabricated a stop: %+v", stored)
		}
		usage := &execution.TerminalUsage{InputTokens: 11, OutputTokens: 4, CostUSD: .03, ToolCalls: 1}
		if err = tasks.RecordClaimUsage(ctx, claimed.ID, claimed.WorkerID, claimed.ClaimEpoch, usage); err != nil {
			t.Fatal(err)
		}
		if err = tasks.CompleteClaimed(ctx, claimed.ID, claimed.WorkerID, json.RawMessage(`{"late":true}`), "late"); err == nil {
			t.Fatal("late completion defeated parent cancellation")
		}
		if err = tasks.AcknowledgeExecutionStopped(ctx, claimed.ID, claimed.WorkerID); err != nil {
			t.Fatal(err)
		}
		stored, err = tasks.Get(ctx, subject.WorkspaceID, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != taskqueue.StatusCancelled || stored.PhysicalUsage.InputTokens != 11 || stored.UnreportedAttempts != 0 {
			t.Fatalf("stop receipt lost physical facts: %+v", stored)
		}
	}

	// A normal terminal stop closes the whole not-yet-started descendant tree in
	// the same commit, not only children reached through the cancellation API.
	parent = newTask("terminal-parent", "terminal-parent")
	if err = tasks.Enqueue(ctx, parent); err != nil {
		t.Fatal(err)
	}
	claimed, err = tasks.Claim(ctx, "terminal-worker", taskqueue.ClaimFilter{Kind: parent.Kind, IdentityKind: parent.IdentityKind})
	if err != nil || claimed == nil {
		t.Fatalf("claim terminal parent: %+v %v", claimed, err)
	}
	child = newTask("terminal-child", "terminal-child")
	child.ParentTaskID = parent.ID
	if err = tasks.Enqueue(ctx, child); err != nil {
		t.Fatal(err)
	}
	grandchild := newTask("terminal-grandchild", "terminal-grandchild")
	grandchild.ParentTaskID = child.ID
	if err = tasks.Enqueue(ctx, grandchild); err != nil {
		t.Fatal(err)
	}
	if err = tasks.CompleteClaimed(ctx, claimed.ID, claimed.WorkerID, json.RawMessage(`{"ok":true}`), "terminal-run"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{child.ID, grandchild.ID} {
		stopped, getErr := tasks.Get(ctx, subject.WorkspaceID, id)
		if getErr != nil || stopped.Status != taskqueue.StatusCancelled {
			t.Fatalf("terminal parent left descendant active: %+v %v", stopped, getErr)
		}
	}

	// Unknown outcome reconciliation reopens the same task only while its parent
	// is active, preserving its absolute deadline and cumulative usage.
	parent = newTask("resume-parent", "resume-parent")
	if err = tasks.Enqueue(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child = newTask("resume-child", "resume-child")
	child.ParentTaskID = parent.ID
	if err = tasks.Enqueue(ctx, child); err != nil {
		t.Fatal(err)
	}
	claimed, err = tasks.Claim(ctx, "resume-worker", taskqueue.ClaimFilter{Kind: child.Kind, IdentityKind: child.IdentityKind})
	if err != nil || claimed == nil {
		t.Fatalf("claim resume child: %+v %v", claimed, err)
	}
	usage := &execution.TerminalUsage{InputTokens: 5, OutputTokens: 2, CostUSD: .01}
	if err = tasks.RecordClaimUsage(ctx, claimed.ID, claimed.WorkerID, claimed.ClaimEpoch, usage); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.RecoverStale(ctx); err != nil {
		t.Fatal(err)
	}
	if err = tasks.AcknowledgeExecutionStopped(ctx, claimed.ID, claimed.WorkerID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := tasks.ReconcileTaskTx(ctx, tx, subject.WorkspaceID, claimed.ID, claimed.ClaimEpoch, taskqueue.Reconciliation{Status: taskqueue.StatusQueued})
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != claimed.ID || resumed.ClaimEpoch != claimed.ClaimEpoch || resumed.PhysicalUsage.InputTokens != 5 || resumed.UnreportedAttempts != 0 || !resumed.DeadlineAt.Equal(parentDeadline) {
		t.Fatalf("same-task recovery reset physical facts: %+v", resumed)
	}
	if err = tasks.Cancel(ctx, subject.WorkspaceID, parent.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err = tasks.Get(ctx, subject.WorkspaceID, child.ID)
	if err != nil || resumed.Status != taskqueue.StatusCancelled {
		t.Fatalf("recovered child escaped later parent stop: %+v %v", resumed, err)
	}
}
