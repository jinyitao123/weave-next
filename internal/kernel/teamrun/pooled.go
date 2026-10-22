package teamrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type pooledTaskStore interface {
	ClaimBatch(context.Context, string, taskqueue.ClaimFilter, int) ([]*taskqueue.Task, error)
	RenewBatch(context.Context, []taskqueue.LeaseClaim) (map[string]bool, error)
	ReadyWorkspaces(context.Context) ([]string, error)
	ListenWorkflowTasks(context.Context, func(string, string)) error
}
type pooledLeaseKey struct{}
type activeWorkflowClaim struct {
	task   *taskqueue.Task
	cancel context.CancelCauseFunc
	ctx    context.Context
	// leaseUntil is a local, conservative bound on the durable lease: it is
	// measured from before the claim or renewal statement was sent.
	leaseUntil time.Time
	ttl        time.Duration
}

// pooledDefaultLeaseTTL bounds a claim whose row did not report its lease.
const pooledDefaultLeaseTTL = 60 * time.Second

// pooledFillDelay coalesces completions and notifications into one claim pass.
const pooledFillDelay = 20 * time.Millisecond

// pooledBusyPoll is the polling period while the pool has work; measured on
// 2026-10-21, a 2s period here halved decide throughput at 200 lanes because
// some claimable transitions raise no notification.
const pooledBusyPoll = 200 * time.Millisecond

// pooledWorkspaceRefresh bounds how stale the ready-workspace list may be.
const pooledWorkspaceRefresh = time.Second

func claimLeaseTTL(task *taskqueue.Task) time.Duration {
	if task.StartedAt != nil && task.LeaseExpiresAt != nil {
		if ttl := task.LeaseExpiresAt.Sub(*task.StartedAt); ttl > 0 {
			return ttl
		}
	}
	return pooledDefaultLeaseTTL
}

type taskNotification struct{ channel, payload string }

// One claimant and heartbeat service own only the in-process claim registry.
// All execution, terminal transitions and stop receipts remain in Executor.
func (workers *Workers) pooledLoop(stop context.Context, capacity int) {
	store, ok := workers.Executor.Tasks.(pooledTaskStore)
	if !ok {
		slog.Error("pooled executor requires a batch task store")
		return
	}
	process := "teamrun:" + uuid.NewString()
	execCtx, cancelExecutions := context.WithCancelCause(context.Background())
	defer cancelExecutions(context.Canceled)
	if workers.Executor.RuntimeRecords != nil {
		batcher, err := loomruntime.NewAttemptLeaseBatcher(workers.Executor.RuntimeRecords)
		if err != nil {
			slog.Error("configure attempt lease batching", "error", err)
			return
		}
		defer batcher.Close()
		execCtx = loomruntime.WithAttemptLeaseBatcher(execCtx, batcher)
	}
	listenCtx, cancelListener := context.WithCancel(context.Background())
	defer cancelListener()
	notifications := make(chan taskNotification, 64)
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		for listenCtx.Err() == nil {
			err := store.ListenWorkflowTasks(listenCtx, func(channel, payload string) {
				select {
				case notifications <- taskNotification{channel, payload}:
				case <-listenCtx.Done():
				}
			})
			if listenCtx.Err() != nil {
				return
			}
			slog.Warn("workflow notification listener reconnecting", "error", err)
			timer := time.NewTimer(time.Second)
			select {
			case <-listenCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	defer func() { cancelListener(); <-listenerDone }()
	active := make(map[string]activeWorkflowClaim)
	completed := make(chan string, capacity)
	// Notifications are the primary wake-up. Polling covers transitions that
	// raise none, such as a delayed retry becoming available or a lost
	// notification: it stays fast while the pool has work and backs off to the
	// idle interval once nothing runs and nothing was claimed.
	idlePoll := workers.PollInterval
	if idlePoll <= 0 {
		idlePoll = 2 * time.Second
	}
	busyPoll := min(pooledBusyPoll, idlePoll)
	pollDelay := busyPoll
	poll := time.NewTimer(pollDelay)
	defer poll.Stop()
	schedulePoll := func(busy bool) {
		if busy {
			pollDelay = busyPoll
		} else {
			pollDelay = min(pollDelay*2, idlePoll)
		}
		poll.Reset(pollDelay)
	}
	// The ready-workspace list is a full read of queued work; refresh it at
	// most once per pooledWorkspaceRefresh and learn new workspaces from the
	// notification payload in between. A stale entry costs one indexed claim.
	var readyCache []string
	var readyAt time.Time
	readyWorkspaces := func(ctx context.Context) ([]string, error) {
		if len(readyCache) > 0 && time.Since(readyAt) < pooledWorkspaceRefresh {
			return readyCache, nil
		}
		workspaces, err := store.ReadyWorkspaces(ctx)
		if err == nil {
			readyCache, readyAt = workspaces, time.Now()
		}
		return workspaces, err
	}
	noteWorkspace := func(workspace string) {
		if workspace != "" && !slices.Contains(readyCache, workspace) {
			readyCache = append(readyCache, workspace)
		}
	}
	var fillSoon <-chan time.Time
	var fillTimer *time.Timer
	defer func() {
		if fillTimer != nil {
			fillTimer.Stop()
		}
	}()
	requestFill := func() {
		if fillTimer == nil {
			fillTimer = time.NewTimer(pooledFillDelay)
			fillSoon = fillTimer.C
		}
	}
	interval := workers.Executor.HeartbeatInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	stopCh := stop.Done()
	var drain <-chan time.Time
	var drainTimer *time.Timer
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	draining, cursor := false, 0
	fill := func() (claimed int) {
		if draining || len(active) >= capacity {
			return 0
		}
		ctx, cancel := context.WithTimeout(execCtx, 5*time.Second)
		defer cancel()
		workspaces, err := readyWorkspaces(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("list workflow workspaces", "error", err)
			}
			return 0
		}
		if len(workspaces) == 0 {
			return 0
		}
		counts := make(map[string]int)
		for _, claim := range active {
			counts[claim.task.WorkspaceID]++
		}
		fairShare := (capacity + len(workspaces) - 1) / len(workspaces)
		start := cursor % len(workspaces)
		for offset := 0; offset < len(workspaces) && len(active) < capacity; offset++ {
			workspace := workspaces[(start+offset)%len(workspaces)]
			for counts[workspace] < fairShare && len(active) < capacity {
				limit := min(64, capacity-len(active), fairShare-counts[workspace])
				if limit < 1 {
					continue
				}
				claimedAt := time.Now()
				tasks, err := store.ClaimBatch(ctx, process, taskqueue.ClaimFilter{Kind: "team_workflow", IdentityKind: taskqueue.IdentityTeamWorkflow, WorkspaceID: workspace}, limit)
				if err != nil {
					slog.Error("claim workflow batch", "error", err)
					break
				}
				counts[workspace] += len(tasks)
				claimed += len(tasks)
				for _, task := range tasks {
					taskCtx, cancel := context.WithCancelCause(context.WithValue(execCtx, pooledLeaseKey{}, true))
					ttl := claimLeaseTTL(task)
					active[task.ID] = activeWorkflowClaim{task: task, cancel: cancel, ctx: taskCtx, ttl: ttl, leaseUntil: claimedAt.Add(ttl)}
					go func() {
						err := workers.Executor.processClaimedWorkflowTask(taskCtx, task, task.WorkerID)
						if err != nil && taskCtx.Err() == nil {
							slog.Error("pooled workflow execution", "task", task.ID, "error", err)
						}
						completed <- task.ID
					}()
				}
				if len(tasks) < limit {
					break
				}
			}
		}
		cursor++
		return claimed
	}
	fill()
	for {
		if draining && len(active) == 0 {
			return
		}
		select {
		case <-stopCh:
			draining = true
			stopCh = nil
			if workers.DrainTimeout <= 0 {
				cancelExecutions(context.Canceled)
			} else {
				drainTimer = time.NewTimer(workers.DrainTimeout)
				drain = drainTimer.C
			}
		case <-drain:
			drain = nil
			cancelExecutions(context.Canceled)
		case id := <-completed:
			claim := active[id]
			stopping := context.Cause(claim.ctx) != nil
			claim.cancel(context.Canceled)
			delete(active, id)
			if stopping && workers.CancelGrace != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := workers.CancelGrace.Sweep(ctx)
				cancel()
				if err != nil {
					slog.Warn("stop receipt confirmation deferred to sweeper", "error", err)
				}
			}
			requestFill()
		case event := <-notifications:
			if event.channel == "weave_task_cancel" {
				// An empty payload names no run; the next renewal finds the
				// cancellation through the status predicate instead.
				if event.payload == "" {
					continue
				}
				for _, claim := range active {
					if claim.task.RunSnapshotID == event.payload {
						claim.cancel(errTaskLeaseLost)
					}
				}
			} else {
				noteWorkspace(event.payload)
				requestFill()
			}
		case <-fillSoon:
			fillTimer, fillSoon = nil, nil
			if fill() > 0 && pollDelay > busyPoll {
				// Work arrived while polling had backed off.
				schedulePoll(true)
			}
		case <-poll.C:
			claimed := fill()
			schedulePoll(claimed > 0 || len(active) > 0)
		case <-heartbeat.C:
			if len(active) == 0 {
				continue
			}
			claims := make([]taskqueue.LeaseClaim, 0, len(active))
			for _, claim := range active {
				claims = append(claims, taskqueue.LeaseClaim{ID: claim.task.ID, WorkerID: claim.task.WorkerID, Epoch: claim.task.ClaimEpoch})
			}
			renewStart := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			renewed, err := store.RenewBatch(ctx, claims)
			cancel()
			if err != nil {
				slog.Warn("renew workflow leases", "claims", len(claims), "error", err)
			}
			// The next attempt starts one interval from now and may itself take
			// the full statement timeout.
			for id, cause := range reconcileRenewal(active, renewed, err, renewStart, time.Now().Add(interval+5*time.Second)) {
				active[id].cancel(cause)
			}
		}
	}
}

// reconcileRenewal applies one batch renewal to the claim registry and returns
// the claims that must stop, with the cause. A returned row proves ownership
// and extends the local lease bound; a missing row proves loss (cancellation,
// another claimant or expiry). A failed statement proves nothing, so a claim
// stops only when its lease would expire before the next attempt completes.
func reconcileRenewal(active map[string]activeWorkflowClaim, renewed map[string]bool, renewErr error, renewStart, nextAttemptDone time.Time) map[string]error {
	lost := make(map[string]error)
	for id, claim := range active {
		switch {
		case renewErr != nil:
			if !claim.leaseUntil.After(nextAttemptDone) {
				lost[id] = fmt.Errorf("%w: renewal unavailable: %v", errTaskLeaseLost, renewErr)
			}
		case renewed[id]:
			claim.leaseUntil = renewStart.Add(claim.ttl)
			active[id] = claim
		default:
			lost[id] = errTaskLeaseLost
		}
	}
	return lost
}

func pooledExecution(ctx context.Context, execute func(context.Context) (RuntimeResult, error)) (RuntimeResult, error) {
	result, err := execute(ctx)
	if errors.Is(context.Cause(ctx), errTaskLeaseLost) {
		err = errors.Join(err, context.Cause(ctx))
	}
	return result, err
}
