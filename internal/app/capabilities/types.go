// Package capabilities owns stable service-application identities and exact,
// immutable releases of already-published workflow capabilities.
package capabilities

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	ContractDialectV1 = "weave.fixed-json-schema-v1"
	NormalizationV1   = "rfc8785-jcs-v1"
	InputMappingV1    = `{"schema_version":1,"mode":"direct_run_input"}`
)

var (
	ErrInvalid           = errors.New("invalid capability service request")
	ErrNotFound          = errors.New("capability service record not found")
	ErrDisabled          = errors.New("capability service record disabled")
	ErrCredentialInvalid = errors.New("service app credential invalid")
	ErrReleaseUnsafe     = errors.New("capability release is outside the no-tool execution profile")
	ErrReleaseConflict   = errors.New("capability release version conflict")
	ErrGrantConflict     = errors.New("capability release grant conflict")
	ErrGrantUnavailable  = errors.New("capability release grant unavailable")
)

type ServiceApp struct {
	WorkspaceID              string    `json:"workspace_id"`
	ID                       string    `json:"id"`
	Name                     string    `json:"name"`
	Description              string    `json:"description"`
	Enabled                  bool      `json:"enabled"`
	MaxConcurrentInvocations int       `json:"max_concurrent_invocations"`
	CreatedBy                string    `json:"created_by"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type Credential struct {
	WorkspaceID string     `json:"workspace_id"`
	ID          string     `json:"id"`
	AppID       string     `json:"app_id"`
	Name        string     `json:"name"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Principal struct {
	WorkspaceID  string
	AppID        string
	CredentialID string
	Scopes       []string
}

func (principal Principal) HasScope(scope string) bool {
	for _, candidate := range principal.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type Capability struct {
	WorkspaceID string    `json:"workspace_id"`
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ExecutionLimits struct {
	MaxInputBytes           int `json:"max_input_bytes"`
	MaxOutputBytes          int `json:"max_output_bytes"`
	MaxNestingDepth         int `json:"max_nesting_depth"`
	MaxObjectFields         int `json:"max_object_fields"`
	MaxArrayItems           int `json:"max_array_items"`
	MaxStringBytes          int `json:"max_string_bytes"`
	QueueTimeoutSeconds     int `json:"queue_timeout_seconds"`
	ExecutionTimeoutSeconds int `json:"execution_timeout_seconds"`
	MaxOutputTokens         int `json:"max_output_tokens"`
}

type ResultPolicy struct {
	ExposedFields           []string `json:"exposed_fields"`
	IncludeUsage            bool     `json:"include_usage"`
	IncludeContractEvidence bool     `json:"include_contract_evidence"`
}

type Release struct {
	WorkspaceID         string          `json:"workspace_id"`
	CapabilityID        string          `json:"capability_id"`
	Version             int             `json:"version"`
	ID                  string          `json:"id"`
	WorkflowID          string          `json:"workflow_id"`
	WorkflowVersion     int             `json:"workflow_version"`
	ArtifactContentHash string          `json:"artifact_content_hash"`
	ContractDialect     string          `json:"contract_dialect"`
	Normalization       string          `json:"normalization"`
	InputContract       json.RawMessage `json:"input_contract"`
	OutputContract      json.RawMessage `json:"output_contract"`
	InputMapping        json.RawMessage `json:"input_mapping"`
	ExecutionLimits     ExecutionLimits `json:"execution_limits"`
	ResultPolicy        ResultPolicy    `json:"result_policy"`
	Enabled             bool            `json:"enabled"`
	CreatedBy           string          `json:"created_by"`
	CreatedAt           time.Time       `json:"created_at"`
}

type Grant struct {
	WorkspaceID    string     `json:"workspace_id"`
	ID             string     `json:"id"`
	AppID          string     `json:"app_id"`
	CapabilityID   string     `json:"capability_id"`
	ReleaseVersion int        `json:"release_version"`
	MaxConcurrent  int        `json:"max_concurrent"`
	GrantedBy      string     `json:"granted_by"`
	GrantedAt      time.Time  `json:"granted_at"`
	RevokedBy      *string    `json:"revoked_by,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}
