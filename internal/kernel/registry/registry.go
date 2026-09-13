package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const (
	CodeDependencyVersionRequired = "workflow_dependency_version_required"
	CodeDependencyUnenumerable    = "workflow_dependency_unenumerable"
	CodeFrozenManifestMismatch    = "workflow_frozen_manifest_mismatch"
	// CodeSkillVersionRequired is the stable fail-closed code for skill
	// bindings that must pin an exact immutable SkillVersion. The same code
	// string is shared across internal/skills, internal/freezer, and the
	// workflow machine so the D3b validator can reuse the sentinel below.
	CodeSkillVersionRequired = "workflow_skill_version_required"
)

var (
	ErrDependencyVersionRequired = &ResolverError{code: CodeDependencyVersionRequired}
	ErrDependencyUnenumerable    = &ResolverError{code: CodeDependencyUnenumerable}
	ErrFrozenManifestMismatch    = &ResolverError{code: CodeFrozenManifestMismatch}
	// ErrSkillVersionRequired rejects registry_version skill bindings that
	// omit or invalidate the exact version pin (missing skill_id, missing
	// version, or version < 1).
	ErrSkillVersionRequired = &ResolverError{code: CodeSkillVersionRequired}
)

// ResolverError carries a stable workflow code while retaining an optional
// underlying cause for infrastructure diagnostics.
type ResolverError struct {
	code  string
	cause error
}

func (e *ResolverError) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *ResolverError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *ResolverError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ResolverError) Is(target error) bool {
	if e == nil || resolverNilInterface(target) {
		return false
	}
	coded, ok := target.(interface{ Code() string })
	if !ok || resolverNilInterface(coded) {
		return false
	}
	code := coded.Code()
	return code != "" && e.code == code
}

// NewResolverError attaches an optional cause to a stable frozen-definition error.
func NewResolverError(code string, cause error) error {
	if resolverNilInterface(cause) {
		cause = nil
	}
	return &ResolverError{code: code, cause: cause}
}

func resolverNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// MCPServerConfig declares an MCP server an agent can access.
type MCPServerConfig struct {
	ServerID   string            `json:"server_id,omitempty"`
	URL        string            `json:"url,omitempty"`
	Filter     []string          `json:"filter,omitempty"`      // only expose these tool names
	WriteTools []string          `json:"write_tools,omitempty"` // tools rejected by the write gate
	Headers    map[string]string `json:"headers,omitempty"`
}

// PermissionConfig holds deny/allow/ask rules for tool access.
type PermissionConfig struct {
	Deny  []string `json:"deny,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"` // tools requiring user confirmation before execution
}

// MemoryConfig controls per-agent memory behavior.
type MemoryConfig struct {
	Enabled      bool   `json:"enabled"`
	TopK         int    `json:"top_k,omitempty"`
	AutoRemember bool   `json:"auto_remember"`
	Scope        string `json:"scope,omitempty"` // "tenant" (default) | "user" | "session"
}

// MemorySlot defines one bounded profile field an agent may remember about the
// current conversation participant.
type MemorySlot struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// GuardConfig configures input guardrails.
type GuardConfig struct {
	Enabled      bool     `json:"enabled"`
	MaxInputLen  int      `json:"max_input_len,omitempty"` // reject inputs longer than N chars
	BlockedTerms []string `json:"blocked_terms,omitempty"` // reject inputs containing these terms
}

// CompactionConfig controls automatic context compaction in ToolLoop.
type CompactionConfig struct {
	Enabled        bool `json:"enabled"`
	TokenThreshold int  `json:"token_threshold,omitempty"` // trigger compaction above this token count (default 6000)
}

// DefaultMemoryConfig is the platform default for agent context management:
// memory retrieval and auto-remember are enabled whenever an embedder is
// available. Top-K defaults to 5, scope to tenant.
func DefaultMemoryConfig() *MemoryConfig {
	return &MemoryConfig{Enabled: true, TopK: 5, AutoRemember: true, Scope: "tenant"}
}

// EffectiveMemoryConfig returns the agent's memory configuration, defaulting to
// the platform default when the record omits one. Explicit Enabled=false is
// respected.
func EffectiveMemoryConfig(rec *AgentRecord) *MemoryConfig {
	if rec != nil && rec.MemoryConfig != nil {
		return rec.MemoryConfig
	}
	return DefaultMemoryConfig()
}

// DefaultCompactionConfig is the platform default for context compaction:
// enabled, with the compiler's default threshold (6000 estimated tokens).
func DefaultCompactionConfig() *CompactionConfig {
	return &CompactionConfig{Enabled: true}
}

// EffectiveCompactionConfig returns the agent's compaction configuration,
// defaulting to enabled when the record omits one. Explicit Enabled=false is
// respected.
func EffectiveCompactionConfig(rec *AgentRecord) *CompactionConfig {
	if rec != nil && rec.Compaction != nil {
		return rec.Compaction
	}
	return DefaultCompactionConfig()
}

// SubAgentRef references another agent for orchestration.
type SubAgentRef struct {
	Name        string `json:"name"`                  // agent name to delegate to
	Description string `json:"description,omitempty"` // when to route to this agent
	RouteKey    string `json:"route_key,omitempty"`   // state key value that routes here
}

// SkillRefSourceType values for SkillRef.SourceType.
const (
	SourceTypeRegistryVersion = "registry_version" // exact immutable SkillVersion pin
	SourceTypeLegacy          = "legacy"           // name-based legacy store binding
	SourceTypeBuiltin         = "builtin"          // platform builtin skill binding
)

// SkillRef is an explicit agent skill binding. registry_version refs pin one
// exact immutable SkillVersion; legacy/builtin refs keep the name-based D2
// live path. SkillID and Name are independent: SkillID is the immutable
// registry identity (registry_version only), Name is the display/matching
// name used by the prompt layer.
type SkillRef struct {
	SourceType   string `json:"source_type"`
	SkillID      string `json:"skill_id,omitempty"`
	SkillVersion *int64 `json:"skill_version,omitempty"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
}

// GraphDefinition is a declarative graph that can be compiled without Go code.
type GraphDefinition struct {
	Entry string           `json:"entry"`
	Steps []StepDefinition `json:"steps"`
}

// StepDefinition defines a single step in a declarative graph.
type StepDefinition struct {
	Name      string         `json:"name"`
	Type      string         `json:"type"` // chat | llm_call | llm_check | yield | transform | builtin
	Display   string         `json:"display"`
	Config    map[string]any `json:"config"`
	Next      *string        `json:"next,omitempty"`      // null = end
	Condition *ConditionDef  `json:"condition,omitempty"` // mutually exclusive with Next
}

// ConditionDef defines conditional routing based on a state key.
type ConditionDef struct {
	Key       string  `json:"key"`
	TrueStep  *string `json:"true"`
	FalseStep *string `json:"false"`
}

// Validate checks the GraphDefinition for structural correctness.
func (gd *GraphDefinition) Validate() error {
	if gd.Entry == "" {
		return fmt.Errorf("entry is required")
	}

	names := make(map[string]bool)
	for _, s := range gd.Steps {
		if s.Name == "" {
			return fmt.Errorf("step name is required")
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate step name: %s", s.Name)
		}
		names[s.Name] = true
	}

	if !names[gd.Entry] {
		return fmt.Errorf("entry step %q not found in steps", gd.Entry)
	}

	validTypes := map[string]bool{"chat": true, "llm_call": true, "llm_check": true, "yield": true, "transform": true, "builtin": true, "worker": true}
	for _, s := range gd.Steps {
		if !validTypes[s.Type] {
			return fmt.Errorf("step %q: invalid type %q", s.Name, s.Type)
		}
		if s.Type == "worker" {
			worker, ok := s.Config["worker"].(string)
			if !ok || strings.TrimSpace(worker) == "" {
				return fmt.Errorf("step %q: config.worker must be a non-empty string", s.Name)
			}
			for _, key := range []string{"prompt_template", "output_key"} {
				if value, exists := s.Config[key]; exists {
					if _, ok := value.(string); !ok {
						return fmt.Errorf("step %q: config.%s must be a string", s.Name, key)
					}
				}
			}
			if value, exists := s.Config["input_keys"]; exists {
				keys, ok := value.([]any)
				if !ok {
					return fmt.Errorf("step %q: config.input_keys must be an array of strings", s.Name)
				}
				for _, key := range keys {
					if _, ok := key.(string); !ok {
						return fmt.Errorf("step %q: config.input_keys must be an array of strings", s.Name)
					}
				}
			}
		}
		if s.Next != nil && s.Condition != nil {
			return fmt.Errorf("step %q: next and condition are mutually exclusive", s.Name)
		}
		if s.Next != nil && *s.Next != "" && !names[*s.Next] {
			return fmt.Errorf("step %q: next target %q not found", s.Name, *s.Next)
		}
		if s.Condition != nil {
			if s.Condition.TrueStep != nil && *s.Condition.TrueStep != "" && !names[*s.Condition.TrueStep] {
				return fmt.Errorf("step %q: condition true target %q not found", s.Name, *s.Condition.TrueStep)
			}
			if s.Condition.FalseStep != nil && *s.Condition.FalseStep != "" && !names[*s.Condition.FalseStep] {
				return fmt.Errorf("step %q: condition false target %q not found", s.Name, *s.Condition.FalseStep)
			}
		}
	}

	return nil
}

// AgentRecord wraps an AgentSpec with platform metadata.
type AgentRecord struct {
	ToolLoopControl *frozen.ToolLoopControl `json:"tool_loop_control,omitempty"`
	Name            string                  `json:"name"`
	ID              string                  `json:"id,omitempty"`
	WorkspaceID     string                  `json:"workspace_id,omitempty"`
	TeamID          string                  `json:"team_id,omitempty"`
	OwnerUserID     *string                 `json:"owner_user_id,omitempty"`
	DisplayName     string                  `json:"display_name,omitempty"`
	Role            string                  `json:"role,omitempty"`       // worker|avatar; empty defaults to worker
	Visibility      string                  `json:"visibility,omitempty"` // public|internal_tool|platform; empty defaults to public
	Engine          string                  `json:"engine,omitempty"`     // ""/"loom" in-process; "opencode"|"codex"|"claude" external CLI runtime
	RuntimeID       string                  `json:"runtime_id,omitempty"`
	// RuntimePolicyMode and RuntimePoolID are request-frozen execution facts.
	// They are carried only by the resolved in-memory copy and never persisted
	// into the AgentRecord or uploaded to a runtime daemon.
	RuntimePolicyMode string            `json:"-"`
	RuntimePoolID     string            `json:"-"`
	Version           int               `json:"version"`
	Model             string            `json:"model"`
	Spec              stdlib.AgentSpec  `json:"spec"`
	Permissions       PermissionConfig  `json:"permissions,omitempty"`
	MCPServers        []MCPServerConfig `json:"mcp_servers,omitempty"`
	MemoryConfig      *MemoryConfig     `json:"memory_config,omitempty"`
	MemorySlots       []MemorySlot      `json:"memory_slots,omitempty"`
	OutputSchema      *json.RawMessage  `json:"output_schema,omitempty"`
	MaxCostUSD        float64           `json:"max_cost_usd,omitempty"`
	MaxTokens         int64             `json:"max_tokens,omitempty"`
	MaxOutputTokens   int               `json:"max_output_tokens,omitempty"` // per-request output token limit
	StepBudget        int64             `json:"step_budget,omitempty"`
	MaxToolRepeats    int               `json:"max_tool_repeats,omitempty"` // consecutive identical tool-call batches before breaking loop (0 = disabled, default 5 when >0)
	FallbackModels    []string          `json:"fallback_models,omitempty"`  // ordered list of models to try if primary fails
	FallbackRetries   int               `json:"fallback_retries,omitempty"` // retries per model on transient errors (default 2)
	Guard             *GuardConfig      `json:"guard,omitempty"`
	Compaction        *CompactionConfig `json:"compaction,omitempty"`
	SubAgents         []SubAgentRef     `json:"sub_agents,omitempty"`
	SkillRefs         []SkillRef        `json:"skill_refs,omitempty"`       // explicit skill bindings (registry_version/legacy/builtin)
	GraphType         string            `json:"graph_type,omitempty"`       // empty/"standard" = standard compilation, other = lookup registered factory
	GraphDefinition   *GraphDefinition  `json:"graph_definition,omitempty"` // declarative graph (used when graph_type = "declarative")
	Tags              []string          `json:"tags,omitempty"`             // grouping labels, e.g. ["customer-service", "production"]
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Deleted           bool              `json:"deleted,omitempty"`
}

// ValidateSkillRefs enforces the SkillRef binding contract on create/update.
// registry_version refs must pin an exact positive SkillVersion (fail-closed
// with ErrSkillVersionRequired so the D3b validator can reuse the code);
// every ref needs a non-empty matching name; names are unique per record.
func ValidateSkillRefs(rec *AgentRecord) error {
	seenNames := make(map[string]struct{}, len(rec.SkillRefs))
	for i := range rec.SkillRefs {
		ref := &rec.SkillRefs[i]
		switch ref.SourceType {
		case SourceTypeRegistryVersion:
			if strings.TrimSpace(ref.SkillID) == "" {
				return fmt.Errorf(
					"%w: registry_version skill ref %d is missing skill_id",
					ErrSkillVersionRequired, i,
				)
			}
			if ref.SkillVersion == nil || *ref.SkillVersion < 1 {
				return fmt.Errorf(
					"%w: registry_version skill %q must pin an exact positive skill_version",
					ErrSkillVersionRequired, ref.SkillID,
				)
			}
		case SourceTypeLegacy, SourceTypeBuiltin:

		default:
			return fmt.Errorf("agent skill ref %d: unknown source_type %q", i, ref.SourceType)
		}
		if strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("agent skill ref %d: name is required", i)
		}
		if _, dup := seenNames[ref.Name]; dup {
			return fmt.Errorf("agent skill refs: duplicate name %q", ref.Name)
		}
		seenNames[ref.Name] = struct{}{}
	}
	return nil
}

// Agent visibility values. Visibility marks platform-owned assets so the
// console can filter them out of team trees and avatar pickers while exact
// name invocation remains available.
const (
	VisibilityPublic       = "public"
	VisibilityInternalTool = "internal_tool"
	VisibilityPlatform     = "platform"
)

// ValidAgentVisibility reports whether v is one of the supported visibility
// values.
func ValidAgentVisibility(v string) bool {
	return v == VisibilityPublic || v == VisibilityInternalTool || v == VisibilityPlatform
}

// AgentVersionContent is one exact immutable agent version with the canonical
// content hash of its frozen spec. The stable agent_id is the identity; name
// is carried for validation and display only.
type AgentVersionContent struct {
	AgentID     string
	Name        string
	Version     int
	ContentHash string
	Engine      string
	RuntimeID   string
	Model       string
}

// ErrAgentRoleReferencedByTeam prevents a Team lead or worker from changing
// the role fact required by its existing relationship.
var ErrAgentRoleReferencedByTeam = errors.New("agent role is referenced by a team")

// ErrOwnerNotWorkspaceMember indicates that an agent owner does not belong to
// the agent's workspace.
var ErrOwnerNotWorkspaceMember = errors.New("agent owner is not a workspace member")

// ErrAgentReferencedByTeamWorker indicates that an agent remains part of a
// team roster and must be unlinked before it can be soft-deleted.
var ErrAgentReferencedByTeamWorker = errors.New("agent is referenced by team worker")

// ErrAgentReferencedByTeamLead indicates that an avatar remains the retained
// lead of an active or archived team.
var ErrAgentReferencedByTeamLead = errors.New("agent is referenced by team lead")

// ErrAgentReferencedByTeamContext preserves legacy team_id evidence until a
// needs-repair organization record is explicitly repaired or archived.
var ErrAgentReferencedByTeamContext = errors.New("agent is referenced by legacy team context")
