package capabilityruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

// Request contains execution inputs, not application credentials or SQL state.
// Its caller owns the claimed invocation and supplies a fenced Recorder.
type Request struct {
	RunKind      string
	WorkspaceID  string
	InvocationID string
	ActivationID string
	Plan         capability.Plan
	Input        json.RawMessage
	State        capability.ExecutionState
	Tools        capability.ToolStepExecutor
}

// Recorder binds engine observations and checkpoints to the caller's claim.
// Implementations must reject expired, cancelled, or superseded claims.
type Recorder interface {
	capability.ExecutionObserver
	BindRuntime(context.Context, string) error
	RecordRun(context.Context, string, string) error
}

// Runner selects and executes existing capability paths. It does not claim
// tasks, renew leases, retry a whole invocation, or write its terminal state.
// The caller's context carries cancellation and the execution deadline.
type Runner struct {
	CanResolveModel func(context.Context, string, string) (bool, error)
	ResolveModel    func(context.Context, string) (contract.LLM, error)
	ListRuntimes    func(context.Context, string) ([]runtimes.Runtime, error)
	Remote          mcphost.RemoteEngineExecutor
	Store           loom.Store
	TerminalSink    func() (loomruntime.TerminalSink, error)
}

func (r *Runner) Execute(ctx context.Context, request Request, recorder Recorder) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || recorder == nil {
		return nil, errors.New("capability execution dependencies are not configured")
	}
	activationID := request.ActivationID
	if activationID == "" {
		activationID = request.InvocationID
	}
	ctx = execution.WithInvocationID(ctx, activationID)
	requirement := request.Plan.Runtime
	if err := r.ValidateRuntime(ctx, request.WorkspaceID, requirement); err != nil {
		return nil, err
	}
	var steps capability.StepExecutor
	if requirement.Engine != "" && requirement.Engine != "loom" {
		record, err := r.SelectRuntime(ctx, request.WorkspaceID, requirement)
		if err != nil {
			return nil, err
		}
		if err := recorder.BindRuntime(ctx, record.RuntimeID); err != nil {
			return nil, err
		}
		steps = remoteSteps{task: request, executor: r.Remote, record: record, recordRun: recorder.RecordRun}
	} else {
		if r.ResolveModel == nil || r.TerminalSink == nil {
			return nil, errors.New("capability model execution dependencies are not configured")
		}
		llm, err := r.ResolveModel(ctx, request.WorkspaceID)
		if err != nil {
			return nil, err
		}
		sink, err := r.TerminalSink()
		if err != nil {
			return nil, err
		}
		steps = LoomSteps{RunKind: request.RunKind, WorkspaceID: request.WorkspaceID,
			InvocationID: request.InvocationID, Model: requirement.Model, LLM: llm,
			Store: r.Store, TerminalSink: sink, RecordRun: recorder.RecordRun}
	}
	if len(request.Plan.Resources.Tools) > 0 {
		if request.Tools == nil {
			return nil, errors.New("capability frozen tool execution dependencies are not configured")
		}
		steps = combinedSteps{StepExecutor: steps, ToolStepExecutor: request.Tools}
	}
	result, _, err := capability.ExecutePlanResumable(ctx, request.Plan, request.Input, steps, request.State, recorder)
	return result, err
}

type combinedSteps struct {
	capability.StepExecutor
	capability.ToolStepExecutor
}
