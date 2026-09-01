package taskqueue

import (
	"encoding/json"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
)

const (
	StatusQueued          = "queued"
	StatusDispatched      = "dispatched"
	StatusRunning         = "running"
	StatusCancelRequested = "cancel_requested"
	StatusCompleted       = "completed"
	StatusFailed          = "failed"
	StatusCancelled       = "cancelled"
	StatusSuperseded      = "superseded"
	StatusCut             = "cut"
	StatusTimedOut        = "timed_out"
)

// IdentityKind selects the durable execution stamp represented by a task.
type IdentityKind string

const (
	IdentityAgent        IdentityKind = "agent"
	IdentityTeamWorkflow IdentityKind = "team_workflow"
	IdentityAudit        IdentityKind = "audit"
)

// ExecutionScope is the shared durable Agent execution scope.
type ExecutionScope = execution.Scope

// Task is one durable unit of work in the task queue.
type Task struct {
	ID                    string          `json:"id"`
	WorkspaceID           string          `json:"workspace_id"`
	ProjectID             string          `json:"project_id,omitempty"`
	Agent                 string          `json:"agent"`
	AgentID               string          `json:"agent_id,omitempty"`
	AgentVersion          int             `json:"agent_version,omitempty"`
	IdentityKind          IdentityKind    `json:"identity_kind"`
	IdentitySchemaVersion int             `json:"identity_schema_version"`
	ExecutionScope        ExecutionScope  `json:"execution_scope,omitempty"`
	WorkflowID            string          `json:"workflow_id,omitempty"`
	WorkflowVersion       int             `json:"workflow_version,omitempty"`
	RunSnapshotID         string          `json:"run_snapshot_id,omitempty"`
	Source                string          `json:"source"`
	Kind                  string          `json:"kind"`
	RuntimeID             string          `json:"runtime_id,omitempty"`
	RuntimeAssignment     json.RawMessage `json:"runtime_assignment,omitempty"`
	Status                string          `json:"status"`
	Priority              int             `json:"priority"`
	ContextKey            string          `json:"context_key,omitempty"`
	TraceID               string          `json:"trace_id,omitempty"`
	ParentTaskID          string          `json:"parent_task_id,omitempty"`
	TaskGroupID           string          `json:"task_group_id,omitempty"`
	SubtaskDeadlineAt     *time.Time      `json:"subtask_deadline_at,omitempty"`
	Payload               json.RawMessage `json:"payload"`
	Result                json.RawMessage `json:"result,omitempty"`
	Error                 string          `json:"error,omitempty"`
	RunID                 string          `json:"run_id,omitempty"`
	WorkerID              string          `json:"worker_id,omitempty"`
	LeaseExpiresAt        *time.Time      `json:"lease_expires_at,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
	StartedAt             *time.Time      `json:"started_at,omitempty"`
	CompletedAt           *time.Time      `json:"completed_at,omitempty"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

// ClaimFilter selects the queue partition a worker is allowed to claim.
type ClaimFilter struct {
	Kind          string
	WorkspaceID   string
	RuntimeID     string
	RunSnapshotID string
	IdentityKind  IdentityKind
}

// IsTerminal reports whether the task will no longer be claimed.
func (t Task) IsTerminal() bool {
	switch t.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusSuperseded, StatusCut, StatusTimedOut:
		return true
	default:
		return false
	}
}

// TaskListResponse preserves the existing /v1/jobs response envelope.
type TaskListResponse struct {
	Jobs  []Task `json:"jobs"`
	Total int    `json:"total"`
}

// EngineExecObservation is the bounded, durable result carrier for a completed
// runtime-side engine task tied to one exact workflow member.
type EngineExecObservation struct {
	TaskID string
	Result json.RawMessage
}
