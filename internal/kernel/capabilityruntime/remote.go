package capabilityruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type remoteSteps struct {
	task      Request
	executor  mcphost.RemoteEngineExecutor
	record    *registry.AgentRecord
	recordRun func(context.Context, string, string) error
}

func (e remoteSteps) ExecuteStep(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
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

// ExecuteTool preserves the existing delegated-tool path. It is not a
// direct tool dispatcher and does not assert an independent operation receipt.
func (e remoteSteps) ExecuteTool(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
	worker := step
	worker.Kind = capability.StepWorker
	worker.Instruction = "Use the runtime tool named " + step.ToolID + " with the supplied input. Perform the real operation and return its result."
	return e.ExecuteStep(ctx, worker, input)
}
