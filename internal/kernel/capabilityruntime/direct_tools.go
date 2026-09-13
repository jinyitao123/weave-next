package capabilityruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// DirectToolConfig is assembled from the invocation's admitted, frozen tool
// grants. Tool names alone never authorize discovery of another connection.
// Credential material is resolved only for the selected binding at execution.
type DirectToolConfig struct {
	WorkspaceID    string
	Agent          *registry.AgentRecord
	ToolIDs        []string
	Bindings       []frozen.FrozenMCPBinding
	AccessFactory  *mcphost.MCPAccessFactory
	ResolveAccess  func(context.Context, frozen.CredentialReference) (map[string]string, error)
	ValidateAccess func(context.Context, frozen.CredentialReference) error
	ValidateClaim  func(context.Context) error
}

// DirectToolExecutor sends the specified tool and unchanged arguments through
// the existing frozen MCP gateway. It owns no retries, queue, or terminal state;
// the caller owns activation/checkpoint recovery, including unknown outcomes.
type DirectToolExecutor struct {
	workspace      string
	agent          *registry.AgentRecord
	bindings       map[string]frozen.FrozenMCPBinding
	access         *mcphost.MCPAccessFactory
	resolveAccess  func(context.Context, frozen.CredentialReference) (map[string]string, error)
	validateAccess func(context.Context, frozen.CredentialReference) error
	validateClaim  func(context.Context) error
}

func NewDirectToolExecutor(config DirectToolConfig) (*DirectToolExecutor, error) {
	if config.WorkspaceID == "" || config.Agent == nil || config.Agent.WorkspaceID != config.WorkspaceID ||
		config.Agent.ID == "" || config.Agent.Version < 1 || config.Agent.Name == "" ||
		config.AccessFactory == nil || config.ResolveAccess == nil || config.ValidateAccess == nil || config.ValidateClaim == nil {
		return nil, errors.New("capability governed tool dependencies are incomplete")
	}
	allowed := make(map[string]struct{}, len(config.ToolIDs))
	for _, name := range config.ToolIDs {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, errors.New("capability tool identity is invalid")
		}
		allowed[name] = struct{}{}
	}
	bound := make(map[string]frozen.FrozenMCPBinding, len(allowed))
	for _, binding := range config.Bindings {
		raw, err := json.Marshal(binding)
		if err != nil {
			return nil, errors.New("capability frozen tool binding is invalid")
		}
		binding, err = frozen.DecodeFrozenMCPBinding(raw)
		if err != nil || binding.WorkspaceID != config.WorkspaceID || binding.ServerID == "" ||
			binding.ServerRevision < 1 || binding.Transport != "http" {
			return nil, errors.New("capability frozen tool binding is invalid")
		}
		for _, tool := range binding.Tools {
			if _, ok := allowed[tool.Name]; !ok {
				continue
			}
			if len(binding.Filter) > 0 && !slices.Contains(binding.Filter, tool.Name) {
				return nil, errors.New("capability tool is outside its frozen filter")
			}
			if _, exists := bound[tool.Name]; exists {
				return nil, errors.New("capability tool identity is ambiguous")
			}
			bound[tool.Name] = binding
		}
	}
	if len(bound) != len(allowed) || len(bound) == 0 {
		return nil, errors.New("capability tool is missing an admitted frozen binding")
	}
	// Only audit identity enters the gateway. Do not retain a mutable caller
	// record with extra connections, permissions, or credential configuration.
	agent := &registry.AgentRecord{WorkspaceID: config.WorkspaceID, ID: config.Agent.ID,
		Version: config.Agent.Version, Name: config.Agent.Name}
	return &DirectToolExecutor{workspace: config.WorkspaceID, agent: agent, bindings: bound,
		access: config.AccessFactory, resolveAccess: config.ResolveAccess,
		validateAccess: config.ValidateAccess, validateClaim: config.ValidateClaim}, nil
}

func (e *DirectToolExecutor) ExecuteTool(ctx context.Context, step capability.PlanStep, input json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e == nil || step.Kind != capability.StepTool {
		return nil, errors.New("capability direct tool step is invalid")
	}
	binding, ok := e.bindings[step.ToolID]
	if !ok {
		return nil, errors.New("capability tool is not authorized")
	}
	callID := execution.InvocationID(ctx)
	if callID == "" {
		return nil, errors.New("capability tool invocation identity is missing")
	}
	guard := func(checkCtx context.Context) error {
		if err := e.validateClaim(checkCtx); err != nil {
			return err
		}
		return e.validateAccess(checkCtx, binding.AccessRef)
	}
	if err := guard(ctx); err != nil {
		return nil, fmt.Errorf("%w: capability tool authority unavailable", mcphost.ErrFailClosed)
	}
	headers, err := e.resolveAccess(ctx, binding.AccessRef)
	if err != nil {
		return nil, fmt.Errorf("%w: capability tool credentials unavailable", mcphost.ErrFailClosed)
	}
	dispatcher, err := e.access.BuildFrozenServer(e.workspace, e.agent, binding, headers, guard)
	if err != nil {
		return nil, err
	}
	result, err := dispatcher.Dispatch(ctx, contract.ToolCall{ID: callID, Name: step.ToolID, Args: string(input)})
	if err != nil {
		return nil, err
	}
	if result == nil || result.CallID != callID {
		return nil, errors.New("capability tool result identity is invalid")
	}
	if result.IsError {
		// Upstream failures can contain credentials or transport diagnostics;
		// the governed audit retains its normal bounded details.
		return nil, errors.New("capability tool execution did not succeed")
	}
	raw := json.RawMessage(result.Content)
	if !json.Valid(raw) {
		return nil, errors.New("capability tool returned non-JSON output")
	}
	if len(step.OutputSchema) > 0 {
		if err := capability.ValidateValue(step.OutputSchema, raw); err != nil {
			return nil, errors.New("capability tool output does not match the declared schema")
		}
	}
	return raw, nil
}

var _ capability.ToolStepExecutor = (*DirectToolExecutor)(nil)
