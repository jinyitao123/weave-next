// Package capabilityruntime binds capability steps to the existing Loom runner.
package capabilityruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

type LoomSteps struct {
	RunKind      string
	WorkspaceID  string
	InvocationID string
	Model        string
	LLM          contract.LLM
	Store        loom.Store
	TerminalSink loomruntime.TerminalSink
	RecordRun    func(context.Context, string, string) error
}

type noTools struct{}

func (noTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (noTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, fmt.Errorf("tools are not authorized for this capability")
}

func (e LoomSteps) ExecuteStep(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
	if e.LLM == nil || e.Store == nil || e.TerminalSink == nil || e.RecordRun == nil || e.Model == "" {
		return nil, fmt.Errorf("Loom step dependencies are incomplete")
	}
	if step.Kind != capability.StepWorker {
		return nil, fmt.Errorf("unsupported Loom step kind")
	}
	identity := fmt.Sprintf("cap-%s-%x", e.RunKind, sha256.Sum256([]byte(e.InvocationID+"\x00"+step.ID)))
	schema := step.OutputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	rec := &registry.AgentRecord{
		WorkspaceID: e.WorkspaceID, Name: identity, Role: "worker", Engine: "loom", Model: e.Model,
		Spec:         stdlib.AgentSpec{SystemPrompt: step.RoleName + "\n" + step.RoleDescription + "\n" + step.Instruction + "\nReturn one JSON value matching the supplied schema."},
		OutputSchema: &schema, MaxOutputTokens: 4096, StepBudget: 2,
		Permissions:  registry.PermissionConfig{Deny: []string{"*"}},
		MemoryConfig: &registry.MemoryConfig{Enabled: false}, Compaction: &registry.CompactionConfig{Enabled: false},
	}
	attribution, err := loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{WorkspaceID: e.WorkspaceID, Scope: loomruntime.TerminalAttributionLegacyUnattributed}, nil)
	if err != nil {
		return nil, err
	}
	result, err := loomruntime.Run(ctx, e.WorkspaceID, rec, loomruntime.Input{
		SessionID: e.InvocationID, LastUserMessage: string(input), Messages: []contract.Message{{Role: "user", Content: string(input)}},
	}, attribution, loomruntime.Dependencies{
		LLM: e.LLM, Store: e.Store, Tools: noTools{}, TerminalSink: e.TerminalSink,
		UsageAttributionHook: func(ctx context.Context, lease loomruntime.RunAttemptLease) error {
			return e.RecordRun(ctx, step.ID, lease.RunID)
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Yielded || string(result.StopReason) != "completed" {
		return nil, fmt.Errorf("Loom stopped without completion: %s", result.StopReason)
	}
	raw := json.RawMessage(result.Output)
	if err := capability.ValidateValue(schema, raw); err != nil {
		return nil, fmt.Errorf("Loom output schema: %w", err)
	}
	return raw, nil
}
