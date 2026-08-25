package fanout

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ErrorCode string

const (
	ErrorInvalidRequest         ErrorCode = "fanout_invalid_request"
	ErrorBranchShapeUnsupported ErrorCode = "fanout_branch_shape_unsupported"
	ErrorGenerationMismatch     ErrorCode = "fanout_generation_mismatch"
	ErrorIntentConflict         ErrorCode = "fanout_intent_conflict"
	ErrorIntentNotRevivable     ErrorCode = "fanout_intent_not_revivable"
	ErrorGroupAlreadyDecided    ErrorCode = "fanout_group_already_decided"
	ErrorLegTerminal            ErrorCode = "fanout_leg_terminal"
	ErrorJoinUnsatisfied        ErrorCode = "fanout_join_unsatisfied"
	ErrorResumeConflict         ErrorCode = "fanout_resume_conflict"
	ErrorUnknownLegStatus       ErrorCode = "fanout_unknown_leg_status"
	ErrorStoreUnavailable       ErrorCode = "fanout_store_unavailable"
)

type WorkflowError struct {
	Code  ErrorCode
	Cause error
}

func (e *WorkflowError) Error() string {
	if e == nil {
		return "fanout error"
	}
	if e.Cause == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Cause.Error()
}

func (e *WorkflowError) Unwrap() error { return e.Cause }

func ErrorCodeOf(err error) ErrorCode {
	var target *WorkflowError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func workflowError(code ErrorCode, format string, args ...any) error {
	return &WorkflowError{Code: code, Cause: fmt.Errorf(format, args...)}
}

type IntentStatus string

const (
	IntentPending IntentStatus = "pending"
	IntentActive  IntentStatus = "active"
	IntentVoided  IntentStatus = "voided"
)

type WorkflowGroupStatus string

const (
	WorkflowGroupPendingActivation WorkflowGroupStatus = "pending_activation"
	WorkflowGroupActive            WorkflowGroupStatus = "active"
	WorkflowGroupDecided           WorkflowGroupStatus = "decided"
	WorkflowGroupResumed           WorkflowGroupStatus = "resumed"
	WorkflowGroupClosed            WorkflowGroupStatus = "closed"
)

type WorkflowGroupMode string

const (
	WorkflowResumeMode      WorkflowGroupMode = "workflow_resume"
	FreeCollabSynthesisMode WorkflowGroupMode = "free_collab_synthesis"
)

type JoinPolicyKind string

const (
	JoinAllSuccess JoinPolicyKind = "all_success"
	JoinQuorum     JoinPolicyKind = "quorum"
	JoinDeadline   JoinPolicyKind = "deadline"
	JoinFailFast   JoinPolicyKind = "fail_fast"
)

type JoinPolicy struct {
	Kind               JoinPolicyKind `json:"kind"`
	Quorum             int            `json:"quorum,omitempty"`
	DeadlineAt         time.Time      `json:"deadline_at"`
	MaxDeadlineSeconds int64          `json:"max_deadline_seconds,omitempty"`
}

type LegTerminal string

const (
	LegSucceeded LegTerminal = "succeeded"
	LegFailed    LegTerminal = "failed"
	LegTimeout   LegTerminal = "timeout"
	LegCut       LegTerminal = "cut"
	LegCancelled LegTerminal = "cancelled"
	LegAbandoned LegTerminal = "abandoned"
)

type LegDecisionState string

const (
	LegDecisionQueued          LegDecisionState = "queued"
	LegDecisionRunning         LegDecisionState = "running"
	LegDecisionCancelRequested LegDecisionState = "cancel_requested"
	LegDecisionSucceeded       LegDecisionState = "succeeded"
	LegDecisionFailed          LegDecisionState = "failed"
	LegDecisionTimeout         LegDecisionState = "timeout"
	LegDecisionCut             LegDecisionState = "cut"
	LegDecisionCancelled       LegDecisionState = "cancelled"
	LegDecisionAbandoned       LegDecisionState = "abandoned"
)

type PlannedLeg struct {
	LegID           string          `json:"leg_id"`
	BranchID        string          `json:"branch_id"`
	BranchOrdinal   int             `json:"branch_ordinal"`
	FrozenBundleRef json.RawMessage `json:"frozen_bundle_ref"`
	InputRef        json.RawMessage `json:"input_ref"`
	MayYieldProof   json.RawMessage `json:"may_yield_proof"`
}

type PrepareParkRequest struct {
	WorkspaceID                string
	ParentRunID                string
	WorkflowID                 string
	WorkflowVersion            int64
	RunSnapshotID              string
	NodeID                     string
	PreviousCheckpointSequence int64
	NodeEntryOrdinal           int64
	CreatorEpoch               int64
	CreatorAttemptGeneration   int64
	CreatorAttemptID           string
	ActivationDeadline         time.Time
	ResumeToken                string
	JoinPolicy                 JoinPolicy
	Legs                       []PlannedLeg
}

type FanoutWaitPayload struct {
	WaitType    string `json:"wait_type"`
	Parked      bool   `json:"parked"`
	IntentID    string `json:"intent_id"`
	GroupID     string `json:"group_id"`
	Generation  string `json:"generation"`
	ResumeToken string `json:"resume_token"`
	ParentRunID string `json:"parent_run_id"`
	JoinNodeID  string `json:"join_node_id"`
}

type YieldedCheckpoint struct {
	WorkspaceID string
	ParentRunID string
	Sequence    int64
	Payload     FanoutWaitPayload
}

type ParkIntent struct {
	WorkspaceID                string
	IntentID                   string
	GroupID                    string
	ParentRunID                string
	WorkflowID                 string
	WorkflowVersion            int64
	RunSnapshotID              string
	NodeID                     string
	PreviousCheckpointSequence int64
	NodeEntryOrdinal           int64
	CreatorEpoch               int64
	CreatorAttemptGeneration   int64
	CreatorAttemptID           string
	Generation                 string
	PlanHash                   string
	Status                     IntentStatus
	CheckpointSequence         *int64
	ActivationDeadline         time.Time
	ActivatedAt                *time.Time
	VoidedAt                   *time.Time
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	Reused                     bool
}

type ActivationStatus string

const (
	ActivationActivated     ActivationStatus = "activated"
	ActivationAlreadyActive ActivationStatus = "already_active"
	ActivationRevived       ActivationStatus = "revived"
)

type ActivationResult struct {
	IntentID string
	GroupID  string
	Status   ActivationStatus
	Reused   bool
}

type GroupLegSnapshot struct {
	GroupID       string
	LegID         string
	BranchID      string
	BranchOrdinal int
	Generation    string
	Status        LegDecisionState
	Result        json.RawMessage
	ErrorCode     string
	CompletedAt   *time.Time
}

type JoinDecision string

const (
	JoinSucceeded JoinDecision = "succeeded"
	JoinFailed    JoinDecision = "failed"
)

type JoinResultLegV1 struct {
	LegID         string           `json:"leg_id"`
	BranchID      string           `json:"branch_id"`
	DecisionState LegDecisionState `json:"decision_state"`
	Terminal      *LegTerminal     `json:"terminal"`
	Result        json.RawMessage  `json:"result"`
	ErrorCode     *string          `json:"error_code"`
	TerminalAt    *time.Time       `json:"terminal_at"`
}

type JoinResultV1 struct {
	SchemaVersion     int               `json:"schema_version"`
	GroupID           string            `json:"group_id"`
	GroupCompletionID string            `json:"group_completion_id"`
	Generation        string            `json:"generation"`
	Decision          JoinDecision      `json:"decision"`
	Diagnostic        string            `json:"diagnostic,omitempty"`
	DecidedAt         time.Time         `json:"decided_at"`
	Policy            JoinPolicy        `json:"policy"`
	Legs              []JoinResultLegV1 `json:"legs"`
}

type JoinActions struct {
	CutLegIDs           []string
	RequestCancelLegIDs []string
}

type JoinEvaluation struct {
	Ready      bool
	Decision   JoinDecision
	Diagnostic string
	Frozen     JoinResultV1
	Actions    JoinActions
}

type LegCompletionRequest struct {
	WorkspaceID string
	GroupID     string
	LegID       string
	Generation  string
	Terminal    LegTerminal
	Result      json.RawMessage
	ErrorCode   string
	CompletedAt time.Time
}

type LegCompletionDisposition string

const (
	LegCompletionApplied       LegCompletionDisposition = "applied"
	LegCompletionLateAuditOnly LegCompletionDisposition = "late_audit_only"
)

type LegCompletionResult struct {
	Disposition LegCompletionDisposition
}

type AuditEvent struct {
	AuditID     string
	WorkspaceID string
	IntentID    string
	GroupID     string
	LegID       string
	Generation  string
	EventType   string
	Payload     json.RawMessage
	OccurredAt  time.Time
}

type ResumeClaimState string

const (
	ResumeClaimClaimed  ResumeClaimState = "claimed"
	ResumeClaimAdmitted ResumeClaimState = "admitted"
	ResumeClaimAdvanced ResumeClaimState = "advanced"
)

type WorkflowGroup struct {
	WorkspaceID       string
	GroupID           string
	IntentID          string
	Mode              WorkflowGroupMode
	Status            WorkflowGroupStatus
	Policy            JoinPolicy
	Generation        string
	GroupCompletionID string
	Decision          JoinDecision
	JoinResult        json.RawMessage
	DecidedAt         *time.Time
	ResumeClaimID     string
	ResumeClaimState  *ResumeClaimState
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type WorkflowLegPlan struct {
	WorkspaceID     string
	GroupID         string
	LegID           string
	BranchID        string
	BranchOrdinal   int
	Generation      string
	FrozenBundleRef json.RawMessage
	InputRef        json.RawMessage
	MayYieldProof   json.RawMessage
}

type WorkflowResumePlan struct {
	WorkspaceID               string
	IntentID                  string
	GroupID                   string
	Mode                      WorkflowGroupMode
	Status                    WorkflowGroupStatus
	ParentRunID               string
	WorkflowID                string
	WorkflowVersion           int64
	RunSnapshotID             string
	NodeID                    string
	Generation                string
	CheckpointSequence        *int64
	ResumeTokenHash           []byte
	CreatorEpoch              int64
	CreatorAttemptGeneration  int64
	CreatorAttemptID          string
	GroupCompletionID         string
	JoinResult                json.RawMessage
	ResumeClaimID             string
	ResumeClaimState          *ResumeClaimState
	PreviousAttemptGeneration int64
	PreviousAttemptID         string
	NewAttemptGeneration      *int64
	NewAttemptID              string
	ResumeReceiptID           string
	ResumeReceipt             json.RawMessage
}

type ResumeClaim struct {
	WorkspaceID               string
	GroupID                   string
	GroupCompletionID         string
	ClaimID                   string
	State                     ResumeClaimState
	PreviousAttemptGeneration int64
	PreviousAttemptID         string
	NewAttemptGeneration      *int64
	NewAttemptID              string
	ClaimedAt                 time.Time
	ResumeReceiptID           string
	ResumeReceipt             json.RawMessage
	AdvancedAt                *time.Time
}

type ClaimResumeRequest struct {
	WorkspaceID               string
	GroupID                   string
	GroupCompletionID         string
	ClaimID                   string
	PreviousAttemptGeneration int64
	PreviousAttemptID         string
	ClaimedAt                 time.Time
}

type AdmitResumeRequest struct {
	WorkspaceID               string
	GroupID                   string
	ClaimID                   string
	PreviousAttemptGeneration int64
	PreviousAttemptID         string
	NewAttemptGeneration      int64
	NewAttemptID              string
}

type AdvanceResumeRequest struct {
	WorkspaceID          string
	GroupID              string
	ClaimID              string
	NewAttemptGeneration int64
	NewAttemptID         string
	ResumeReceiptID      string
	ResumeReceipt        json.RawMessage
	AdvancedAt           time.Time
}

type ActivateParkRequest struct {
	WorkspaceID        string
	IntentID           string
	ParentRunID        string
	CheckpointSequence int64
	Generation         string
	ResumeToken        string
}

type ResumeParkedRunRequest struct {
	WorkspaceID               string
	ParentRunID               string
	RunSnapshotID             string
	IntentID                  string
	GroupID                   string
	Generation                string
	ResumeToken               string
	GroupCompletionID         string
	ClaimID                   string
	ExpectedAttemptGeneration int64
	ExpectedAttemptID         string
	NewAttemptGeneration      int64
	NewAttemptID              string
	JoinResult                json.RawMessage
}

type ResumeStatus string

const (
	ResumeAdvanced        ResumeStatus = "advanced"
	ResumeAlreadyAdvanced ResumeStatus = "already_advanced"
	ResumeClaimConflict   ResumeStatus = "claim_conflict"
)

type ResumeResult struct {
	Status            ResumeStatus
	ClaimID           string
	ResumeReceiptID   string
	AttemptGeneration int64
	AttemptID         string
	ResumeReceipt     json.RawMessage
}

type CreatorLeaseIdentity struct {
	WorkspaceID       string
	RunID             string
	CreatorEpoch      int64
	AttemptGeneration int64
	AttemptID         string
}

type CreatorLeaseState struct {
	WorkspaceID       string
	RunID             string
	AttemptGeneration int64
	AttemptID         string
	State             string
	LeaseExpiresAt    time.Time
	MarkerPhase       string
	MarkerGeneration  int64
	MarkerAttemptID   string
}

type GroupCompletion struct {
	WorkspaceID       string
	GroupID           string
	GroupCompletionID string
	Mode              string
	Generation        string
	JoinResult        json.RawMessage
	DecidedAt         time.Time
}

type LateSynthesisResult struct {
	Status         string
	CompletionID   string
	SessionEventID string
	ChildRunID     string
}

type JoinProjectionV1 struct {
	SchemaVersion int                        `json:"schema_version"`
	Decision      JoinDecision               `json:"decision"`
	Results       map[string]json.RawMessage `json:"results"`
	Errors        map[string]string          `json:"errors"`
}
