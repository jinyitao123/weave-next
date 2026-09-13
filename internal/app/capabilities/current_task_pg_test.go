package capabilities

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type currentTaskExecutorFunc func(context.Context, InvocationTask) (json.RawMessage, error)

func (f currentTaskExecutorFunc) Execute(ctx context.Context, task InvocationTask) (json.RawMessage, error) {
	return f(ctx, task)
}

func TestCapabilityWorkerBindsCurrentPhysicalTaskRealPG(t *testing.T) {
	_, store, invocation := executionFixture(t)
	observed := make(chan error, 1)
	executor := currentTaskExecutorFunc(func(ctx context.Context, task InvocationTask) (json.RawMessage, error) {
		current, ok := execution.CurrentTaskFromContext(ctx)
		if !ok || current.ID != task.TaskID || current.WorkspaceID != task.WorkspaceID || current.Subject.UserID != task.ActorUserID || current.WorkerID != task.WorkerID || current.ClaimEpoch != task.ClaimEpoch {
			err := fmt.Errorf("capability lost current physical task: %+v", current)
			observed <- err
			return nil, err
		}
		observed <- nil
		return json.RawMessage(`{"ok":true}`), nil
	})
	worker := taskqueue.NewWorker(store.queue, 1)
	if err := worker.Register("capability_invocation", taskqueue.IdentityCapability, PlatformTaskHandler{Store: store, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	worker.Start()
	defer worker.Stop()
	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capability executor not called")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	task, err := store.queue.AwaitTerminal(ctx, invocation.WorkspaceID, invocation.TaskID, 5*time.Second)
	if err != nil || task.Status != taskqueue.StatusCompleted {
		t.Fatalf("capability task did not complete: %+v %v", task, err)
	}
}
