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
	recordedAttempt := false
	for _, attempt := range result.Attempts {
		if attempt.AttemptID == "" {
			continue
		}
		recordedAttempt = true
		if recordErr := e.recordRun(ctx, step.ID, attempt.AttemptID); recordErr != nil {
			return nil, errors.Join(err, recordErr)
		}
	}
	if !recordedAttempt && result.SessionID != "" {
		if recordErr := e.recordRun(ctx, step.ID, result.SessionID); recordErr != nil {
			return nil, errors.Join(err, recordErr)
		}
	}
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(result.Output)
	if !json.Valid(raw) {
		return nil, errors.New("remote runtime returned invalid JSON")
	}
	return raw, nil
}
