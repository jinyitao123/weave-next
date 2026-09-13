package runtimes

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func claimRuntimeControlParent(t *testing.T, ctx context.Context, queue *taskqueue.Store, id string) (context.Context, *taskqueue.Task) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	if limit, ok := ctx.Deadline(); ok {
		deadline = limit.Add(-time.Second)
	}
	parent := &taskqueue.Task{ID: id, WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
		Source: "test", Kind: "control", Payload: json.RawMessage(`{}`), DeadlineAt: &deadline}
	if err := queue.Enqueue(ctx, parent); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, "control-worker", taskqueue.ClaimFilter{WorkspaceID: "ws", Kind: "control"})
	if err != nil || claimed == nil || claimed.ID != id {
		t.Fatalf("claim control task: %+v %v", claimed, err)
	}
	bound, err := taskqueue.BindTaskExecution(ctx, claimed, queue)
	if err != nil {
		t.Fatal(err)
	}
	return bound, claimed
}

type controlRuntimeHarness struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	queue    *taskqueue.Store
	executor *Executor
	record   *registry.AgentRecord
}

func newControlRuntimeHarness(t *testing.T) controlRuntimeHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "runtime-user"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_users(id,tenant_id,username,password) VALUES('runtime-user','ws','runtime-user','unused');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');`); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	prepared, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.Close)
	store := NewStore(prepared)
	runtime, _, err := store.Create(ctx, "ws", "Runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture", ProtocolVersion: "1", AuthMode: AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
		t.Fatal(err)
	}
	queue := taskqueue.New(prepared, nil, time.Minute)
	record := &registry.AgentRecord{WorkspaceID: "ws", ID: "agent", Name: "worker", Version: 1, Engine: engine.Claude, RuntimeID: runtime.ID, RuntimePolicyMode: "strict_pin", Model: "missing-model", FallbackModels: []string{"fallback"}}
	return controlRuntimeHarness{ctx: ctx, pool: prepared, queue: queue, executor: NewExecutor(queue, store, "", ""), record: record}
}

func (h controlRuntimeHarness) run(ctx context.Context) error {
	_, err := h.executor.ExecRemote(ctx, "ws", h.record, execution.AgentExecutionStamp{AgentID: "agent", AgentVersion: 1, ExecutionScope: execution.ScopeLegacyOrchestrator}, "one action", nil)
	return err
}

func TestRuntimeControlParentRejectsStoppedAndStaleOwnersRealPG(t *testing.T) {
	for _, stop := range []string{"cancelled", "completed", "reclaimed", "foreign_subject"} {
		t.Run(stop, func(t *testing.T) {
			h := newControlRuntimeHarness(t)
			bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "control")
			switch stop {
			case "cancelled":
				if err := h.queue.Cancel(h.ctx, "ws", parent.ID); err != nil {
					t.Fatal(err)
				}
			case "completed":
				if err := h.queue.CompleteClaimed(h.ctx, parent.ID, parent.WorkerID, json.RawMessage(`{}`), ""); err != nil {
					t.Fatal(err)
				}
			case "reclaimed":
				if _, err := h.pool.Exec(h.ctx, `UPDATE weave_task_queue SET worker_id='next-worker',claim_epoch=claim_epoch+1 WHERE id=$1`, parent.ID); err != nil {
					t.Fatal(err)
				}
			case "foreign_subject":
				bound = execution.WithSubject(bound, execution.Subject{WorkspaceID: "ws", UserID: "another-user"})
			}
			if _, err := taskqueue.BindTaskExecution(bound, parent, h.queue); err == nil {
				t.Fatal("unproven claim rebound as current task")
			}
			if err := h.run(bound); err == nil {
				t.Fatal("invalid current owner dispatched CLI")
			}
			var count int
			if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("physical tasks=%d err=%v", count, err)
			}
		})
	}
}

func TestRuntimeControlParentCancellationSerializesWithCLIAdmissionRealPG(t *testing.T) {
	h := newControlRuntimeHarness(t)
	for i := 0; i < 8; i++ {
		bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, fmt.Sprintf("control-%d", i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var runErr, cancelErr error
		go func() { defer wg.Done(); <-start; runErr = h.run(bound) }()
		go func() { defer wg.Done(); <-start; cancelErr = h.queue.Cancel(h.ctx, "ws", parent.ID) }()
		close(start)
		wg.Wait()
		if cancelErr != nil || runErr == nil {
			t.Fatalf("cancel=%v run=%v", cancelErr, runErr)
		}
		var active int
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM weave_task_queue WHERE parent_task_id=$1 AND status IN ('queued','dispatched','running')`, parent.ID).Scan(&active); err != nil || active != 0 {
			t.Fatalf("children survived parent cancellation: %d %v", active, err)
		}
		if err := h.run(bound); err == nil {
			t.Fatal("cancelled team owner dispatched another CLI")
		}
	}
}

func TestRuntimeFallbackStopsWhenControlParentStopsRealPG(t *testing.T) {
	for _, terminal := range []string{"cancel", "complete"} {
		t.Run(terminal, func(t *testing.T) {
			h := newControlRuntimeHarness(t)
			bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "control")
			done := make(chan error, 1)
			go func() { done <- h.run(execution.WithInvocationID(bound, "stop-between-models")) }()
			worker := RuntimeWorkerID("ws", h.record.RuntimeID)
			var first *taskqueue.Task
			for first == nil && h.ctx.Err() == nil {
				var err error
				first, err = h.queue.Claim(h.ctx, worker, taskqueue.ClaimFilter{WorkspaceID: "ws", Kind: "engine_exec", RuntimeID: h.record.RuntimeID})
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					time.Sleep(time.Millisecond)
				}
			}
			if first == nil {
				t.Fatal("first model attempt was not claimed")
			}
			// Keep the first receipt and parent stop in one transaction so fallback
			// cannot race ahead before the cancellation/termination becomes visible.
			tx, err := h.pool.Begin(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			receipt := CLIEngineExecResult(engine.RunResult{Status: "failed", Err: "model is not supported", RetrySafeBeforeExecution: true})
			receipt.Subject, receipt.ClaimEpoch = first.Subject, first.ClaimEpoch
			raw, _ := json.Marshal(receipt)
			if _, err = tx.Exec(h.ctx, `UPDATE weave_task_queue SET status='completed',result=$1,worker_id=NULL,lease_expires_at=NULL,completed_at=now() WHERE id=$2`, raw, first.ID); err != nil {
				_ = tx.Rollback(h.ctx)
				t.Fatal(err)
			}
			status := "cancelled"
			if terminal == "complete" {
				status = "completed"
			}
			if _, err = tx.Exec(h.ctx, `UPDATE weave_task_queue SET status=$1,worker_id=NULL,lease_expires_at=NULL,completed_at=now() WHERE id=$2`, status, parent.ID); err != nil {
				_ = tx.Rollback(h.ctx)
				t.Fatal(err)
			}
			if err = tx.Commit(h.ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("fallback continued after its owner stopped")
			}
			var count int
			if err = h.pool.QueryRow(h.ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("physical attempts=%d err=%v", count, err)
			}
		})
	}
}

func TestCurrentTaskTransactionFenceRejectsForgedAndExpiredClaimsRealPG(t *testing.T) {
	h := newControlRuntimeHarness(t)
	bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "control")
	validate := func(ctx context.Context) error {
		tx, err := h.pool.Begin(h.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(h.ctx)
		return h.queue.ValidateCurrentTaskTx(ctx, tx)
	}
	if err := validate(bound); err != nil {
		t.Fatal(err)
	}
	if err := validate(h.ctx); err == nil {
		t.Fatal("missing current task authorized a write")
	}
	current, _ := execution.CurrentTaskFromContext(bound)
	for _, field := range []string{"id", "worker", "epoch"} {
		changed := current
		switch field {
		case "id":
			changed.ID = "fabricated"
		case "worker":
			changed.WorkerID = "other-worker"
		case "epoch":
			changed.ClaimEpoch++
		}
		forged, err := execution.WithCurrentTask(h.ctx, changed)
		if err != nil {
			t.Fatal(err)
		}
		if err := validate(forged); err == nil {
			t.Fatalf("forged %s authorized a write", field)
		}
	}
	foreign := execution.WithSubject(bound, execution.Subject{WorkspaceID: "other", UserID: "runtime-user"})
	if err := validate(foreign); err == nil {
		t.Fatal("foreign workspace authorized a write")
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := validate(bound); err == nil {
		t.Fatal("expired lease authorized a write")
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE weave_task_queue SET lease_expires_at=now()+interval '2 minute' WHERE id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	h.queue = taskqueue.New(h.pool, controlFenceClock{now: parent.DeadlineAt.Add(time.Second)}, time.Minute)
	if err := validate(bound); err == nil {
		t.Fatal("expired absolute deadline authorized a write")
	}
}

func TestCurrentTaskTransactionFenceSerializesWithCancellationRealPG(t *testing.T) {
	h := newControlRuntimeHarness(t)
	bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "control")
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(h.ctx)
	if err := h.queue.ValidateCurrentTaskTx(bound, tx); err != nil {
		t.Fatal(err)
	}
	// The trusted write transaction holds the parent row. A simultaneous stop
	// must wait, proving it cannot commit between the fence and the write.
	stopCtx, cancel := context.WithTimeout(h.ctx, 80*time.Millisecond)
	err = h.queue.Cancel(stopCtx, "ws", parent.ID)
	cancel()
	if err == nil {
		t.Fatal("parent cancellation bypassed the transaction fence")
	}
	if err = tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err = h.queue.Cancel(h.ctx, "ws", parent.ID); err != nil {
		t.Fatal(err)
	}
	next, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback(h.ctx)
	if err = h.queue.ValidateCurrentTaskTx(bound, next); err == nil {
		t.Fatal("stopped owner authorized another transaction")
	}
}

type controlFenceClock struct{ now time.Time }

func (c controlFenceClock) Now() time.Time { return c.now }

func TestParentCancelIntentReachesOwnedAndQueuedDescendantsRealPG(t *testing.T) {
	for _, disconnected := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnected=%v", disconnected), func(t *testing.T) {
			h := newControlRuntimeHarness(t)
			bound, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "root-control")
			enqueue := func(ctx context.Context, id, kind, parentID string) *taskqueue.Task {
				t.Helper()
				task := &taskqueue.Task{ID: id, WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1,
					IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
					Source: "test", Kind: kind, ParentTaskID: parentID, Payload: json.RawMessage(`{}`), OutcomeSensitive: true}
				if err := h.queue.Enqueue(ctx, task); err != nil {
					t.Fatal(err)
				}
				return task
			}
			enqueue(bound, "running-child", "nested-control", parent.ID)
			running, err := h.queue.Claim(h.ctx, "nested-owner", taskqueue.ClaimFilter{WorkspaceID: "ws", Kind: "nested-control"})
			if err != nil || running == nil {
				t.Fatalf("nested claim: %+v %v", running, err)
			}
			nested, err := taskqueue.BindTaskExecution(h.ctx, running, h.queue)
			if err != nil {
				t.Fatal(err)
			}
			enqueue(nested, "queued-grandchild", "nested-work", running.ID)
			enqueue(bound, "queued-child", "nested-work", parent.ID)
			if disconnected {
				if _, err := h.pool.Exec(h.ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, parent.ID); err != nil {
					t.Fatal(err)
				}
			}
			for repeat := 0; repeat < 2; repeat++ {
				if err := h.queue.Cancel(h.ctx, "ws", parent.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"queued-child", "queued-grandchild"} {
				task, err := h.queue.Get(h.ctx, "ws", id)
				if err != nil || task.Status != taskqueue.StatusCancelled || task.WorkerID != "" {
					t.Fatalf("queued descendant escaped: %+v %v", task, err)
				}
			}
			child, err := h.queue.Get(h.ctx, "ws", running.ID)
			if err != nil || child.Status != taskqueue.StatusCancelRequested || child.WorkerID != running.WorkerID || child.ClaimEpoch != running.ClaimEpoch || child.LeaseExpiresAt == nil || !child.LeaseExpiresAt.Equal(*running.LeaseExpiresAt) {
				t.Fatalf("child stop intent lost actual owner: %+v %v", child, err)
			}
			root, err := h.queue.Get(h.ctx, "ws", parent.ID)
			if err != nil || root.Status != taskqueue.StatusCancelRequested || root.WorkerID != parent.WorkerID {
				t.Fatalf("parent exit was fabricated: %+v %v", root, err)
			}
			usage := &execution.TerminalUsage{InputTokens: 7, OutputTokens: 2, ToolCalls: 1}
			if err := h.queue.RecordClaimUsage(h.ctx, running.ID, running.WorkerID, running.ClaimEpoch, usage); err != nil {
				t.Fatal(err)
			}
			if err := h.queue.CompleteClaimed(h.ctx, running.ID, running.WorkerID, json.RawMessage(`{"late":true}`), ""); err == nil {
				t.Fatal("late completion defeated stop intent")
			}
			if err := h.queue.AcknowledgeExecutionStopped(h.ctx, running.ID, running.WorkerID); err != nil {
				t.Fatal(err)
			}
			if err := h.queue.Cancel(h.ctx, "ws", parent.ID); err != nil {
				t.Fatal(err)
			}
			child, err = h.queue.Get(h.ctx, "ws", running.ID)
			if err != nil || child.Status != taskqueue.StatusCancelled || child.WorkerID != "" || child.PhysicalUsage.InputTokens != 7 || child.StoppedEpoch != running.ClaimEpoch {
				t.Fatalf("child exit receipt lost physical evidence: %+v %v", child, err)
			}
		})
	}
}

func TestUnstartedClaimStopDoesNotReleaseAnotherOwnerRealPG(t *testing.T) {
	h := newControlRuntimeHarness(t)
	_, parent := claimRuntimeControlParent(t, h.ctx, h.queue, "control")
	if err := h.queue.Cancel(h.ctx, "ws", parent.ID); err != nil {
		t.Fatal(err)
	}
	stale := *parent
	stale.ClaimEpoch++
	if err := h.queue.AcknowledgeUnstartedClaim(h.ctx, &stale); err != nil {
		t.Fatal(err)
	}
	foreign := *parent
	foreign.Subject.UserID = "other"
	if err := h.queue.AcknowledgeUnstartedClaim(h.ctx, &foreign); err != nil {
		t.Fatal(err)
	}
	pending, err := h.queue.Get(h.ctx, "ws", parent.ID)
	if err != nil || pending.Status != taskqueue.StatusCancelRequested || pending.WorkerID != parent.WorkerID {
		t.Fatalf("unproven stop cleared owner: %+v %v", pending, err)
	}
	if err := h.queue.AcknowledgeUnstartedClaim(h.ctx, parent); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.queue.Get(h.ctx, "ws", parent.ID)
	if err != nil || stopped.Status != taskqueue.StatusCancelled || stopped.WorkerID != "" || stopped.StoppedEpoch != parent.ClaimEpoch {
		t.Fatalf("unstarted claim stranded owner: %+v %v", stopped, err)
	}
}
