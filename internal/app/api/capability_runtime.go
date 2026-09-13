package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
)

type capabilityRuntime struct{ server *Server }

func (s *Server) validateCapabilityRuntime(ctx context.Context, workspace string, requirement capability.RuntimeRequirement) error {
	if requirement.Engine != "loom" || requirement.Pool != "" || len(requirement.Capabilities) > 0 {
		return errors.New("capability_runtime_unsupported")
	}
	if s.Models == nil || requirement.Model == "" {
		return errors.New("capability_model_unavailable")
	}
	available, err := s.Models.CanResolve(ctx, workspace, requirement.Model)
	if err != nil || !available {
		return errors.New("capability_model_unavailable")
	}
	return nil
}

func (e capabilityRuntime) Execute(ctx context.Context, task capabilities.InvocationTask) (json.RawMessage, error) {
	s := e.server
	requirement := task.Plan.Runtime
	if err := s.validateCapabilityRuntime(ctx, task.WorkspaceID, requirement); err != nil {
		return nil, err
	}
	llm, err := s.Models.ForWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	sink, err := s.rootTerminalSink()
	if err != nil {
		return nil, err
	}
	steps := capabilityruntime.LoomSteps{
		WorkspaceID: task.WorkspaceID, InvocationID: task.InvocationID, Model: requirement.Model, LLM: llm, Store: s.Store, TerminalSink: sink,
		RecordRun: func(ctx context.Context, step, run string) error {
			tag, err := s.Pool.Exec(ctx, `INSERT INTO weave_capability_step_runs(workspace_id,invocation_id,step_id,run_id)
    SELECT i.workspace_id,i.invocation_id,$3,$4
    FROM weave_capability_invocations i JOIN weave_capability_invocation_tasks t ON t.task_id=i.task_id
    WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.status='running' AND t.status='running' AND t.claim_token=$5 AND t.deadline_at>now()`,
				task.WorkspaceID, task.InvocationID, step, run, task.ClaimToken)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return capabilities.ErrClaimLost
			}
			return nil
		},
	}
	return capability.ExecutePlan(ctx, task.Plan, task.Input, steps)
}

// Worker lifetime belongs to Server.Start; no untracked goroutines survive shutdown.
func (s *Server) serveCapabilityTasks(ctx context.Context) {
	store := capabilities.NewPGStore(s.Pool)
	for ctx.Err() == nil {
		processed, err := capabilities.RunOne(ctx, store, capabilityRuntime{s})
		if err != nil && ctx.Err() == nil {
			slog.Error("capability worker failed", "error", fmt.Sprint(err))
		}
		if processed && err == nil {
			continue
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
