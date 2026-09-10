package teamforge

// Agent assembly write tools (plan §10.2.1). Both tools share one
// field-level whitelist: only assembly fields may be written, every write is
// receipt-gated and pre-validated (name/role/avatar, engine/runtime binding,
// F2 model resolution, F13 CLI-vs-graph), and each successful call lands one
// immutable version through AgentRegistry.PutTx.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	// ToolCreateAgent creates one AgentRecord as immutable v1.
	ToolCreateAgent = "tf_create_agent"
	// ToolUpdateAgent patches one AgentRecord over the assembly whitelist,
	// producing the next immutable version.
	ToolUpdateAgent = "tf_update_agent"
)

// agentAssemblyWhitelist is the shared field surface of the two assembly
// tools. build_run_id and name form the call envelope (run binding + target
// asset); everything else is an assembly field. Any other key is rejected
// before dispatch.
var agentAssemblyWhitelist = map[string]bool{
	"build_run_id":      true,
	"name":              true,
	"display_name":      true,
	"role":              true,
	"identity":          true,
	"system_prompt":     true,
	"profiles":          true,
	"skills":            true,
	"mcp_servers":       true,
	"model":             true,
	"memory":            true,
	"memory_slots":      true,
	"guard":             true,
	"compaction":        true,
	"permissions":       true,
	"output_schema":     true,
	"max_cost_usd":      true,
	"max_tokens":        true,
	"max_output_tokens": true,
	"step_budget":       true,
	"tool_loop_control": true,
	"max_tool_repeats":  true,
	"fallback_models":   true,
	"fallback_retries":  true,
	"engine":            true,
	"runtime_id":        true,
	"tags":              true,
}

// writeForbiddenHints explains where out-of-scope keys belong so the LLM can
// self-heal ("提示走正确工具").
var writeForbiddenHints = map[string]string{
	"graph_definition": "graph_definition is owned by the employee internal-graph tools",
	"graph_type":       "graph_type is owned by the employee internal-graph tools",
	"id":               "id is platform-managed and immutable",
	"workspace_id":     "workspace_id is platform-managed",
	"version":          "version is assigned by the registry",
	"team_id":          "team_id is managed by the team tools",
	"owner_user_id":    "owner_user_id is platform-managed",
	"sub_agents":       "sub-agent orchestration is not part of agent assembly",
}

// agentWriteArgs is the typed shape of both assembly tools. output_schema and
// the pointer configs use pointers so "absent" and "null" stay distinct for
// the merge semantics.
type agentWriteArgs struct {
	ToolLoopControl *frozen.ToolLoopControl `json:"tool_loop_control"`
	BuildRunID      string                  `json:"build_run_id"`
	Name            string                  `json:"name"`

	DisplayName  string                         `json:"display_name"`
	Role         string                         `json:"role"`
	Identity     stdlib.IdentitySpec            `json:"identity"`
	SystemPrompt string                         `json:"system_prompt"`
	Profiles     map[string]stdlib.ProfileEntry `json:"profiles"`
	Skills       []stdlib.SkillDef              `json:"skills"`
	MCPServers   []registry.MCPServerConfig     `json:"mcp_servers"`
	Model        string                         `json:"model"`
	Memory       *registry.MemoryConfig         `json:"memory"`
	MemorySlots  []registry.MemorySlot          `json:"memory_slots"`
	Guard        *registry.GuardConfig          `json:"guard"`
	Compaction   *registry.CompactionConfig     `json:"compaction"`
	Permissions  registry.PermissionConfig      `json:"permissions"`
	OutputSchema *json.RawMessage               `json:"output_schema"`

	MaxCostUSD      float64  `json:"max_cost_usd"`
	MaxTokens       int64    `json:"max_tokens"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	StepBudget      int64    `json:"step_budget"`
	MaxToolRepeats  int      `json:"max_tool_repeats"`
	FallbackModels  []string `json:"fallback_models"`
	FallbackRetries int      `json:"fallback_retries"`

	Engine    string   `json:"engine"`
	RuntimeID string   `json:"runtime_id"`
	Tags      []string `json:"tags"`
}

// parseAgentWriteCall decodes the raw args into both a field map (presence /
// whitelist enforcement) and the typed struct (strict JSON types).
func parseAgentWriteCall(call contract.ToolCall) (map[string]json.RawMessage, *agentWriteArgs, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Args), &fields); err != nil {
		return nil, nil, fmt.Errorf("invalid input: %w", err)
	}
	var args agentWriteArgs
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		return nil, nil, fmt.Errorf("invalid input: %w", err)
	}
	return fields, &args, nil
}

// rejectNonWhitelisted rejects every parameter outside the assembly
// whitelist, with a pointer to the correct tool for platform-managed fields.
func rejectNonWhitelisted(fields map[string]json.RawMessage) error {
	for key := range fields {
		if agentAssemblyWhitelist[key] {
			continue
		}
		if hint, ok := writeForbiddenHints[key]; ok {
			return fmt.Errorf(
				"%w: %q cannot be written here (%s); use the correct tool",
				ErrWriteFieldForbidden, key, hint,
			)
		}
		return fmt.Errorf("%w: unknown field %q", ErrWriteFieldForbidden, key)
	}
	return nil
}

// identityFieldPresence detects which identity sub-fields were given and
// rejects any identity key outside core/extended/raw.
func identityFieldPresence(fields map[string]json.RawMessage) (map[string]bool, error) {
	raw, ok := fields["identity"]
	if !ok {
		return nil, nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("invalid input: identity must be an object: %w", err)
	}
	presence := make(map[string]bool, len(nested))
	for key := range nested {
		switch key {
		case "core", "extended", "raw":
			presence[key] = true
		default:
			return nil, fmt.Errorf(
				"%w: unknown identity field %q (allowed: core, extended, raw)",
				ErrWriteFieldForbidden, key,
			)
		}
	}
	return presence, nil
}

// fieldPresence derives the "was this key given" map used by the merge.
func fieldPresence(fields map[string]json.RawMessage) map[string]bool {
	presence := make(map[string]bool, len(fields))
	for key := range fields {
		presence[key] = true
	}
	return presence
}

// buildAgentRecord maps the typed create args onto a fresh AgentRecord.
func buildAgentRecord(args *agentWriteArgs) *registry.AgentRecord {
	return &registry.AgentRecord{
		Name:            args.Name,
		DisplayName:     args.DisplayName,
		Role:            args.Role,
		Model:           args.Model,
		Engine:          args.Engine,
		RuntimeID:       args.RuntimeID,
		Tags:            args.Tags,
		MCPServers:      args.MCPServers,
		Permissions:     args.Permissions,
		MemoryConfig:    args.Memory,
		MemorySlots:     args.MemorySlots,
		Guard:           args.Guard,
		Compaction:      args.Compaction,
		OutputSchema:    args.OutputSchema,
		MaxCostUSD:      args.MaxCostUSD,
		MaxTokens:       args.MaxTokens,
		MaxOutputTokens: args.MaxOutputTokens,
		StepBudget:      args.StepBudget,
		MaxToolRepeats:  args.MaxToolRepeats,
		ToolLoopControl: args.ToolLoopControl,
		FallbackModels:  args.FallbackModels,
		FallbackRetries: args.FallbackRetries,
		Spec: stdlib.AgentSpec{
			SystemPrompt: args.SystemPrompt,
			Identity:     args.Identity,
			Profiles:     args.Profiles,
			Skills:       args.Skills,
		},
	}
}

// mergeAgentAssembly overlays only the given whitelist fields onto the
// existing record (PUT semantics per api/agents.go mergeAgentRecord, field
// surface limited to the whitelist). Unspecified fields stay unchanged. Name
// is the target identity and never a patch field: name changes are rejected
// by construction.
func mergeAgentAssembly(
	existing *registry.AgentRecord,
	args *agentWriteArgs,
	presence map[string]bool,
	identityPresence map[string]bool,
) *registry.AgentRecord {
	merged := *existing
	merged.Name = existing.Name

	if presence["display_name"] && args.DisplayName != "" {
		merged.DisplayName = args.DisplayName
	}
	if presence["role"] {
		merged.Role = args.Role
	}
	if presence["identity"] {
		if identityPresence["core"] {
			merged.Spec.Identity.Core = args.Identity.Core
		}
		if identityPresence["extended"] {
			merged.Spec.Identity.Extended = args.Identity.Extended
		}
		if identityPresence["raw"] {
			merged.Spec.Identity.Raw = args.Identity.Raw
		}
	}
	if presence["system_prompt"] {
		merged.Spec.SystemPrompt = args.SystemPrompt
	}
	if presence["profiles"] {
		merged.Spec.Profiles = args.Profiles
	}
	if presence["skills"] {
		merged.Spec.Skills = args.Skills
	}
	if presence["mcp_servers"] {
		merged.MCPServers = args.MCPServers
	}
	if presence["model"] && args.Model != "" {
		merged.Model = args.Model
	}
	if presence["memory"] && args.Memory != nil {
		merged.MemoryConfig = args.Memory
	}
	if presence["memory_slots"] {
		merged.MemorySlots = args.MemorySlots
	}
	if presence["guard"] && args.Guard != nil {
		merged.Guard = args.Guard
	}
	if presence["compaction"] && args.Compaction != nil {
		merged.Compaction = args.Compaction
	}
	if presence["permissions"] {
		merged.Permissions = args.Permissions
	}
	if presence["output_schema"] && args.OutputSchema != nil {
		merged.OutputSchema = args.OutputSchema
	}
	if presence["max_cost_usd"] {
		merged.MaxCostUSD = args.MaxCostUSD
	}
	if presence["max_tokens"] {
		merged.MaxTokens = args.MaxTokens
	}
	if presence["max_output_tokens"] {
		merged.MaxOutputTokens = args.MaxOutputTokens
	}
	if presence["step_budget"] {
		merged.StepBudget = args.StepBudget
	}
	if presence["tool_loop_control"] {
		merged.ToolLoopControl = args.ToolLoopControl
	}
	if presence["max_tool_repeats"] {
		merged.MaxToolRepeats = args.MaxToolRepeats
	}
	if presence["fallback_models"] {
		merged.FallbackModels = args.FallbackModels
	}
	if presence["fallback_retries"] {
		merged.FallbackRetries = args.FallbackRetries
	}
	if presence["engine"] {
		merged.Engine = args.Engine
	}
	if presence["runtime_id"] {
		merged.RuntimeID = args.RuntimeID
	}
	if presence["tags"] {
		merged.Tags = args.Tags
	}
	return &merged
}

// createAgent implements tf_create_agent: every assembly field is optional
// except role, the target name must sit inside the receipt scope, and the
// write lands immutable v1.
func (d *WriteToolsDispatcher) createAgent(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	fields, args, err := parseAgentWriteCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := rejectNonWhitelisted(fields); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if strings.TrimSpace(args.Name) == "" {
		return toolError(call.ID, "name is required"), nil
	}
	if err := validateAgentNameWrite(args.Name); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gate.authorizeCreate(); err != nil {
		return toolError(call.ID, fmt.Sprintf("tf_create_agent rejected: %v", err)), nil
	}
	if err := d.gate.authorize(ctx, args.Name); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gate.requireBuildRunContext(call.Args); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if args.Role == "" {
		return toolError(call.ID, "role is required: worker or avatar"), nil
	}
	if err := validateAgentRoleWrite(args.Role); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if d.gate.deps.AgentLoad == nil {
		return toolError(call.ID, "agent registry read is unavailable"), nil
	}
	existing, err := d.gate.deps.AgentLoad.Get(ctx, d.gate.workspaceID, args.Name)
	switch {
	case err == nil && existing != nil:
		return toolError(call.ID, fmt.Sprintf("agent %q already exists; use tf_update_agent", args.Name)), nil
	case err != nil && !isAgentNotFound(err):
		return toolError(call.ID, fmt.Sprintf("check agent %q: %v", args.Name, err)), nil
	}

	rec := buildAgentRecord(args)
	applyAgentContextDefaultsWrite(rec)
	if err := d.gate.validateWriteRecord(ctx, rec, nil, false); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	var payload string
	if err := d.gate.writeTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := d.gate.resolveModel(ctx, tx, rec); err != nil {
			return err
		}
		if d.gate.deps.Agents == nil {
			return errors.New("agent registry write is unavailable")
		}
		if err := d.gate.deps.Agents.PutTx(ctx, tx, d.gate.workspaceID, rec); err != nil {
			return err
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		payload = string(data)
		return nil
	}); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	return &contract.ToolResult{CallID: call.ID, Content: payload}, nil
}

// updateAgent implements tf_update_agent: a whitelist-field patch over the
// current record that lands the next immutable version. Only the given
// fields change; name is the target identity and can never be changed here.
func (d *WriteToolsDispatcher) updateAgent(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	fields, args, err := parseAgentWriteCall(call)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := rejectNonWhitelisted(fields); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	identityPresence, err := identityFieldPresence(fields)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	presence := fieldPresence(fields)
	if strings.TrimSpace(args.Name) == "" {
		return toolError(call.ID, "name is required"), nil
	}
	if err := validateAgentNameWrite(args.Name); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if err := d.gate.requireBuildRunContext(call.Args); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	if fields["role"] != nil {
		if err := validateAgentRoleWrite(args.Role); err != nil {
			return toolError(call.ID, err.Error()), nil
		}
	}
	// The caller may target the agent by stable name or stable ID; the
	// resolved record carries both identities so the receipt scope gate
	// matches create-mode NamePrefix and optimize-mode ID refs alike.
	existing, err := d.gate.resolveAgentTarget(ctx, args.Name)
	if err != nil {
		if isAgentNotFound(err) {
			return toolError(call.ID, fmt.Sprintf("agent %q not found; use tf_create_agent", args.Name)), nil
		}
		return toolError(call.ID, fmt.Sprintf("load agent %q: %v", args.Name, err)), nil
	}
	if err := d.gate.authorizeRef(ctx, teambuild.AssetRef{
		Kind: "agent", ID: existing.ID, Name: existing.Name,
	}); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	merged := mergeAgentAssembly(existing, args, presence, identityPresence)
	applyAgentContextDefaultsWrite(merged)
	if err := d.gate.validateWriteRecord(ctx, merged, existing, fields["engine"] != nil); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	var payload string
	if err := d.gate.writeTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := d.gate.resolveModel(ctx, tx, merged); err != nil {
			return err
		}
		if d.gate.deps.Agents == nil {
			return errors.New("agent registry write is unavailable")
		}
		if err := d.gate.deps.Agents.PutTx(ctx, tx, d.gate.workspaceID, merged); err != nil {
			return err
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		payload = string(data)
		return nil
	}); err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	return &contract.ToolResult{CallID: call.ID, Content: payload}, nil
}
