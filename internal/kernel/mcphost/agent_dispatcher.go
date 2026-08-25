package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// AgentToolDispatcher exposes registered agents as a single "delegate" meta-tool.
//
// Instead of N separate tools (one per agent), we expose ONE tool whose description
// contains a concise catalog of available agents. This forces the LLM to make a
// deliberate delegation decision rather than reflexively calling the first matching
// agent tool it sees.
//
// Inner agent calls are depth-limited to 1 (no recursive agent-as-tool).
type AgentToolDispatcher struct {
	registry         agentRegistry
	tenant           string
	selfName         string // the calling agent's name — excluded from catalog
	llm              contract.LLM
	store            loom.Store
	memoryService    *memory.Service
	recorder         DispatchRecorder
	attachments      []execenv.Attachment
	compileAgent     compiler.GraphFactory
	runner           *AgentRunner
	dispatched       atomic.Int64
	LocalExec        RemoteEngineExecutor
	RemoteExec       RemoteEngineExecutor
	Broker           *ToolBroker
	RunLifecycleHook loomruntime.RunLifecycleHook
	// InnerPlatformTools, when set, contributes extra platform dispatchers to
	// depth-1 inner agent runs (e.g. read-only task status tools). The inner
	// agent record is supplied so tools can scope per-agent queries. Write-gated
	// tools must never be exposed through this hook.
	InnerPlatformTools func(rec *registry.AgentRecord) []contract.ToolDispatcher
}

// DispatchedCount returns the number of authorized delegate calls dispatched.
func (d *AgentToolDispatcher) DispatchedCount() int64 {
	return d.dispatched.Load()
}

type agentRegistry interface {
	ListManaged(ctx context.Context, tenant, fromName string) ([]registry.ManagedAgent, error)
	Get(ctx context.Context, tenant, name string) (*registry.AgentRecord, error)
	GetVersion(ctx context.Context, tenant, agentID string, version int) (*registry.AgentRecord, error)
}

// RemoteEngineExecutor executes an already-resolved CLI agent record on its
// bound runtime.
type RemoteEngineExecutor interface {
	ExecRemote(
		ctx context.Context,
		tenant string,
		rec *registry.AgentRecord,
		stamp execution.AgentExecutionStamp,
		prompt string,
		attachments []execenv.Attachment,
	) (output string, err error)
}

// StructuredRemoteEngineExecutor is an optional extension for callers that
// require the CLI's final message to satisfy a JSON Schema. Ordinary worker
// dispatch keeps using RemoteEngineExecutor; protocol adapters opt in.
type StructuredRemoteEngineExecutor interface {
	ExecRemoteStructured(
		ctx context.Context,
		tenant string,
		rec *registry.AgentRecord,
		stamp execution.AgentExecutionStamp,
		prompt string,
		attachments []execenv.Attachment,
		outputSchema json.RawMessage,
	) (output string, err error)
}

// DispatchRecorder records completed single-agent dispatches for tracing.
type DispatchRecorder interface {
	RecordDispatch(ctx context.Context, workspaceID, fromAgent, toAgent, message, result string, ok bool) error
}

// NewAgentToolDispatcher creates a dispatcher that wraps managed agents as a
// single delegate tool.
func NewAgentToolDispatcher(
	reg *registry.AgentRegistry,
	tenant, selfName string,
	llm contract.LLM,
	store loom.Store,
	memSvc *memory.Service,
	recorder DispatchRecorder,
	workspacesRoot, oneapiBase, boundaryBase, oneapiKey string,
	attachments []execenv.Attachment,
) *AgentToolDispatcher {
	runner := NewAgentRunner(reg, tenant, llm, store, memSvc, workspacesRoot, oneapiBase, boundaryBase, oneapiKey, attachments)
	return &AgentToolDispatcher{
		registry:      reg,
		tenant:        tenant,
		selfName:      selfName,
		llm:           llm,
		store:         store,
		memoryService: memSvc,
		recorder:      recorder,
		attachments:   attachments,
		compileAgent:  compiler.CompileAgent,
		runner:        runner,
		LocalExec:     runner.LocalExec,
	}
}

// delegateInputSchema is the JSON Schema for the delegate meta-tool.
var delegateInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"agent": {
			"type": "string",
			"description": "Name of the agent to delegate to (from the catalog in the tool description)"
		},
		"message": {
			"type": "string",
			"description": "The task or question to send to the agent"
		}
	},
	"required": ["agent", "message"]
}`)

// agentSummary extracts a short one-line summary from an agent's identity/prompt.
func agentSummary(rec registry.AgentRecord) string {
	source := rec.Spec.Identity.Core
	if source == "" {
		source = rec.Spec.SystemPrompt
	}
	if source == "" {
		return "specialized agent"
	}

	// Take first meaningful line (skip headers starting with #).
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Truncate if too long.
		if len(line) > 80 {
			line = line[:80] + "..."
		}
		return line
	}

	// Fallback: first 80 chars of the whole thing.
	flat := strings.ReplaceAll(source, "\n", " ")
	if len(flat) > 80 {
		flat = flat[:80] + "..."
	}
	return flat
}

// ListTools returns a single "delegate" tool with an embedded catalog of
// available agents. Returns nil if no managed agents are registered.
func (d *AgentToolDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	agents, err := d.registry.ListManaged(ctx, d.tenant, d.selfName)
	if err != nil {
		return nil, nil
	}

	// Build agent catalog.
	var catalog strings.Builder
	count := 0
	for _, rec := range agents {
		if rec.Name == d.selfName || rec.Kind != "consult" {
			continue
		}
		fmt.Fprintf(&catalog, "- %s: %s\n", rec.Name, agentSummary(rec.AgentRecord))
		if rec.Instruction != "" {
			fmt.Fprintf(&catalog, "  派工指引: %s\n", rec.Instruction)
		}
		count++
	}
	if count == 0 {
		return nil, nil // no other agents to delegate to
	}

	desc := fmt.Sprintf(
		"Delegate a task to a specialized agent. "+
			"Only use when the task clearly falls outside your own expertise "+
			"and within another agent's specialty. "+
			"Do NOT delegate if you can answer directly.\n\n"+
			"Available agents (%d):\n%s", count, catalog.String())

	return []contract.ToolDef{{
		Name:        "delegate",
		Description: desc,
		InputSchema: delegateInputSchema,
	}}, nil
}

// Dispatch handles a delegate tool call. It compiles and runs the target
// agent's graph synchronously, returning the output as a tool result.
// The inner agent does NOT get agent-as-tool capabilities (depth = 1).
func (d *AgentToolDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != "delegate" {
		return &contract.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("unknown tool %q", call.Name),
			IsError: true,
		}, nil
	}

	// Parse input.
	var input struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return &contract.ToolResult{
			CallID:  call.ID,
			Content: "invalid input: " + err.Error(),
			IsError: true,
		}, nil
	}
	if input.Agent == "" || input.Message == "" {
		return &contract.ToolResult{
			CallID:  call.ID,
			Content: "both 'agent' and 'message' fields are required",
			IsError: true,
		}, nil
	}

	managed, err := d.registry.ListManaged(ctx, d.tenant, d.selfName)
	if err != nil || !containsAgentKind(managed, input.Agent, "consult") {
		return &contract.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("not authorized to dispatch to %q", input.Agent),
			IsError: true,
		}, nil
	}
	d.dispatched.Add(1)

	finish := func(content string, isError bool) *contract.ToolResult {
		if d.recorder != nil {
			_ = d.recorder.RecordDispatch(
				ctx, d.tenant, d.selfName, input.Agent, input.Message, content, !isError,
			)
		}
		return &contract.ToolResult{CallID: call.ID, Content: content, IsError: isError}
	}

	// Prevent self-delegation.
	if input.Agent == d.selfName {
		return finish("cannot delegate to self", true), nil
	}

	if d.runner == nil {
		d.runner = &AgentRunner{
			registry: d.registry, tenant: d.tenant, llm: d.llm, store: d.store,
			memoryService: d.memoryService, attachments: d.attachments,
		}
	}
	d.runner.LocalExec = d.LocalExec
	d.runner.RemoteExec = d.RemoteExec
	d.runner.Broker = d.Broker
	d.runner.InnerPlatformTools = d.InnerPlatformTools
	d.runner.RunLifecycleHook = d.RunLifecycleHook
	d.runner.compileAgent = d.compileAgent
	result, err := d.runner.Run(ctx, input.Agent, input.Message)
	if err != nil {
		return finish(err.Error(), true), nil
	}
	return finish(result.Output, false), nil
}

func containsAgentKind(agents []registry.ManagedAgent, name, kind string) bool {
	for _, rec := range agents {
		if rec.Name == name && rec.Kind == kind {
			return true
		}
	}
	return false
}

// TryDispatch handles the case where the LLM calls an agent name directly
// (e.g. "requirement-understanding") instead of using the "delegate" meta-tool.
// If the name matches a managed agent, the call is rewritten as a delegate call.
// Returns (nil, nil) if the name is not in the caller's managed set.
func (d *AgentToolDispatcher) TryDispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	// Don't match self.
	if call.Name == d.selfName {
		return nil, nil
	}

	// Match only managed agents so fallback dispatch does not reveal whether an
	// unauthorized name exists elsewhere in the workspace.
	managed, err := d.registry.ListManaged(ctx, d.tenant, d.selfName)
	if err != nil || !containsAgentKind(managed, call.Name, "consult") {
		return nil, nil // not managed — let someone else handle it
	}

	// Extract a message from the original call args.
	message := call.Name // fallback: just the agent name
	if call.Args != "" {
		var raw map[string]any
		if json.Unmarshal([]byte(call.Args), &raw) == nil {
			// Try common field names the LLM might use.
			for _, key := range []string{"message", "query", "input", "task", "request", "content"} {
				if v, ok := raw[key].(string); ok && v != "" {
					message = v
					break
				}
			}
		}
	}

	// Rewrite as a delegate call.
	delegateArgs, _ := json.Marshal(map[string]string{
		"agent":   call.Name,
		"message": message,
	})
	rewritten := contract.ToolCall{
		ID:   call.ID,
		Name: "delegate",
		Args: string(delegateArgs),
	}
	return d.Dispatch(ctx, rewritten)
}

// Compile-time interface checks.
var _ contract.ToolDispatcher = (*AgentToolDispatcher)(nil)
var _ FallbackDispatcher = (*AgentToolDispatcher)(nil)
