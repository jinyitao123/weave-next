package capabilities

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestCapabilityStopReceiptReconcilesSameTaskWithoutResettingDeadlineOrUsageRealPG(t *testing.T) {
	pool, store, invocation := executionFixture(t)
	ctx := capabilityTestContext(t.Context(), invocation.WorkspaceID)
	physical, err := store.queue.Claim(ctx, "worker-one", taskqueue.ClaimFilter{
		Kind: "capability_invocation", IdentityKind: taskqueue.IdentityCapability,
	})
	if err != nil || physical == nil {
		t.Fatalf("claim platform task: %+v %v", physical, err)
	}
	originalDeadline := *physical.DeadlineAt
	usage := execution.TerminalUsage{InputTokens: 17, OutputTokens: 5, CostUSD: 0.02, ToolCalls: 1}
	if err := store.queue.RecordClaimUsage(ctx, physical.ID, physical.WorkerID, physical.ClaimEpoch, &usage); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, physical.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := store.queue.RecoverStale(ctx); err != nil || recovered != 1 {
		t.Fatalf("recover stale=%d err=%v", recovered, err)
	}
	unknown, err := store.GetInvocation(ctx, invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID)
	if err != nil || unknown.Status != "reconciling" {
		t.Fatalf("unknown outcome projection=%+v err=%v", unknown, err)
	}
	if _, err := store.ReconcileTask(ctx, invocation.WorkspaceID, invocation.InvocationID, Reconciliation{Decision: ReconcileRetrySafe}); err == nil {
		t.Fatal("retry admitted before the physical worker stopped")
	}
	if err := store.queue.AcknowledgeExecutionStopped(ctx, physical.ID, physical.WorkerID); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileTask(ctx, invocation.WorkspaceID, invocation.InvocationID, Reconciliation{Decision: ReconcileRetrySafe})
	if err != nil || reconciled.Status != "queued" || reconciled.TaskID != physical.ID {
		t.Fatalf("reconcile same task=%+v err=%v", reconciled, err)
	}
	resumed, err := store.queue.Get(ctx, invocation.WorkspaceID, physical.ID)
	if err != nil || resumed.Status != taskqueue.StatusQueued || resumed.DeadlineAt == nil || !resumed.DeadlineAt.Equal(originalDeadline) {
		t.Fatalf("absolute deadline changed across reconciliation: %+v err=%v", resumed, err)
	}
	if resumed.PhysicalUsage != usage || resumed.UnreportedAttempts != 0 || resumed.ClaimEpoch != physical.ClaimEpoch {
		t.Fatalf("physical accounting reset across reconciliation: %+v", resumed)
	}
	second, err := store.queue.Claim(ctx, "worker-two", taskqueue.ClaimFilter{Kind: "capability_invocation", IdentityKind: taskqueue.IdentityCapability})
	if err != nil || second == nil || second.ID != physical.ID || second.ClaimEpoch != physical.ClaimEpoch+1 {
		t.Fatalf("same task was not resumed as a new attempt: %+v err=%v", second, err)
	}
	if second.DeadlineAt == nil || second.DeadlineAt.After(originalDeadline.Add(time.Millisecond)) {
		t.Fatalf("new claim extended the absolute deadline: %+v", second.DeadlineAt)
	}
}

func TestCapabilityPlatformQueueClaimsOnceAndConfirmsCancellationRealPG(t *testing.T) {
	_, store, invocation := executionFixture(t)
	ctx := capabilityTestContext(t.Context(), invocation.WorkspaceID)
	var wg sync.WaitGroup
	claimed := make(chan *taskqueue.Task, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, _ := store.queue.Claim(ctx, "worker-concurrent", taskqueue.ClaimFilter{Kind: "capability_invocation", IdentityKind: taskqueue.IdentityCapability})
			if task != nil {
				claimed <- task
			}
		}()
	}
	wg.Wait()
	close(claimed)
	if len(claimed) != 1 {
		t.Fatalf("physical claims=%d", len(claimed))
	}
	physical := <-claimed
	if err := store.queue.Cancel(ctx, invocation.WorkspaceID, physical.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.queue.AcknowledgeExecutionStopped(ctx, physical.ID, physical.WorkerID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.GetInvocation(ctx, invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel stop receipt=%+v err=%v", cancelled, err)
	}
}

type cancelledExecutor struct{}

func (cancelledExecutor) Execute(ctx context.Context, _ InvocationTask) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCapabilityHumanPauseResumesTheSamePlatformTaskRealPG(t *testing.T) {
	_, store, invocation := executionFixture(t)
	ctx := capabilityTestContext(t.Context(), invocation.WorkspaceID)
	task, claimed, err := store.ClaimTask(ctx)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	physical, err := store.queue.Get(ctx, invocation.WorkspaceID, invocation.TaskID)
	if err != nil || physical.DeadlineAt == nil {
		t.Fatal(err)
	}
	deadline := *physical.DeadlineAt
	if _, err := store.CompleteTask(ctx, task, nil, &capability.PauseError{StepID: "approval", Title: "确认", Schema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.GetInvocation(ctx, invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID)
	if err != nil || waiting.Status != "waiting" {
		t.Fatalf("waiting=%+v err=%v", waiting, err)
	}
	resumed, err := store.ResumeHuman(ctx, invocation.WorkspaceID, invocation.ApplicationID, invocation.InvocationID, "approval", json.RawMessage(`{"approved":true}`))
	if err != nil || resumed.Status != "queued" || resumed.TaskID != invocation.TaskID {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	queued, err := store.queue.Get(ctx, invocation.WorkspaceID, invocation.TaskID)
	if err != nil || queued.Status != taskqueue.StatusQueued || queued.DeadlineAt == nil || !queued.DeadlineAt.Equal(deadline) {
		t.Fatalf("human resume changed physical task or deadline: %+v err=%v", queued, err)
	}
}
