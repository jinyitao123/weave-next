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
	ErrInvalid               = errors.New("invalid capability service request")
	ErrNotFound              = errors.New("capability service record not found")
	ErrDisabled              = errors.New("capability service record disabled")
	ErrCredentialInvalid     = errors.New("service app credential invalid")
	ErrReleaseUnsafe         = errors.New("capability release is outside the no-tool execution profile")
	ErrReleaseConflict       = errors.New("capability release version conflict")
	ErrGrantConflict         = errors.New("capability release grant conflict")
	ErrGrantUnavailable      = errors.New("capability release grant unavailable")
	ErrScopeDenied           = errors.New("service app scope denied")
	ErrRequestConflict       = errors.New("capability request identity conflict")
	ErrCancelledBeforeSubmit = errors.New("capability request was cancelled before submission")
	ErrCapacityExceeded      = errors.New("capability invocation capacity exceeded")
	ErrConcurrencyBusy       = errors.New("capability concurrency key is already active")
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

// HasScope reports whether this authenticated service credential grants one
// of the three public invocation operations.
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

type Invocation struct {
	WorkspaceID         string    `json:"-"`
	AppID               string    `json:"app_id"`
	InvocationID        string    `json:"invocation_id"`
	RequestID           string    `json:"request_id"`
	CapabilityID        string    `json:"capability_id"`
	ReleaseVersion      int       `json:"release_version"`
	ReleaseID           string    `json:"release_id"`
	GrantID             string    `json:"-"`
	CredentialID        string    `json:"-"`
	ArtifactContentHash string    `json:"artifact_content_hash"`
	Normalization       string    `json:"normalization"`
	InputHash           string    `json:"input_hash"`
	RequestFingerprint  string    `json:"-"`
	ConcurrencyKey      *string   `json:"concurrency_key,omitempty"`
	RunID               string    `json:"run_id"`
	TaskID              string    `json:"task_id"`
	AcceptedAt          time.Time `json:"accepted_at"`
	DeadlineAt          time.Time `json:"deadline_at"`
}

type InvocationReceipt struct {
	Invocation Invocation `json:"invocation"`
	Replay     bool       `json:"replay"`
	QueryPath  string     `json:"query_path"`
}

type SubmitInvocationRequest struct {
	Principal      Principal
	CapabilityID   string
	ReleaseVersion int
	RequestID      string
	Input          json.RawMessage
	ConcurrencyKey *string
}

type CancelInvocationRequest struct {
	Principal    Principal
	RequestID    string
	InvocationID string
	Reason       string
}

type CancellationReceipt struct {
	RequestID     string `json:"request_id"`
	InvocationID  string `json:"invocation_id,omitempty"`
	Status        string `json:"status"`
	AlreadyExists bool   `json:"already_exists"`
}

type SchemaViolationError struct {
	Problems []SchemaProblem `json:"problems"`
}

func (err *SchemaViolationError) Error() string { return "capability input violates its fixed schema" }

type SchemaProblem struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

type ResourceLimitError struct {
	Limit string `json:"limit"`
}

func (err *ResourceLimitError) Error() string { return "capability value exceeds " + err.Limit }

type InvocationStatus struct {
	Invocation         Invocation        `json:"invocation"`
	ExecutionStatus    string            `json:"execution_status"`
	ResultAvailability string            `json:"result_availability"`
	Result             json.RawMessage   `json:"result,omitempty"`
	ResultProblems     []SchemaProblem   `json:"result_problems,omitempty"`
	CancellationStatus string            `json:"cancellation_status"`
	UpdatedAt          *time.Time        `json:"updated_at,omitempty"`
	TerminalAt         *time.Time        `json:"terminal_at,omitempty"`
	FailureCode        string            `json:"failure_code,omitempty"`
	ContractEvidence   *ContractEvidence `json:"contract_evidence,omitempty"`
	UsageAvailable     *bool             `json:"usage_available,omitempty"`
}

type ContractEvidence struct {
	InputHash           string `json:"input_hash"`
	ArtifactContentHash string `json:"artifact_content_hash"`
	ContractDialect     string `json:"contract_dialect"`
	Normalization       string `json:"normalization"`
	OutputValidated     bool   `json:"output_validated"`
}
