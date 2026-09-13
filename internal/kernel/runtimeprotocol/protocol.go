// Package runtimeprotocol defines the versioned, storage-free contract between
// the Weave platform and a Workbench runtime host.
package runtimeprotocol

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

const (
	ProtocolVersion = "weave.runtime/v1"
	ClaimSchemaV1   = 1
	RequestSchemaV1 = 1
	ReceiptSchemaV1 = 1
)

var ErrUnsupportedVersion = errors.New("runtime protocol version is unsupported")

type Versioned struct {
	ProtocolVersion string `json:"protocol_version"`
}

func (v Versioned) Validate() error {
	if v.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: %q", ErrUnsupportedVersion, v.ProtocolVersion)
	}
	return nil
}

type HostHelloRequest struct {
	Versioned
	Engines            []string           `json:"engines"`
	EngineCapabilities []EngineCapability `json:"engine_capabilities"`
	TotalSlots         int                `json:"total_slots"`
}

type HostHelloResponse struct {
	Versioned
	RuntimeID string `json:"runtime_id"`
	Name      string `json:"name"`
}

type EngineCapability struct {
	Engine              string `json:"engine"`
	BinaryPath          string `json:"binary_path"`
	BinaryVersion       string `json:"binary_version"`
	AuthMode            string `json:"auth_mode"`
	ProtocolVersion     string `json:"engine_protocol_version"`
	PublicEvents        bool   `json:"public_events,omitempty"`
	EndpointClass       string `json:"endpoint_class"`
	ConfiguredEndpoint  string `json:"configured_endpoint,omitempty"`
	ConfiguredModel     string `json:"configured_model,omitempty"`
	ConfigurationSource string `json:"configuration_source,omitempty"`
	Availability        string `json:"availability,omitempty"`
	UnavailableReason   string `json:"unavailable_reason,omitempty"`
}

type ClaimRequest struct {
	Versioned
	WaitSeconds int `json:"wait_seconds"`
}

type ClaimResponse struct {
	Versioned
	Claim *ExecutionClaim `json:"claim,omitempty"`
}

// ExecutionClaim contains only facts a Host needs to run one admitted
// physical attempt. Platform queue state and storage records never cross the
// wire.
type ExecutionClaim struct {
	SchemaVersion  int               `json:"schema_version"`
	TaskID         string            `json:"task_id"`
	WorkspaceID    string            `json:"workspace_id"`
	Subject        execution.Subject `json:"subject"`
	ClaimEpoch     int64             `json:"claim_epoch"`
	LeaseIssuedAt  time.Time         `json:"lease_issued_at"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`
	DeadlineAt     *time.Time        `json:"deadline_at,omitempty"`
	RunSnapshotID  string            `json:"run_snapshot_id,omitempty"`
	Agent          AgentIdentity     `json:"agent"`
	Request        ExecutionRequest  `json:"request"`
}

type AgentIdentity struct {
	ID             string          `json:"id"`
	Version        int             `json:"version"`
	Name           string          `json:"name"`
	ExecutionScope execution.Scope `json:"execution_scope"`
}

// ExecutionRequest is a redacted, immutable Host request. FrozenAgent carries
// the exact published execution definition as canonical JSON; its identity is
// repeated in ExecutionClaim and must match before execution.
type ExecutionRequest struct {
	SchemaVersion       int             `json:"schema_version"`
	Engine              string          `json:"engine"`
	Model               string          `json:"model,omitempty"`
	Prompt              string          `json:"prompt"`
	OutputSchema        json.RawMessage `json:"output_schema,omitempty"`
	FrozenAgent         json.RawMessage `json:"frozen_agent"`
	FrozenAgentHash     string          `json:"frozen_agent_hash"`
	LogicalInvocationID string          `json:"logical_invocation_id,omitempty"`
	NodeID              string          `json:"node_id,omitempty"`
	TimeoutSeconds      int             `json:"timeout_seconds,omitempty"`
	EngineVersion       string          `json:"engine_version,omitempty"`
	Attachments         []Attachment    `json:"attachments,omitempty"`
	InputFiles          []InputFile     `json:"input_files,omitempty"`
	TaskMCP             []TaskMCPTarget `json:"task_mcp,omitempty"`
	Loom                *LoomInput      `json:"loom,omitempty"`
}

type LoomInput struct {
	Messages        []contract.Message   `json:"messages"`
	LastUserMessage string               `json:"last_user_message"`
	SessionID       string               `json:"session_id,omitempty"`
	ConversationID  string               `json:"conversation_id,omitempty"`
	Profile         string               `json:"profile,omitempty"`
	Effort          contract.EffortLevel `json:"effort,omitempty"`
	Context         map[string]any       `json:"context,omitempty"`
	NoDispatch      bool                 `json:"no_dispatch,omitempty"`
	MCPServerCount  int                  `json:"mcp_server_count,omitempty"`
}

type Attachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

type InputFile struct {
	TaskID      string `json:"task_id"`
	NodeID      string `json:"node_id"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Content     string `json:"content"`
}

type TaskMCPTarget struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ExecutionReceipt reports one physical attempt. The platform remains the
// sole owner of task terminal state and accounting acceptance.
type ExecutionReceipt struct {
	Versioned
	SchemaVersion            int                              `json:"schema_version"`
	TaskID                   string                           `json:"task_id"`
	ClaimEpoch               int64                            `json:"claim_epoch"`
	Subject                  execution.Subject                `json:"subject"`
	Status                   string                           `json:"status"`
	Output                   string                           `json:"output,omitempty"`
	Error                    string                           `json:"error,omitempty"`
	SessionID                string                           `json:"session_id,omitempty"`
	RunID                    string                           `json:"run_id,omitempty"`
	StopReason               string                           `json:"stop_reason,omitempty"`
	Usage                    *contract.Usage                  `json:"usage,omitempty"`
	UsageReceipt             *engine.UsageReceipt             `json:"usage_receipt,omitempty"`
	ReportedModels           []string                         `json:"reported_models,omitempty"`
	RetrySafeBeforeExecution bool                             `json:"retry_safe_before_execution,omitempty"`
	Diagnostics              []engine.Diagnostic              `json:"diagnostics,omitempty"`
	Events                   []engine.Event                   `json:"events,omitempty"`
	Artifacts                []engine.Artifact                `json:"artifacts,omitempty"`
	ArtifactCollection       *fileartifact.CollectionEvidence `json:"artifact_collection,omitempty"`
}

type StoppedReceipt struct {
	Versioned
	SchemaVersion int               `json:"schema_version"`
	TaskID        string            `json:"task_id"`
	ClaimEpoch    int64             `json:"claim_epoch"`
	Subject       execution.Subject `json:"subject"`
}

func (claim ExecutionClaim) Validate() error {
	if claim.SchemaVersion != ClaimSchemaV1 || claim.Request.SchemaVersion != RequestSchemaV1 {
		return fmt.Errorf("%w: claim=%d request=%d", ErrUnsupportedVersion, claim.SchemaVersion, claim.Request.SchemaVersion)
	}
	if claim.TaskID == "" || claim.WorkspaceID == "" || claim.ClaimEpoch < 1 || claim.Agent.ID == "" || claim.Agent.Version < 1 || claim.Agent.Name == "" || claim.Request.Engine == "" || len(claim.Request.FrozenAgent) == 0 || !json.Valid(claim.Request.FrozenAgent) {
		return errors.New("runtime claim is incomplete")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(claim.Request.FrozenAgent))
	if claim.Request.FrozenAgentHash != digest {
		return errors.New("runtime claim frozen agent hash is invalid")
	}
	if err := claim.Subject.Validate(); err != nil || claim.Subject.WorkspaceID != claim.WorkspaceID {
		return errors.New("runtime claim subject is invalid")
	}
	if !claim.Agent.ExecutionScope.Valid() || !claim.LeaseExpiresAt.After(claim.LeaseIssuedAt) {
		return errors.New("runtime claim lease or execution identity is invalid")
	}
	if claim.DeadlineAt != nil && !claim.DeadlineAt.After(claim.LeaseIssuedAt) {
		return errors.New("runtime claim deadline is exhausted")
	}
	return nil
}

func (receipt ExecutionReceipt) ValidateFor(claim ExecutionClaim) error {
	if err := receipt.Versioned.Validate(); err != nil {
		return err
	}
	if receipt.SchemaVersion != ReceiptSchemaV1 {
		return fmt.Errorf("%w: receipt=%d", ErrUnsupportedVersion, receipt.SchemaVersion)
	}
	if receipt.TaskID != claim.TaskID || receipt.ClaimEpoch != claim.ClaimEpoch || receipt.Subject != claim.Subject {
		return errors.New("runtime receipt does not match its claim")
	}
	switch receipt.Status {
	case "completed":
		if receipt.Error != "" {
			return errors.New("completed runtime receipt contains an error")
		}
	case "failed", "timeout":
		if receipt.Error == "" {
			return errors.New("failed runtime receipt omitted its error")
		}
	default:
		return errors.New("runtime receipt status is invalid")
	}
	return nil
}

func NewVersioned() Versioned { return Versioned{ProtocolVersion: ProtocolVersion} }
