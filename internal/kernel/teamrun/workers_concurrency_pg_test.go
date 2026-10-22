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

// concurrencyRuntime holds every execution until released and records how
// many ran at once.
type concurrencyRuntime struct {
	RuntimeRunner
	current, peak atomic.Int64
	started       chan struct{}
	release       chan struct{}
}

func (r *concurrencyRuntime) Execute(ctx context.Context, run TeamRun, task *taskqueue.Task) (RuntimeResult, error) {
	now := r.current.Add(1)
	defer r.current.Add(-1)
	for {
		peak := r.peak.Load()
		if now <= peak || r.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	r.started <- struct{}{}
	select {
	case <-r.release:
		return r.RuntimeRunner.Execute(ctx, run, task)
	case <-ctx.Done():
		return RuntimeResult{}, ctx.Err()
	}
}

// TestWorkersRunExactlyExecutorConcurrencyRealPG checks the configured
// capacity against real claims: with more claimable workflow tasks than
// capacity, both executor modes run exactly ExecutorConcurrency at once and
// then finish the rest.
func TestWorkersRunExactlyExecutorConcurrencyRealPG(t *testing.T) {
	const capacity, queued = 3, 7
	for _, pooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("pooled=%v", pooled), func(t *testing.T) {
			h := newProcessNextHarness(t)
			taskIDs := make([]string, queued)
			for i := range taskIDs {
				taskIDs[i] = h.enqueueWorkflowTask(t, fmt.Sprintf("capacity-%d", i))
			}
			h.runtime.executeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"ok":true}`)}
			runtime := &concurrencyRuntime{RuntimeRunner: h.runtime, started: make(chan struct{}, queued), release: make(chan struct{})}
			h.executor.Runtime = runtime
			workers := &Workers{Pooled: pooled, Executor: h.executor, ExecutorConcurrency: capacity, PollInterval: 20 * time.Millisecond,
				CancelGrace: &CancelGraceSweeper{Tasks: h.tasks, Transactions: h.pool, Runs: NewPGStore()},
				TimerWake:   &TimerWakeSweeper{Transactions: h.pool, Runs: NewPGStore(), Executor: h.executor}}
			workers.Start()
			defer workers.Stop()
			for range capacity {
				select {
				case <-runtime.started:
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d of %d executions started", runtime.current.Load(), capacity)
				}
			}
			// Several poll periods pass with claimable work left: none may start.
			select {
			case <-runtime.started:
				t.Fatalf("an execution beyond capacity %d started", capacity)
			case <-time.After(300 * time.Millisecond):
			}
			close(runtime.release)
			deadline := time.Now().Add(10 * time.Second)
			for _, taskID := range taskIDs {
				for {
					stored, err := h.tasks.Get(t.Context(), "workspace-1", taskID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.Status == taskqueue.StatusCompleted {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("task %s did not complete: %s", taskID, stored.Status)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			if peak := runtime.peak.Load(); peak != capacity {
				t.Fatalf("peak concurrent executions = %d, want %d", peak, capacity)
			}
		})
	}
}
