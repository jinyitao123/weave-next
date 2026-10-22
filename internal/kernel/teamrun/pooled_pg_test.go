package teamrun

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestWorkflowBatchClaimAndRenewalFencingRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	for i := 0; i < 4; i++ {
		h.enqueueWorkflowTask(t, fmt.Sprintf("batch-%d", i))
	}
	ctx := t.Context()
	filter := taskqueue.ClaimFilter{Kind: "team_workflow", IdentityKind: taskqueue.IdentityTeamWorkflow}
	tasks, err := h.tasks.ClaimBatch(ctx, "process", filter, 4)
	if err != nil || len(tasks) != 4 {
		t.Fatalf("batch claim: %d %v", len(tasks), err)
	}
	seen := map[string]bool{}
	claims := make([]taskqueue.LeaseClaim, 0, 4)
	for _, task := range tasks {
		if task.WorkerID == "" || seen[task.WorkerID] || task.ClaimEpoch != 1 {
			t.Fatalf("non-unique claim: %+v", task)
		}
		seen[task.WorkerID] = true
		claims = append(claims, taskqueue.LeaseClaim{ID: task.ID, WorkerID: task.WorkerID, Epoch: task.ClaimEpoch})
	}
	if _, err := h.tasks.RequestCancelTask(ctx, tasks[0].WorkspaceID, tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=$2 WHERE id=$1`, tasks[1].ID, h.now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claims[2].Epoch++ // an old/different generation cannot renew the owner
	renewed, err := h.tasks.RenewBatch(ctx, claims)
	if err != nil || len(renewed) != 1 || !renewed[tasks[3].ID] {
		t.Fatalf("renewed invalid owner: %v %v", renewed, err)
	}
	if err := h.tasks.CompleteClaimed(ctx, tasks[0].ID, tasks[0].WorkerID, nil, ""); err == nil {
		t.Fatal("cancelled task completed")
	}
	if err := h.tasks.CompleteClaimed(ctx, tasks[1].ID, tasks[1].WorkerID, nil, ""); err == nil {
		t.Fatal("expired task completed")
	}
	if _, err := h.tasks.ClaimBatch(ctx, "process", taskqueue.ClaimFilter{Kind: "engine_exec", IdentityKind: taskqueue.IdentityAgent}, 1); err == nil {
		t.Fatal("batch bypassed runtime claim gate")
	}
	// A new physical claim must not reuse the old process worker identity.
	if _, err := h.pool.Exec(ctx, `UPDATE weave_task_queue SET status='queued',worker_id=NULL,lease_expires_at=NULL WHERE id=$1`, tasks[3].ID); err != nil {
		t.Fatal(err)
	}
	next, err := h.tasks.ClaimBatch(ctx, "process", filter, 1)
	if err != nil || len(next) != 1 || next[0].WorkerID == tasks[3].WorkerID || next[0].ClaimEpoch != 2 {
		t.Fatalf("new claim not fenced: %v %v", next, err)
	}
	if err := h.tasks.CompleteClaimed(context.Background(), tasks[3].ID, tasks[3].WorkerID, nil, ""); err == nil {
		t.Fatal("late old execution completed new claim")
	}
}

type pooledBlockingRuntime struct {
	RuntimeRunner
	started, release, stopped chan struct{}
}

func (r *pooledBlockingRuntime) Execute(ctx context.Context, run TeamRun, task *taskqueue.Task) (RuntimeResult, error) {
	close(r.started)
	defer close(r.stopped)
	select {
	case <-r.release:
		return r.RuntimeRunner.Execute(ctx, run, task)
	case <-ctx.Done():
		return RuntimeResult{}, ctx.Err()
	}
}

type pooledCountingTasks struct {
	*taskqueue.Store
	renewals atomic.Int64
}

func (s *pooledCountingTasks) Heartbeat(context.Context, string, string) error {
	panic("pooled execution renewed one task separately")
}
func (s *pooledCountingTasks) RenewBatch(ctx context.Context, claims []taskqueue.LeaseClaim) (map[string]bool, error) {
	if len(claims) > 0 {
		s.renewals.Add(1)
	}
	return s.Store.RenewBatch(ctx, claims)
}

func TestPooledExecutorDrainsAndJoinsBeforeStoppingRealPG(t *testing.T) {
	for _, finish := range []bool{true, false} {
		t.Run(fmt.Sprint(finish), func(t *testing.T) {
			h := newProcessNextHarness(t)
			taskID := h.enqueueWorkflowTask(t, "pooled-drain", true)
			h.runtime.executeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"ok":true}`)}
			blocked := &pooledBlockingRuntime{RuntimeRunner: h.runtime, started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
			tasks := &pooledCountingTasks{Store: h.tasks}
			h.executor.Tasks = tasks
			h.executor.Runtime = blocked
			h.executor.HeartbeatInterval = 20 * time.Millisecond
			// A finishing execution gets ample drain time so completion, not the
			// timer, decides the outcome; a forced drain needs the timer to fire.
			drainTimeout := 100 * time.Millisecond
			if finish {
				drainTimeout = 5 * time.Second
			}
			workers := &Workers{Pooled: true, Executor: h.executor, ExecutorConcurrency: 2, DrainTimeout: drainTimeout, PollInterval: time.Hour,
				CancelGrace: &CancelGraceSweeper{Tasks: h.tasks, Transactions: h.pool, Runs: NewPGStore()}, TimerWake: &TimerWakeSweeper{Transactions: h.pool, Runs: NewPGStore(), Executor: h.executor}}
			workers.Start()
			defer workers.Stop()
			select {
			case <-blocked.started:
			case <-time.After(3 * time.Second):
				t.Fatal("pooled executor never started the claimed task")
			}
			stopped := make(chan struct{})
			go func() { workers.Stop(); close(stopped) }()
			select {
			case <-stopped:
				t.Fatal("stop returned before its active execution joined")
			case <-time.After(30 * time.Millisecond):
			}
			if finish {
				close(blocked.release)
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("executor did not finish its drain")
			}
			select {
			case <-blocked.stopped:
			default:
				t.Fatal("stop claimed completion while runtime still ran")
			}
			if tasks.renewals.Load() < 1 {
				t.Fatal("the active execution lost batch renewal while draining")
			}
			if finish {
				h.assertTask(t, taskID, taskqueue.StatusCompleted, "pooled-drain", "")
			} else {
				stored, err := h.tasks.Get(t.Context(), "workspace-1", taskID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status == taskqueue.StatusCompleted || stored.WorkerID != "" || stored.StoppedEpoch != stored.ClaimEpoch || stored.UnreportedAttempts == 0 {
					t.Fatalf("forced drain erased unknown outcome or omitted its stop proof: %+v", stored)
				}
			}
		})
	}
}
