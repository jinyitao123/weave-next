// Package workflow persists workspace-scoped team workflows and their drafts.
package workflow

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	WorkflowStatusActive   = "active"
	WorkflowStatusArchived = "archived"

	VersionStatusDraft     = "draft"
	VersionStatusPublished = "published"
)

var (
	ErrNotFound                     = errors.New("workflow not found")
	ErrVersionConflict              = errors.New("workflow version conflict")
	ErrDraftExists                  = errors.New("workflow draft already exists")
	ErrArchived                     = errors.New("workflow is archived")
	ErrNotPublished                 = errors.New("workflow is not published")
	ErrNotPureDraft                 = errors.New("workflow is not a pure draft")
	ErrAdmissionIdempotencyConflict = errors.New(
		"admission idempotency conflict",
	)
	ErrWorkflowScheduleAdmissionDenied = errors.New(
		"workflow schedule admission denied",
	)
)

type FixedWorkflowAdmissionDenialCode string

const (
	FixedWorkflowAdmissionTeamWorkerDisabled FixedWorkflowAdmissionDenialCode = "team_worker_disabled"
	FixedWorkflowAdmissionVersionBlocked     FixedWorkflowAdmissionDenialCode = "workflow_version_blocked"
)

// FixedWorkflowAdmissionDenial is the stable control-plane denial returned by
// the shared fixed-workflow gate. It deliberately excludes mutable details.
type FixedWorkflowAdmissionDenial struct {
	ReasonCode      FixedWorkflowAdmissionDenialCode
	WorkflowVersion int
}

func (e *FixedWorkflowAdmissionDenial) Error() string {
	if e == nil {
		return ""
	}
	return string(e.ReasonCode)
}

func (e *FixedWorkflowAdmissionDenial) Code() string {
	if e == nil {
		return ""
	}
	return string(e.ReasonCode)
}

func (e *FixedWorkflowAdmissionDenial) Unwrap() error {
	return ErrWorkflowScheduleAdmissionDenied
}

type FixedWorkflowAdmissionRequest struct {
	WorkspaceID     string
	TeamID          string
	WorkflowID      string
	WorkflowVersion int
	GraphDefinition json.RawMessage
}

type FixedWorkflowAdmissionDenialAttempt struct {
	WorkspaceID         string
	WorkflowID          string
	WorkflowVersion     int
	TriggerType         string
	AdmissionAttemptKey string
	ReasonCode          FixedWorkflowAdmissionDenialCode
}

type FixedWorkflowAdmissionDenialRecord struct {
	WorkspaceID         string                           `json:"workspace_id"`
	WorkflowID          string                           `json:"workflow_id"`
	WorkflowVersion     int                              `json:"workflow_version"`
	TriggerType         string                           `json:"trigger_type"`
	AdmissionAttemptKey string                           `json:"admission_attempt_key"`
	ReasonCode          FixedWorkflowAdmissionDenialCode `json:"reason_code"`
	DecidedAt           time.Time                        `json:"decided_at"`
}

// WorkflowScheduleAdmissionRequest identifies one schedule occurrence whose
// exact published workflow facts must be admitted in a caller-owned transaction.
type WorkflowScheduleAdmissionRequest struct {
	WorkspaceID   string
	ScheduleID    string
	WorkflowID    string
	OccurrenceKey string
	ScheduledFor  time.Time
}

// WorkflowManualRunAdmissionRequest identifies one operator-triggered run
// whose exact published workflow facts must be admitted transactionally.
type WorkflowManualRunAdmissionRequest struct {
	WorkspaceID string
	WorkflowID  string
	SourceRef   string
	TriggerType string
}

// Clock supplies timestamps for workflow mutations.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// TeamWorkflow is the mutable identity and lifecycle record for a team graph.
type TeamWorkflow struct {
	WorkspaceID      string    `json:"workspace_id"`
	ID               string    `json:"id"`
	TeamID           string    `json:"team_id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	PublishedVersion *int      `json:"published_version,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TeamWorkflowVersion contains the versioned trigger and graph definition.
type TeamWorkflowVersion struct {
	WorkspaceID     string          `json:"workspace_id"`
	WorkflowID      string          `json:"workflow_id"`
	Version         int             `json:"version"`
	Status          string          `json:"status"`
	TriggerConfig   json.RawMessage `json:"trigger_config"`
	GraphDefinition json.RawMessage `json:"graph_definition"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	PublishedAt     *time.Time      `json:"published_at,omitempty"`
}

// TeamWorkflowDependency is the immutable impact-analysis index for one
// published workflow dependency.
type TeamWorkflowDependency struct {
	WorkspaceID       string `json:"workspace_id"`
	WorkflowID        string `json:"workflow_id"`
	WorkflowVersion   int    `json:"workflow_version"`
	OwnerType         string `json:"owner_type"`
	OwnerID           string `json:"owner_id"`
	OwnerAgentVersion *int64 `json:"owner_agent_version,omitempty"`
	DependencyType    string `json:"dependency_type"`
	DependencyKey     string `json:"dependency_key"`
	DependencyVersion *int64 `json:"dependency_version,omitempty"`
	ContentHash       string `json:"content_hash"`
}

// PublishedArtifactContent is the immutable content fact for one published
// workflow version. Payload is its sole content source.
type PublishedArtifactContent struct {
	WorkspaceID               string          `json:"workspace_id"`
	WorkflowID                string          `json:"workflow_id"`
	WorkflowVersion           int             `json:"workflow_version"`
	ArtifactSchemaVersion     int             `json:"artifact_schema_version"`
	CanonicalizationAlgorithm string          `json:"canonicalization_algorithm"`
	CanonicalizationVersion   int             `json:"canonicalization_version"`
	HashAlgorithm             string          `json:"hash_algorithm"`
	ContentHash               string          `json:"content_hash"`
	Payload                   json.RawMessage `json:"payload"`
	CreatedAt                 time.Time       `json:"created_at"`
}

// Publication is the complete set of facts inserted by one publication
// transaction. Its top-level identity is authoritative for all nested rows.
type Publication struct {
	WorkspaceID       string                   `json:"workspace_id"`
	WorkflowID        string                   `json:"workflow_id"`
	WorkflowVersion   int                      `json:"workflow_version"`
	ExpectedUpdatedAt time.Time                `json:"expected_updated_at"`
	Dependencies      []TeamWorkflowDependency `json:"dependencies"`
	Artifact          PublishedArtifactContent `json:"artifact"`
}

// AdmissionChange is one idempotent command to set a published version's
// admission kill switch.
type AdmissionChange struct {
	WorkspaceID     string `json:"workspace_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	IdempotencyKey  string `json:"idempotency_key"`
	DesiredBlocked  bool   `json:"desired_blocked"`
	OperatorID      string `json:"operator_id"`
	Reason          string `json:"reason"`
}

// AdmissionResult is the frozen schema-one response stored in an idempotency
// receipt.
type AdmissionResult struct {
	SchemaVersion int     `json:"schema_version"`
	OldBlocked    bool    `json:"old_blocked"`
	NewBlocked    bool    `json:"new_blocked"`
	Changed       bool    `json:"changed"`
	AuditID       *string `json:"audit_id"`
}

// AdmissionAudit is one append-only blocked-state transition.
type AdmissionAudit struct {
	WorkspaceID     string    `json:"workspace_id"`
	WorkflowID      string    `json:"workflow_id"`
	WorkflowVersion int       `json:"workflow_version"`
	AuditID         string    `json:"audit_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	OldBlocked      bool      `json:"old_blocked"`
	NewBlocked      bool      `json:"new_blocked"`
	OperatorID      string    `json:"operator_id"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"created_at"`
}

// DraftInput is the complete mutable content of one workflow draft. CreatedBy
// is used only when a new workflow and its initial draft are created.
type DraftInput struct {
	TriggerConfig   json.RawMessage `json:"trigger_config"`
	GraphDefinition json.RawMessage `json:"graph_definition"`
	CreatedBy       string          `json:"created_by,omitempty"`
}
