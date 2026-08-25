package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

func resolverError(code string, cause error) error {
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
	Name        string  `json:"name"`
	ID          string  `json:"id,omitempty"`
	WorkspaceID string  `json:"workspace_id,omitempty"`
	TeamID      string  `json:"team_id,omitempty"`
	OwnerUserID *string `json:"owner_user_id,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
	Role        string  `json:"role,omitempty"`       // worker|avatar; empty defaults to worker
	Visibility  string  `json:"visibility,omitempty"` // public|internal_tool|platform; empty defaults to public
	Engine      string  `json:"engine,omitempty"`     // ""/"loom" in-process; "opencode"|"codex"|"claude" external CLI runtime
	RuntimeID   string  `json:"runtime_id,omitempty"`
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

// validateSkillRefs enforces the SkillRef binding contract on create/update.
// registry_version refs must pin an exact positive SkillVersion (fail-closed
// with ErrSkillVersionRequired so the D3b validator can reuse the code);
// every ref needs a non-empty matching name; names are unique per record.
func validateSkillRefs(rec *AgentRecord) error {
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
			// Resolved by name through the D2 live path; no version pin.
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

// AgentRegistry provides CRUD operations on agent specs.
type AgentRegistry struct {
	pool *pgxpool.Pool
}

// New creates an AgentRegistry backed by PostgreSQL.
func New(pool *pgxpool.Pool) *AgentRegistry {
	return &AgentRegistry{pool: pool}
}

// Get retrieves the latest version of an agent.
func (r *AgentRegistry) Get(ctx context.Context, tenant, name string) (*AgentRecord, error) {
	var visibility string
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, team_id, owner_user_id, display_name, role, spec, version, visibility
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, name)
	rec, err := scanAgentRecord(row, tenant, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// GetVersion retrieves one immutable historical AgentRecord by stable ID and
// version. The workspace join is an authorization boundary; a missing or
// foreign version is never replaced with the current record.
func (r *AgentRegistry) GetVersion(ctx context.Context, tenant, agentID string, version int) (*AgentRecord, error) {
	var data []byte
	err := r.pool.QueryRow(ctx, `
		SELECT version.spec
		FROM weave_agent_versions AS version
		JOIN weave_agents AS agent
		  ON agent.workspace_id=version.workspace_id
		 AND agent.id=version.agent_id
		WHERE version.workspace_id=$1
		  AND version.agent_id=$2
		  AND version.version=$3
	`, tenant, agentID, version).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("agent version %q@%d not found for tenant %q", agentID, version, tenant)
	}
	if err != nil {
		return nil, err
	}

	var rec AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("corrupt agent version %q@%d: %w", agentID, version, err)
	}
	if rec.ID != agentID || rec.WorkspaceID != tenant || rec.Version != version || rec.Name == "" {
		return nil, fmt.Errorf("corrupt agent version %q@%d: frozen identity does not match version key", agentID, version)
	}
	return &rec, nil
}

// ResolveAgentVersionTx reads and locks one exact immutable AgentRecord using
// the caller-owned transaction. It never resolves through the mutable head.
func (r *AgentRegistry) ResolveAgentVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, agentID string,
	dependencyVersion *int64,
) (*AgentRecord, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(agentID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		agentID != strings.TrimSpace(agentID) ||
		dependencyVersion == nil ||
		*dependencyVersion < 1 ||
		*dependencyVersion > frozen.MaxJCSSafeInteger {
		return nil, resolverError(CodeDependencyVersionRequired, nil)
	}

	var data []byte
	err := tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, agentID, *dependencyVersion).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyVersionRequired, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("read exact agent version: %w", err)
	}

	var rec AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, resolverError(CodeFrozenManifestMismatch, err)
	}
	if rec.ID != agentID ||
		rec.WorkspaceID != workspaceID ||
		int64(rec.Version) != *dependencyVersion ||
		strings.TrimSpace(rec.Name) == "" {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}
	return &rec, nil
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

// ResolveAgentHeadVersionsTx locks the exact agent head rows of one workspace
// and returns their current version numbers using the caller-owned
// transaction. Missing or deleted agents fail closed, so a baseline can never
// pin a ghost roster reference.
func (r *AgentRegistry) ResolveAgentHeadVersionsTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	agentIDs []string,
) (map[string]int64, error) {
	if tx == nil || strings.TrimSpace(workspaceID) == "" || len(agentIDs) == 0 {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}
	unique := make([]string, 0, len(agentIDs))
	seen := make(map[string]struct{}, len(agentIDs))
	for _, agentID := range agentIDs {
		if strings.TrimSpace(agentID) == "" || agentID != strings.TrimSpace(agentID) {
			return nil, resolverError(CodeDependencyUnenumerable, nil)
		}
		if _, exists := seen[agentID]; exists {
			continue
		}
		seen[agentID] = struct{}{}
		unique = append(unique, agentID)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, version
		FROM weave_agents
		WHERE workspace_id=$1 AND id=ANY($2::text[]) AND deleted=false
		ORDER BY id COLLATE "C"
		FOR SHARE
	`, workspaceID, unique)
	if err != nil {
		return nil, fmt.Errorf("lock roster agent heads: %w", err)
	}
	defer rows.Close()

	versions := make(map[string]int64, len(unique))
	for rows.Next() {
		var agentID string
		var version int64
		if err := rows.Scan(&agentID, &version); err != nil {
			return nil, fmt.Errorf("scan roster agent head: %w", err)
		}
		if version < 1 {
			return nil, resolverError(CodeFrozenManifestMismatch, nil)
		}
		versions[agentID] = version
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read roster agent heads: %w", err)
	}
	for _, agentID := range unique {
		if _, exists := versions[agentID]; !exists {
			return nil, fmt.Errorf(
				"roster agent %q not found in workspace %q",
				agentID,
				workspaceID,
			)
		}
	}
	return versions, nil
}

// ResolveAgentVersionContentTx locks and reads one exact immutable AgentRecord
// spec plus its canonical content hash using the caller-owned transaction. It
// never resolves through the mutable head and mirrors the identity validation
// of ResolveAgentVersionTx.
func (r *AgentRegistry) ResolveAgentVersionContentTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, agentID string,
	version int64,
) (*AgentVersionContent, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(agentID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		agentID != strings.TrimSpace(agentID) ||
		version < 1 ||
		version > frozen.MaxJCSSafeInteger {
		return nil, resolverError(CodeDependencyVersionRequired, nil)
	}

	var data []byte
	err := tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, agentID, version).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyVersionRequired, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("read exact agent version content: %w", err)
	}

	var rec AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, resolverError(CodeFrozenManifestMismatch, err)
	}
	if rec.ID != agentID ||
		rec.WorkspaceID != workspaceID ||
		int64(rec.Version) != version ||
		strings.TrimSpace(rec.Name) == "" {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}
	canonical, err := frozen.CanonicalizeJSON(data)
	if err != nil {
		return nil, resolverError(CodeFrozenManifestMismatch, err)
	}
	digest := sha256.Sum256(canonical)
	return &AgentVersionContent{
		AgentID:     agentID,
		Name:        rec.Name,
		Version:     rec.Version,
		ContentHash: hex.EncodeToString(digest[:]),
		Engine:      rec.Engine,
		RuntimeID:   rec.RuntimeID,
		Model:       rec.Model,
	}, nil
}

// List returns all non-deleted agents for a tenant.
func (r *AgentRegistry) List(ctx context.Context, tenant string) ([]AgentRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, team_id, owner_user_id, display_name, role, spec, version, visibility
		FROM weave_agents
		WHERE workspace_id=$1 AND deleted=false
		ORDER BY name
	`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []AgentRecord
	for rows.Next() {
		var visibility string
		rec, err := scanAgentRecord(rows, tenant, &visibility)
		if err != nil {
			return nil, err
		}
		records = append(records, *rec)
	}
	return records, rows.Err()
}

// ListWorkspaces returns the distinct workspace IDs that hold live agents.
// Used by the startup upgrade scan to diagnose pre-workspace-scoping
// deployments; not part of the request path.
func (r *AgentRegistry) ListWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT workspace_id
		FROM weave_agents
		WHERE deleted=false
		ORDER BY workspace_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ws)
	}
	return workspaces, rows.Err()
}

// Put creates or updates an agent, bumping the version.
func (r *AgentRegistry) Put(ctx context.Context, tenant string, rec *AgentRecord) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := r.PutTx(ctx, tx, tenant, rec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutTx creates or updates an agent using the caller's transaction.
// It shares Put's insert, conflict, version, and ownership behavior, but leaves
// commit and rollback to the caller.
func (r *AgentRegistry) PutTx(ctx context.Context, tx pgx.Tx, tenant string, rec *AgentRecord) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name)
		VALUES ($1, $1, $1)
		ON CONFLICT DO NOTHING
	`, tenant); err != nil {
		return err
	}
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}
	// Postgres timestamptz has microsecond resolution; truncate so the returned
	// CreatedAt/UpdatedAt match what a later Get reads back from the DB (and so
	// the value is stable across the macOS ns-clock vs Linux ns-clock split).
	now := time.Now().Truncate(time.Microsecond)
	var id string
	var version int
	var createdAt time.Time
	var existingOwnerUserID *string
	var existingTeamID *string
	var existingRole string
	agentExists := true
	err := tx.QueryRow(ctx, `
		SELECT id, version, created_at, owner_user_id, team_id, role
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2
		FOR UPDATE
	`, tenant, rec.Name).Scan(
		&id, &version, &createdAt, &existingOwnerUserID, &existingTeamID, &existingRole,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		agentExists = false
		id = uuid.NewString()
		version = 1
		createdAt = now
	case err != nil:
		return err
	default:
		version++
	}
	if ownerUserIDRequiresValidation(agentExists, existingOwnerUserID, rec.OwnerUserID) {
		isMember, err := workspaceMemberExists(ctx, tx, tenant, *rec.OwnerUserID)
		if err != nil {
			return err
		}
		if !isMember {
			return fmt.Errorf("%w: user %q is not a member of workspace %q", ErrOwnerNotWorkspaceMember, *rec.OwnerUserID, tenant)
		}
	}

	if rec.Role == "" {
		rec.Role = "worker"
	}
	// SkillRef binding contract (same convention as ValidateTeamWorkerAgentRecord:
	// PutTx is the single enforcement point so create/update and every other
	// registry write share the same validation).
	if err := validateSkillRefs(rec); err != nil {
		return err
	}
	if rec.Visibility == "" {
		rec.Visibility = VisibilityPublic
	}
	if !ValidAgentVisibility(rec.Visibility) {
		return fmt.Errorf(
			"invalid agent visibility %q: must be %s, %s, or %s",
			rec.Visibility,
			VisibilityPublic,
			VisibilityInternalTool,
			VisibilityPlatform,
		)
	}
	if agentExists {
		var referencedAsWorker, referencedAsLead bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM weave_team_workers
				WHERE workspace_id=$1 AND worker_agent_id=$2
			), EXISTS (
				SELECT 1 FROM weave_teams
				WHERE workspace_id=$1 AND lead_avatar_id=$2
			)
		`, tenant, id).Scan(&referencedAsWorker, &referencedAsLead); err != nil {
			return err
		}
		if rec.Role != existingRole && (referencedAsWorker || referencedAsLead || existingTeamID != nil) {
			return fmt.Errorf("%w: agent %q", ErrAgentRoleReferencedByTeam, rec.Name)
		}
		if referencedAsWorker {
			if err := ValidateTeamWorkerAgentRecord(rec); err != nil {
				return err
			}
		}
	}
	// Legacy team_id is a read-only migration compatibility field. New agents
	// never receive it and updates preserve the existing database value.
	rec.TeamID = ""
	if existingTeamID != nil {
		rec.TeamID = *existingTeamID
	}
	rec.ID = id
	rec.WorkspaceID = tenant
	rec.Version = version
	rec.CreatedAt = createdAt
	rec.UpdatedAt = now
	rec.Deleted = false

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if agentExists {
		tag, err := tx.Exec(ctx, `
			UPDATE weave_agents
			SET owner_user_id=$3,
				display_name=$4,
				role=$5,
				visibility=$6,
				spec=$7,
				version=$8,
				updated_at=$9,
				deleted=false
			WHERE id=$1 AND workspace_id=$2 AND name=$10
		`, id, tenant, rec.OwnerUserID, rec.DisplayName, rec.Role, rec.Visibility, data, version, now, rec.Name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("update agent %q: locked identity changed", rec.Name)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_agents (
				id, workspace_id, team_id, owner_user_id, name, display_name, role,
				visibility, spec, version, deleted, created_at, updated_at
			)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, false, $11, $12)
		`, id, tenant, rec.TeamID, rec.OwnerUserID, rec.Name, rec.DisplayName, rec.Role, rec.Visibility, data, version, createdAt, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agent_versions (workspace_id, agent_id, version, spec)
		VALUES ($1, $2, $3, $4)
	`, tenant, id, version, data); err != nil {
		return err
	}
	return nil
}

// ErrAgentRoleReferencedByTeam prevents a Team lead or worker from changing
// the role fact required by its existing relationship.
var ErrAgentRoleReferencedByTeam = errors.New("agent role is referenced by a team")

// ErrOwnerNotWorkspaceMember indicates that an agent owner does not belong to
// the agent's workspace.
var ErrOwnerNotWorkspaceMember = errors.New("agent owner is not a workspace member")

// IsWorkspaceMember reports whether a live user belongs to the workspace.
// The join checks both the membership row and the user's tenant boundary.
func (r *AgentRegistry) IsWorkspaceMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	return workspaceMemberExists(ctx, r.pool, workspaceID, userID)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func workspaceMemberExists(ctx context.Context, q queryRower, workspaceID, userID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_members AS member
			JOIN weave_users AS owner_user
			  ON owner_user.id=member.user_id
			 AND owner_user.tenant_id=member.workspace_id
			WHERE member.workspace_id=$1 AND member.user_id=$2
		)
	`, workspaceID, userID).Scan(&exists)
	return exists, err
}

func ownerUserIDRequiresValidation(agentExists bool, existingOwnerUserID, newOwnerUserID *string) bool {
	if newOwnerUserID == nil {
		return false
	}
	if !agentExists || existingOwnerUserID == nil {
		return true
	}
	return *existingOwnerUserID != *newOwnerUserID
}

// ErrAgentReferencedByTeamWorker indicates that an agent remains part of a
// team roster and must be unlinked before it can be soft-deleted.
var ErrAgentReferencedByTeamWorker = errors.New("agent is referenced by team worker")

// ErrAgentReferencedByTeamLead indicates that an avatar remains the retained
// lead of an active or archived team.
var ErrAgentReferencedByTeamLead = errors.New("agent is referenced by team lead")

// ErrAgentReferencedByTeamContext preserves legacy team_id evidence until a
// needs-repair organization record is explicitly repaired or archived.
var ErrAgentReferencedByTeamContext = errors.New("agent is referenced by legacy team context")

// Delete soft-deletes an agent after excluding TeamWorker references.
func (r *AgentRegistry) Delete(ctx context.Context, tenant, name string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}

	// TeamWorker writers lock their workspace-scoped team before touching a
	// roster. Locking this workspace's teams in stable order prevents a new
	// relation from racing the reference check below.
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM weave_teams
		WHERE workspace_id=$1
		ORDER BY id
		FOR UPDATE
	`, tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	var agentID string
	var legacyTeamID *string
	err = tx.QueryRow(ctx, `
		SELECT id, team_id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2
		FOR UPDATE
	`, tenant, name).Scan(&agentID, &legacyTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	if err != nil {
		return err
	}

	var referenced bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_team_workers
			WHERE workspace_id=$1 AND worker_agent_id=$2
		)
	`, tenant, agentID).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: agent %q", ErrAgentReferencedByTeamWorker, name)
	}
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_teams
			WHERE workspace_id=$1 AND lead_avatar_id=$2
		)
	`, tenant, agentID).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: agent %q", ErrAgentReferencedByTeamLead, name)
	}
	if legacyTeamID != nil {
		return fmt.Errorf("%w: agent %q team %q", ErrAgentReferencedByTeamContext, name, *legacyTeamID)
	}

	result, err := tx.Exec(ctx, `
		UPDATE weave_agents
		SET deleted=true, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, tenant, agentID)
	if err != nil {
		return err
	}
	affected := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	return tx.Commit(ctx)
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanAgentRecord overlays the authoritative head-row projection onto the spec
// JSON. Get/List pass the optional authoritative visibility-column destination;
// wrappers such as managedAgentRow may continue appending their own tail fields.
func scanAgentRecord(row rowScanner, tenant string, visibilityColumn ...*string) (*AgentRecord, error) {
	if len(visibilityColumn) > 1 || (len(visibilityColumn) == 1 && visibilityColumn[0] == nil) {
		return nil, fmt.Errorf("scan agent record: expected at most one non-nil visibility destination")
	}
	var (
		id          string
		name        string
		teamID      *string
		ownerUserID *string
		displayName string
		role        string
		data        []byte
		version     int
	)
	dest := []any{&id, &name, &teamID, &ownerUserID, &displayName, &role, &data, &version}
	if len(visibilityColumn) == 1 {
		dest = append(dest, visibilityColumn[0])
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	var rec AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("corrupt agent record %q: %w", name, err)
	}
	rec.ID = id
	rec.WorkspaceID = tenant
	rec.TeamID = ""
	if teamID != nil {
		rec.TeamID = *teamID
	}
	rec.OwnerUserID = ownerUserID
	rec.Name = name
	rec.DisplayName = displayName
	rec.Role = role
	if len(visibilityColumn) == 1 {
		rec.Visibility = *visibilityColumn[0]
	}
	// Normalize legacy peer agents after authoritative column values are applied.
	if rec.Role == "peer" {
		rec.Role = "worker"
	}
	rec.Version = version
	rec.Deleted = false
	return &rec, nil
}
