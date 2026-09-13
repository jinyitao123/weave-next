package capabilities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
)

// RuntimeTaskExecutor binds the application's claim and persistence to the
// kernel runner. It neither selects an engine nor adds an execution retry.
type RuntimeTaskExecutor struct {
	Runner *capabilityruntime.Runner
	Store  *PGStore
}

func (e RuntimeTaskExecutor) Execute(ctx context.Context, task InvocationTask) (json.RawMessage, error) {
	if e.Runner == nil || e.Store == nil {
		return nil, errors.New("capability execution dependencies are not configured")
	}
	return e.Runner.Execute(ctx, capabilityruntime.Request{
		RunKind: task.RunKind, WorkspaceID: task.WorkspaceID, InvocationID: task.InvocationID,
		Plan: task.Plan, Input: task.Input, State: task.State,
	}, invocationRecorder{PGExecutionObserver{Store: e.Store, Task: task}})
}

type invocationRecorder struct{ PGExecutionObserver }

func (r invocationRecorder) BindRuntime(ctx context.Context, runtimeID string) error {
	return r.Store.BindRuntime(ctx, r.Task, runtimeID)
}

func (r invocationRecorder) RecordRun(ctx context.Context, stepID, runID string) error {
	return r.Store.RecordStepRun(ctx, r.Task, stepID, runID)
}
