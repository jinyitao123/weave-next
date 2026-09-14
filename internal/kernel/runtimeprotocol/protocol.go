// Package runtimeprotocol defines the versioned, storage-free contract between
// the Weave platform and a Workbench runtime host.
package runtimeprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

const (
	ProtocolVersion = "weave.runtime/v1"
	HeaderVersion   = "X-Weave-Runtime-Protocol"
	ClaimSchemaV1   = 1
	RequestSchemaV1 = 1
	ReceiptSchemaV1 = 1

	AuthModeChatGPT  = "chatgpt"
	AuthModeOAuth    = "oauth"
	AuthModeProvider = "provider"
	AuthModeUnknown  = "unknown"

	EngineAvailabilityReady       = "ready"
	EngineAvailabilityUnavailable = "unavailable"
	EngineAvailabilityUnknown     = "unknown"
	EngineLoom                    = "loom"
	SubjectIsolationStrong        = "strong"
	SubjectIsolationSingleUser    = "single_user"
)

var ErrUnsupportedVersion = errors.New("runtime protocol version is unsupported")

const (
	MaxInputFiles         = 512
	MaxInputFilesBytes    = 8 * 1024 * 1024
	PublicEventLimit      = 512
	PublicEventBatchLimit = 16
)

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

type HostHeartbeatRequest struct {
	Versioned
	ActiveSlots int `json:"active_slots"`
}

type EngineCapability struct {
	SubjectIsolation    string `json:"subject_isolation,omitempty"`
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

type LeaseRequest struct {
	Versioned
	TaskID     string            `json:"task_id"`
	ClaimEpoch int64             `json:"claim_epoch"`
	Subject    execution.Subject `json:"subject"`
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

func ValidateInputFiles(files []InputFile) error {
	if len(files) > MaxInputFiles {
		return errors.New("too many upstream files")
	}
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		if file.TaskID == "" || file.NodeID == "" || path.Base(file.NodeID) != file.NodeID || file.NodeID == "." || file.NodeID == ".." || strings.Contains(file.NodeID, "\\") || !strings.HasPrefix(file.Path, file.NodeID+"/") {
			return errors.New("upstream file source identity is invalid")
		}
		if err := engine.ValidateArtifacts([]engine.Artifact{{Path: file.Path, ContentType: file.ContentType, Content: file.Content}}); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(file.Content))
		if hex.EncodeToString(digest[:]) != file.SHA256 || seen[file.Path] {
			return errors.New("upstream file hash or path conflicts")
		}
		seen[file.Path] = true
		total += len(file.Content)
		if total > MaxInputFilesBytes {
			return errors.New("upstream files exceed size bound")
		}
	}
	return nil
}

type TaskMCPTarget struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type PublicEvent struct {
	Seq        int64        `json:"seq"`
	OccurredAt time.Time    `json:"occurred_at"`
	Event      engine.Event `json:"event"`
	Truncated  bool         `json:"truncated,omitempty"`
}

type PublicEventsRequest struct {
	Versioned
	Events []PublicEvent `json:"events"`
}

type PublicEventsResponse struct {
	Versioned
	AckSeq int64 `json:"ack_seq"`
}

func ValidatePublicEvent(event PublicEvent) error {
	if event.Seq < 1 || event.Seq > PublicEventLimit || event.OccurredAt.IsZero() {
		return errors.New("invalid public event identity")
	}
	switch event.Event.Kind {
	case "text", "tool_call", "tool_result":
		return engine.ValidateEvents([]engine.Event{event.Event})
	case "stream_end":
		if event.Event != (engine.Event{Kind: "stream_end"}) {
			return errors.New("invalid stream end")
		}
		return nil
	default:
		return errors.New("only public text and tool events are accepted")
	}
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
	ReceiptID    string            `json:"receipt_id,omitempty"`
	ResultDigest string            `json:"result_digest,omitempty"`
	Result       *ExecutionReceipt `json:"result,omitempty"`
	Versioned
	SchemaVersion int               `json:"schema_version"`
	TaskID        string            `json:"task_id"`
	ClaimEpoch    int64             `json:"claim_epoch"`
	Subject       execution.Subject `json:"subject"`
}

func (request LeaseRequest) ValidateFor(claim ExecutionClaim) error {
	if err := request.Versioned.Validate(); err != nil {
		return err
	}
	if request.TaskID != claim.TaskID || request.ClaimEpoch != claim.ClaimEpoch || request.Subject != claim.Subject {
		return errors.New("runtime lease request does not match its claim")
	}
	return nil
}

func (receipt StoppedReceipt) ValidateFor(claim ExecutionClaim) error {
	if err := receipt.Versioned.Validate(); err != nil {
		return err
	}
	if receipt.SchemaVersion != ReceiptSchemaV1 {
		return fmt.Errorf("%w: stopped receipt=%d", ErrUnsupportedVersion, receipt.SchemaVersion)
	}
	if receipt.TaskID != claim.TaskID || receipt.ClaimEpoch != claim.ClaimEpoch || receipt.Subject != claim.Subject {
		return errors.New("runtime stopped receipt does not match its claim")
	}
	if receipt.Result != nil {
		if err := receipt.Result.ValidateFor(claim); err != nil {
			return err
		}
		if receipt.ReceiptID != receipt.Result.Identity() || receipt.ResultDigest != receipt.Result.Digest() {
			return errors.New("runtime stopped receipt digest mismatch")
		}
	} else if receipt.ReceiptID != "" || receipt.ResultDigest != "" {
		return errors.New("runtime stopped receipt omitted evidence")
	}
	return nil
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

// Identity and Digest remain stable across shutdown and spool replay.
func (receipt ExecutionReceipt) Identity() string {
	return fmt.Sprintf("execution:%s:%d", receipt.TaskID, receipt.ClaimEpoch)
}
func (receipt ExecutionReceipt) Digest() string {
	body, err := json.Marshal(receipt)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(body))
}
