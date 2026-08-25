package frozen

import "encoding/json"

const FrozenSchemaVersion = 1

// CapabilityManifestSchemaVersion is independent from the outer bundle
// schema. Version 2 adds the frozen agent role and content hash proof.
const CapabilityManifestSchemaVersion = 2

type FactoryKey struct {
	FactoryID      string `json:"factory_id"`
	FactoryVersion string `json:"factory_version"`
	CompilerABI    string `json:"compiler_abi"`
}

type FrozenAgentIdentity struct {
	Core     string `json:"core"`
	Extended string `json:"extended"`
	Raw      string `json:"raw"`
}

type FrozenAgentProfile struct {
	SystemAddition string `json:"system_addition"`
	Greeting       string `json:"greeting"`
}

type FrozenPermissions struct {
	Deny  []string `json:"deny"`
	Allow []string `json:"allow"`
	Ask   []string `json:"ask"`
}

type FrozenMemoryConfig struct {
	Enabled      bool   `json:"enabled"`
	TopK         int64  `json:"top_k"`
	AutoRemember bool   `json:"auto_remember"`
	Scope        string `json:"scope"`
}

type FrozenMemorySlot struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type FrozenAgentLimits struct {
	MaxCostUSD      float64 `json:"max_cost_usd"`
	MaxTokens       int64   `json:"max_tokens"`
	MaxOutputTokens int64   `json:"max_output_tokens"`
	StepBudget      int64   `json:"step_budget"`
	MaxToolRepeats  int64   `json:"max_tool_repeats"`
}

type FrozenFallback struct {
	Models  []string `json:"models"`
	Retries int64    `json:"retries"`
}

type FrozenGuard struct {
	Enabled      bool     `json:"enabled"`
	MaxInputLen  int64    `json:"max_input_len"`
	BlockedTerms []string `json:"blocked_terms"`
}

type FrozenCompaction struct {
	Enabled        bool  `json:"enabled"`
	TokenThreshold int64 `json:"token_threshold"`
}

type FrozenAgentRecord struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID  string `json:"workspace_id"`
	AgentID      string `json:"agent_id"`
	AgentVersion int64  `json:"agent_version"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	Engine       string `json:"engine"`
	RuntimeID    string `json:"runtime_id"`
	Model        string `json:"model"`

	SystemPrompt string                        `json:"system_prompt"`
	Identity     FrozenAgentIdentity           `json:"identity"`
	Profiles     map[string]FrozenAgentProfile `json:"profiles"`
	Permissions  FrozenPermissions             `json:"permissions"`
	MemoryConfig *FrozenMemoryConfig           `json:"memory_config"`
	MemorySlots  []FrozenMemorySlot            `json:"memory_slots"`
	OutputSchema json.RawMessage               `json:"output_schema"`
	Limits       FrozenAgentLimits             `json:"limits"`
	Fallback     FrozenFallback                `json:"fallback"`
	Guard        *FrozenGuard                  `json:"guard"`
	Compaction   *FrozenCompaction             `json:"compaction"`
	GraphType    string                        `json:"graph_type"`
	FactoryInput json.RawMessage               `json:"factory_input"`
}

type FrozenSkillResource struct {
	Kind         string `json:"kind"`
	RelativePath string `json:"relative_path"`
	MediaType    string `json:"media_type"`
	Encoding     string `json:"encoding"`
	Content      string `json:"content"`
	ContentHash  string `json:"content_hash"`
}

type FrozenSkill struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID  string                `json:"workspace_id"`
	Name         string                `json:"name"`
	SourceType   string                `json:"source_type"`
	SkillID      string                `json:"skill_id"`
	SkillVersion *int64                `json:"skill_version"`
	Description  string                `json:"description"`
	Body         string                `json:"body"`
	AlwaysActive bool                  `json:"always_active"`
	Resources    []FrozenSkillResource `json:"resources"`
	ContentHash  string                `json:"content_hash"`
}

type CredentialKind string

const (
	CredentialProviderAPIKey       CredentialKind = "provider_api_key"
	CredentialMCPServerAccess      CredentialKind = "mcp_server_access"
	CredentialRuntimeAccess        CredentialKind = "runtime_access"
	CredentialDeliveryTargetAccess CredentialKind = "delivery_target_access"
)

type CredentialReference struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID       string         `json:"workspace_id"`
	Kind              CredentialKind `json:"kind"`
	ResourceID        string         `json:"resource_id"`
	Slot              string         `json:"slot"`
	CredentialVersion *int64         `json:"credential_version"`
}

type FrozenMCPBinding struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID    string              `json:"workspace_id"`
	ServerID       string              `json:"server_id"`
	ServerRevision int64               `json:"server_revision"`
	Transport      string              `json:"transport"`
	URL            string              `json:"url"`
	Command        string              `json:"command"`
	Args           []string            `json:"args"`
	Filter         []string            `json:"filter"`
	WriteTools     []string            `json:"write_tools"`
	AccessRef      CredentialReference `json:"access_ref"`
	ContentHash    string              `json:"content_hash"`
}

type FrozenModelBinding struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID      string              `json:"workspace_id"`
	ProviderID       string              `json:"provider_id"`
	ProviderRevision int64               `json:"provider_revision"`
	ModelID          string              `json:"model_id"`
	BaseURL          string              `json:"base_url"`
	JSONObjectMode   bool                `json:"json_object_mode"`
	CredentialRef    CredentialReference `json:"credential_ref"`
	ContentHash      string              `json:"content_hash"`
}

type FrozenRuntimeBinding struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID     string              `json:"workspace_id"`
	RuntimeID       string              `json:"runtime_id"`
	Engine          string              `json:"engine"`
	RuntimeRevision int64               `json:"runtime_revision"`
	AccessRef       CredentialReference `json:"access_ref"`
	ContentHash     string              `json:"content_hash"`
}

type FrozenDeliveryCredentialBinding struct {
	HeaderName    string              `json:"header_name"`
	CredentialRef CredentialReference `json:"credential_ref"`
}

type FrozenDeliveryTarget struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID        string                            `json:"workspace_id"`
	TargetID           string                            `json:"target_id"`
	TargetRevision     int64                             `json:"target_revision"`
	Kind               string                            `json:"kind"`
	Transport          string                            `json:"transport"`
	URL                string                            `json:"url"`
	Method             string                            `json:"method"`
	ContentType        string                            `json:"content_type"`
	TimeoutSeconds     int64                             `json:"timeout_seconds"`
	CredentialBindings []FrozenDeliveryCredentialBinding `json:"credential_bindings"`
	AccessRef          CredentialReference               `json:"access_ref"`
	ContentHash        string                            `json:"content_hash"`
}

type FrozenTeamWorker struct {
	SchemaVersion int `json:"schema_version"`

	WorkspaceID        string   `json:"workspace_id"`
	TeamID             string   `json:"team_id"`
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
	Enabled            bool     `json:"enabled"`
}

type EnumeratedDependencyRef struct {
	WorkspaceID       string `json:"workspace_id"`
	OwnerType         string `json:"owner_type"`
	OwnerID           string `json:"owner_id"`
	OwnerAgentVersion *int64 `json:"owner_agent_version"`
	DependencyType    string `json:"dependency_type"`
	DependencyKey     string `json:"dependency_key"`
	DependencyVersion *int64 `json:"dependency_version"`
}

type EnumeratedDependencyManifest struct {
	SchemaVersion int                       `json:"schema_version"`
	Dependencies  []EnumeratedDependencyRef `json:"dependencies"`
}

type FrozenDependencyRef struct {
	EnumeratedDependencyRef
	ContentHash string `json:"content_hash"`
}

type FrozenDependencyManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	Dependencies  []FrozenDependencyRef `json:"dependencies"`
	ManifestHash  string                `json:"manifest_hash"`
}

type CapabilityManifest struct {
	SchemaVersion      int      `json:"schema_version"`
	Role               string   `json:"role"`
	AgentContentHash   string   `json:"agent_content_hash"`
	MayYield           bool     `json:"may_yield"`
	InteractiveStepIDs []string `json:"interactive_step_ids"`
	InteractiveToolIDs []string `json:"interactive_tool_ids"`
	MayInvokeAgent     bool     `json:"may_invoke_agent"`
	AgentStepIDs       []string `json:"agent_step_ids"`
}

// TeamInteractionAuthorizedWorker is the frozen catalog projection shared by
// consult and dispatch authorization.
type TeamInteractionAuthorizedWorker struct {
	WorkerAgentID      string `json:"worker_agent_id"`
	WorkerAgentVersion int64  `json:"worker_agent_version"`
	DisplayName        string `json:"display_name"`
	Duty               string `json:"duty"`
	WhenToUse          string `json:"when_to_use"`
	ContextInstruction string `json:"context_instruction"`
	ResultRequirement  string `json:"result_requirement"`
	DefaultKind        string `json:"default_kind"`
}

// TeamInteractionHandoffRoute binds a canonical route key to one exact frozen
// worker version.
type TeamInteractionHandoffRoute struct {
	RouteKey           string `json:"route_key"`
	WorkerAgentID      string `json:"worker_agent_id"`
	WorkerAgentVersion int64  `json:"worker_agent_version"`
	DisplayName        string `json:"display_name"`
}

// TeamInteractionCatalog is the canonical, snapshot-bound authorization
// catalog. Map keys are stable worker IDs or canonical handoff route keys.
type TeamInteractionCatalog struct {
	WorkspaceID   string                                     `json:"workspace_id"`
	TeamID        string                                     `json:"team_id"`
	RunID         string                                     `json:"run_id"`
	RunSnapshotID string                                     `json:"run_snapshot_id"`
	LeadAgentID   string                                     `json:"lead_agent_id"`
	Consult       map[string]TeamInteractionAuthorizedWorker `json:"consult"`
	Dispatch      map[string]TeamInteractionAuthorizedWorker `json:"dispatch"`
	Handoff       map[string]TeamInteractionHandoffRoute     `json:"handoff"`
}

type FrozenExecutionBundle struct {
	SchemaVersion int `json:"schema_version"`

	FactoryKey     FactoryKey               `json:"factory_key"`
	Agent          FrozenAgentRecord        `json:"agent"`
	Skills         []FrozenSkill            `json:"skills"`
	MCPBindings    []FrozenMCPBinding       `json:"mcp_bindings"`
	PrimaryModel   FrozenModelBinding       `json:"primary_model,omitzero"`
	FallbackModels []FrozenModelBinding     `json:"fallback_models"`
	Runtime        *FrozenRuntimeBinding    `json:"runtime"`
	Credentials    []CredentialReference    `json:"credentials"`
	Dependencies   FrozenDependencyManifest `json:"dependencies"`
	Capability     CapabilityManifest       `json:"capability"`
}

const (
	ArtifactSchemaVersion             = 1
	ArtifactCanonicalizationAlgorithm = "rfc8785+jcs-preorder"
	ArtifactCanonicalizationVersion   = 1
	ArtifactHashAlgorithm             = "sha256"
)

type ArtifactTeamV1 struct {
	WorkspaceID          string             `json:"workspace_id"`
	TeamID               string             `json:"team_id"`
	LeadAgentID          string             `json:"lead_agent_id"`
	LeadAgentVersion     int64              `json:"lead_agent_version,omitempty"`
	LeadAgentContentHash string             `json:"lead_agent_content_hash,omitempty"`
	Workers              []FrozenTeamWorker `json:"workers"`
}

type ArtifactPayloadV1 struct {
	SchemaVersion   int                     `json:"schema_version"`
	TriggerConfig   json.RawMessage         `json:"trigger_config"`
	GraphDefinition json.RawMessage         `json:"graph_definition"`
	Team            ArtifactTeamV1          `json:"team"`
	Bundles         []FrozenExecutionBundle `json:"bundles"`
	DeliveryTargets []FrozenDeliveryTarget  `json:"delivery_targets"`
}

// ArtifactEnvelopeV1 is the public artifact contract carrying workflow
// identity, canonicalization and hash columns, and the encoded payload.
// It corresponds to the docs/超级能力/规格/2026-07-23-P0-工作流-Publication-Foundation-合同.md Section 6.3 (:798-808) envelope contract.
type ArtifactEnvelopeV1 struct {
	WorkspaceID               string          `json:"workspace_id"`
	WorkflowID                string          `json:"workflow_id"`
	WorkflowVersion           int             `json:"workflow_version"`
	ArtifactSchemaVersion     int             `json:"artifact_schema_version"`
	CanonicalizationAlgorithm string          `json:"canonicalization_algorithm"`
	CanonicalizationVersion   int             `json:"canonicalization_version"`
	HashAlgorithm             string          `json:"hash_algorithm"`
	ContentHash               string          `json:"content_hash"`
	Payload                   json.RawMessage `json:"payload"`
}

type ArtifactEnvelopeHashInputV1 struct {
	WorkspaceID               string            `json:"workspace_id"`
	WorkflowID                string            `json:"workflow_id"`
	WorkflowVersion           int               `json:"workflow_version"`
	ArtifactSchemaVersion     int               `json:"artifact_schema_version"`
	CanonicalizationAlgorithm string            `json:"canonicalization_algorithm"`
	CanonicalizationVersion   int               `json:"canonicalization_version"`
	HashAlgorithm             string            `json:"hash_algorithm"`
	Payload                   ArtifactPayloadV1 `json:"payload"`
}
