package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type capabilityRuntime struct{ server *Server }

func (s *Server) validateCapabilityRuntime(ctx context.Context, workspace string, requirement capability.RuntimeRequirement) error {
	engine := strings.TrimSpace(requirement.Engine)
	if engine == "" || engine == "loom" {
		if s.Models == nil || requirement.Model == "" {
			return errors.New("capability_model_unavailable")
		}
		available, err := s.Models.CanResolve(ctx, workspace, requirement.Model)
		if err != nil || !available {
			return errors.New("capability_model_unavailable")
		}
		return nil
	}
	if engine != "codex" && engine != "claude" && engine != "opencode" {
		return errors.New("capability_runtime_unsupported")
	}
	if s.Runtimes == nil || s.engineExecutorFor(true) == nil {
		return errors.New("capability_runtime_unavailable")
	}
	_, err := s.selectCapabilityRuntime(ctx, workspace, requirement)
	if err != nil {
		return errors.New("capability_runtime_unavailable")
	}
	return nil
}

func (s *Server) selectCapabilityRuntime(ctx context.Context, workspace string, requirement capability.RuntimeRequirement) (*registry.AgentRecord, error) {
	items, err := s.Runtimes.List(ctx, workspace)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !item.Enabled || !item.Online || item.RevokedAt != nil || item.HealthStatus == "quarantined" || item.ActiveSlots >= item.TotalSlots {
			continue
		}
		if requirement.Pool != "" && item.PoolID != requirement.Pool && item.ID != requirement.Pool {
			continue
		}
		available := false
		for _, engine := range item.Engines {
			if engine == requirement.Engine {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		sum := sha256.Sum256([]byte(workspace + "\x00" + item.ID + "\x00" + requirement.Engine))
		return &registry.AgentRecord{ID: fmt.Sprintf("capability-runtime-%x", sum[:12]), Version: 1, Name: fmt.Sprintf("capability-%x", sum[:8]), WorkspaceID: workspace, Engine: requirement.Engine, Model: requirement.Model, RuntimeID: item.ID, RuntimePolicyMode: "strict_pin", RuntimePoolID: item.PoolID, Role: "worker", Permissions: registry.PermissionConfig{Allow: []string{"*"}}}, nil
	}
	return nil, errors.New("no eligible capability runtime")
}

type remoteCapabilitySteps struct {
	task      capabilities.InvocationTask
	executor  mcphost.RemoteEngineExecutor
	record    *registry.AgentRecord
	recordRun func(context.Context, string, string) error
}

func (e remoteCapabilitySteps) ExecuteStep(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
	structured, ok := e.executor.(mcphost.StructuredRemoteEngineExecutor)
	if !ok {
		return nil, errors.New("remote runtime does not support structured results")
	}
	rec := *e.record
	rec.Name = rec.Name + "-" + step.ID
	rec.DisplayName = step.RoleName
	rec.Spec.SystemPrompt = step.RoleName + "\n" + step.RoleDescription + "\n" + step.Instruction
	schema := step.OutputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	rec.OutputSchema = &schema
	prompt := fmt.Sprintf("你是能力团队中的%s。职责：%s\n任务：%s\n输入：%s\n完成实际工作后，只返回符合输出结构的 JSON。", step.RoleName, step.RoleDescription, step.Instruction, input)
	result, err := structured.ExecRemoteStructured(ctx, e.task.WorkspaceID, &rec, execution.AgentExecutionStamp{AgentID: rec.ID, AgentVersion: rec.Version, ExecutionScope: execution.ScopeTeamWorkerLeaf, RunSnapshotID: e.task.InvocationID}, prompt, nil, schema)
	if err != nil {
		return nil, err
	}
	runID := result.SessionID
	if len(result.Attempts) > 0 {
		runID = result.Attempts[len(result.Attempts)-1].AttemptID
	}
	if runID != "" {
		if err := e.recordRun(ctx, step.ID, runID); err != nil {
			return nil, err
		}
	}
	raw := json.RawMessage(result.Output)
	if !json.Valid(raw) {
		return nil, errors.New("remote runtime returned invalid JSON")
	}
	return raw, nil
}

func (e remoteCapabilitySteps) ExecuteTool(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
	worker := step
	worker.Kind = capability.StepWorker
	worker.Instruction = "Use the runtime tool named " + step.ToolID + " with the supplied input. Perform the real operation and return its result."
	return e.ExecuteStep(ctx, worker, input)
}

func (e capabilityRuntime) Execute(ctx context.Context, task capabilities.InvocationTask) (json.RawMessage, error) {
	s := e.server
	requirement := task.Plan.Runtime
	if err := s.validateCapabilityRuntime(ctx, task.WorkspaceID, requirement); err != nil {
		return nil, err
	}
	recordRun := func(ctx context.Context, step, run string) error {
		tag, err := s.Pool.Exec(ctx, `INSERT INTO weave_capability_step_runs(workspace_id,invocation_id,step_id,run_id)
    SELECT i.workspace_id,i.invocation_id,$3,$4 FROM weave_capability_invocations i JOIN weave_capability_invocation_tasks t ON t.task_id=i.task_id
    WHERE i.workspace_id=$1 AND i.invocation_id=$2 AND i.status='running' AND t.status='running' AND t.claim_token=$5 AND t.deadline_at>now()
    ON CONFLICT DO NOTHING`, task.WorkspaceID, task.InvocationID, step, run, task.ClaimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return capabilities.ErrClaimLost
		}
		return nil
	}
	if requirement.Engine != "" && requirement.Engine != "loom" {
		rec, err := s.selectCapabilityRuntime(ctx, task.WorkspaceID, requirement)
		if err != nil {
			return nil, err
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE weave_capability_invocations SET runtime_id=$3 WHERE workspace_id=$1 AND invocation_id=$2 AND status='running'`, task.WorkspaceID, task.InvocationID, rec.RuntimeID); err != nil {
			return nil, err
		}
		result, _, runErr := capability.ExecutePlanResumable(ctx, task.Plan, task.Input, remoteCapabilitySteps{task: task, executor: s.engineExecutorFor(true), record: rec, recordRun: recordRun}, task.State, capabilities.PGExecutionObserver{Store: capabilities.NewPGStore(s.Pool), Task: task})
		return result, runErr
	}
	if len(task.Plan.Resources.ToolIDs) > 0 {
		return nil, errors.New("capability_tool_runtime_required")
	}
	llm, err := s.Models.ForWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	sink, err := s.rootTerminalSink()
	if err != nil {
		return nil, err
	}
	steps := capabilityruntime.LoomSteps{RunKind: task.RunKind, WorkspaceID: task.WorkspaceID, InvocationID: task.InvocationID, Model: requirement.Model, LLM: llm, Store: s.Store, TerminalSink: sink, RecordRun: recordRun}
	result, _, runErr := capability.ExecutePlanResumable(ctx, task.Plan, task.Input, steps, task.State, capabilities.PGExecutionObserver{Store: capabilities.NewPGStore(s.Pool), Task: task})
	return result, runErr
}

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
