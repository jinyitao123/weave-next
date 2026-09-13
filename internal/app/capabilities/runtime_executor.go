package capabilities

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
)

// RuntimeTaskExecutor binds the application's claim and persistence to the
// kernel runner. It neither selects an engine nor adds an execution retry.
type RuntimeTaskExecutor struct {
	Runner      *capabilityruntime.Runner
	Store       *PGStore
	DirectTools func(context.Context, InvocationTask) (capability.ToolStepExecutor, error)
}

func (e RuntimeTaskExecutor) Execute(ctx context.Context, task InvocationTask) (json.RawMessage, error) {
	if e.Runner == nil || e.Store == nil {
		return nil, errors.New("capability execution dependencies are not configured")
	}
	var tools capability.ToolStepExecutor
	var err error
	if len(task.Plan.Resources.Tools) > 0 {
		if e.DirectTools == nil {
			return nil, errors.New("capability direct tool executor is not configured")
		}
		tools, err = e.DirectTools(ctx, task)
		if err != nil {
			return nil, err
		}
	}
	return e.Runner.Execute(ctx, capabilityruntime.Request{
		RunKind: task.RunKind, WorkspaceID: task.WorkspaceID, InvocationID: task.InvocationID,
		Plan: task.Plan, Input: task.Input, State: task.State, Tools: tools,
	}, invocationRecorder{PGExecutionObserver{Store: e.Store, Task: task}})
}

type invocationRecorder struct{ PGExecutionObserver }

func (r invocationRecorder) BindRuntime(ctx context.Context, runtimeID string) error {
	return r.Store.BindRuntime(ctx, r.Task, runtimeID)
}

func (r invocationRecorder) RecordRun(ctx context.Context, stepID, runID string) error {
	return r.Store.RecordStepRun(ctx, r.Task, stepID, runID)
}
