package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// Handler executes one claimed task; the worker alone controls the queue lease
// and terminal state. A continuation yields the same frozen task for later work.
type Handler interface {
	ExecuteTask(context.Context, Task) (TaskResult, error)
}

type TaskResult struct {
	// Pause settles this physical invocation; business state determines later resumption.
	Pause         bool
	Usage         *execution.TerminalUsage
	Result        json.RawMessage
	RunID         string
	Continue      bool
	ContinueAfter time.Duration
}

type registration struct {
	kind     string
	identity IdentityKind
	handler  Handler
}

// Worker routes typed tasks through one shared claim, lease and cancellation loop.
type Worker struct {
	store         *Store
	handlers      []registration
	concurrency   int
	pollInterval  time.Duration
	onLegTerminal func(context.Context, string, string)
	mu            sync.Mutex
	cancel        context.CancelFunc
	cancellers    map[string]context.CancelFunc
	pollWG        sync.WaitGroup
}

func NewWorker(store *Store, concurrency int) *Worker {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Worker{store: store, concurrency: concurrency, pollInterval: 2 * time.Second, cancellers: make(map[string]context.CancelFunc)}
}

// Register installs a handler before startup. Identity is part of the route,
// so a handler cannot accidentally claim another product's execution contract.
func (w *Worker) Register(kind string, identity IdentityKind, handler Handler) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return errors.New("task worker has already started")
	}
	if kind == "" || identity == "" || handler == nil || kind == "engine_exec" {
		return errors.New("invalid local task handler")
	}
	for _, entry := range w.handlers {
		if entry.kind == kind {
			return fmt.Errorf("task handler %q already registered", kind)
		}
	}
	w.handlers = append(w.handlers, registration{kind, identity, handler})
	return nil
}

func (w *Worker) SetOnLegTerminal(hook func(context.Context, string, string)) { w.onLegTerminal = hook }

func (w *Worker) Start() {
	w.mu.Lock()
	if w.cancel != nil {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.mu.Unlock()
	for i := 0; i < w.concurrency; i++ {
		w.pollWG.Add(1)
		go w.pollLoop(ctx, i)
	}
}

// Stop interrupts handlers and stops polling. A handler which has not returned
// keeps its durable owner; shutdown cannot fabricate a physical stop receipt.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.pollWG.Wait()
}

func (w *Worker) CancelTask(ctx context.Context, workspaceID, id string) error {
	if err := w.store.Cancel(ctx, workspaceID, id); err != nil {
		return err
	}
	w.mu.Lock()
	cancel := w.cancellers[id]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (w *Worker) pollLoop(ctx context.Context, slot int) {
	defer w.pollWG.Done()
	next := slot
	for ctx.Err() == nil {
		var task *Task
		var selected Handler
		for i := 0; i < len(w.handlers); i++ {
			entry := w.handlers[(next+i)%len(w.handlers)]
			// A unique worker ID per claim also fences any late result after a resume.
			claimed, err := w.store.Claim(ctx, uuid.NewString(), ClaimFilter{Kind: entry.kind, IdentityKind: entry.identity})
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("task claim failed", "kind", entry.kind, "error", err)
				}
				break
			}
			if claimed != nil {
				task = claimed
				selected = entry.handler
				next = (next + i + 1) % len(w.handlers)
				break
			}
		}
		if task == nil {
			if !waitForPoll(ctx, w.pollInterval) {
				return
			}
			continue
		}
		done := make(chan struct{})
		go func() { defer close(done); w.executeTask(ctx, *task, selected) }()
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
	}
}

func waitForPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) executeTask(workerCtx context.Context, task Task, handler Handler) {
	defer func() {
		if task.TaskGroupID != "" && w.onLegTerminal != nil {
			w.onLegTerminal(context.Background(), task.WorkspaceID, task.TaskGroupID)
		}
	}()
	boundCtx, err := BindTaskSubject(workerCtx, &task)
	if err != nil {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, err.Error())
		return
	}
	execCtx, cancel := context.WithCancel(boundCtx)
	if task.DeadlineAt != nil {
		cancel()
		execCtx, cancel = context.WithDeadline(boundCtx, *task.DeadlineAt)
	}
	if task.SubtaskDeadlineAt != nil {
		limited, done := context.WithDeadline(execCtx, *task.SubtaskDeadlineAt)
		previous := cancel
		cancel = func() { done(); previous() }
		execCtx = limited
	}
	w.mu.Lock()
	w.cancellers[task.ID] = cancel
	w.mu.Unlock()
	defer func() { cancel(); w.mu.Lock(); delete(w.cancellers, task.ID); w.mu.Unlock() }()
	// Recheck admission after installing the cancellation hook and before any side effect.
	if err := w.store.Heartbeat(workerCtx, task.ID, task.WorkerID); err != nil {
		_ = w.store.AcknowledgeExecutionStopped(context.Background(), task.ID, task.WorkerID)
		return
	}
	if execCtx.Err() != nil {
		_ = w.store.failClaimed(context.Background(), task.ID, task.WorkerID, "execution deadline exceeded")
		return
	}
	type outcome struct {
		result TaskResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func(callCtx context.Context) {
		result, err := handler.ExecuteTask(callCtx, task)
		outcomes <- outcome{result, err}
	}(execCtx)
	interval := w.store.leaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var stopped bool
	var leaseLost bool
	for {
		select {
		case <-ticker.C:
			if stopped {
				continue
			}
			if err := w.store.Heartbeat(context.Background(), task.ID, task.WorkerID); err != nil {
				leaseLost = true
				stopped = true
				cancel()
			}
		case <-execCtx.Done():
			// Keep waiting for the handler's physical return, without renewing its lease.
			stopped = true
			execCtx = context.WithoutCancel(execCtx)
		case out := <-outcomes:
			finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer finishCancel()
			if err := w.store.RecordClaimUsage(finishCtx, task.ID, task.WorkerID, task.ClaimEpoch, out.result.Usage); err != nil {
				slog.Error("task usage receipt rejected", "task_id", task.ID, "error", err)
			}
			if stopped || workerCtx.Err() != nil {
				if !leaseLost {
					_ = w.store.failClaimed(finishCtx, task.ID, task.WorkerID, "execution interrupted")
				}
				_ = w.store.AcknowledgeExecutionStopped(finishCtx, task.ID, task.WorkerID)
				return
			}
			if out.err != nil {
				var retryable retryableTaskError
				if errors.As(out.err, &retryable) && retryable.RetryTask() {
					if err := w.store.requeueClaimed(finishCtx, task.ID, task.WorkerID, 0); err != nil {
						slog.Error("task continuation failed", "task_id", task.ID, "error", err)
					}
				} else {
					_ = w.store.failClaimed(finishCtx, task.ID, task.WorkerID, out.err.Error())
				}
				// A concurrent cancellation wins over either result. Only now can it be acknowledged.
				_ = w.store.AcknowledgeExecutionStopped(finishCtx, task.ID, task.WorkerID)
				return
			}
			if out.result.Continue && !out.result.Pause {
				if err := w.store.requeueClaimed(finishCtx, task.ID, task.WorkerID, out.result.ContinueAfter); err != nil {
					slog.Error("task continuation failed", "task_id", task.ID, "error", err)
				}
			} else if err := w.store.completeClaimed(finishCtx, task.ID, task.WorkerID, out.result.Result, out.result.RunID); err != nil {
				slog.Warn("task completion rejected", "task_id", task.ID, "error", err)
			}
			_ = w.store.AcknowledgeExecutionStopped(finishCtx, task.ID, task.WorkerID)
			return
		}
	}
}
