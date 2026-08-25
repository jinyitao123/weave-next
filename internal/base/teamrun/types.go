package teamrun

import (
	"encoding/json"
	"fmt"
	"time"
)

type Status string

const (
	StatusQueued          Status = "queued"
	StatusRunning         Status = "running"
	StatusParked          Status = "parked"
	StatusCancelRequested Status = "cancel_requested"
	StatusSucceeded       Status = "succeeded"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusAbandoned       Status = "abandoned"
)

func (status Status) Terminal() bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusAbandoned:
		return true
	default:
		return false
	}
}

type TeamRunGeneration int64
type ExecutionLeaseEpoch int64
type ResumeGeneration int64
type SessionLeaseEpoch int64
type FanoutGeneration int64

type SourceKind string

const (
	SourceSession  SourceKind = "session"
	SourceSchedule SourceKind = "schedule"
	SourceManual   SourceKind = "manual"
	SourceAPI      SourceKind = "api"
	SourceEvent    SourceKind = "event"
)

type WaitKind string

const (
	WaitTimer  WaitKind = "timer"
	WaitFanout WaitKind = "fanout"
)

type TeamRun struct {
	WorkspaceID         string
	ProjectID           string
	RunID               string
	Status              Status
	Generation          TeamRunGeneration
	ExecutionLeaseEpoch ExecutionLeaseEpoch
	ResumeGeneration    ResumeGeneration

	TeamID                  string
	WorkflowID              string
	WorkflowVersion         int
	RunSnapshotID           string
	SourceKind              SourceKind
	SourceTaskID            string
	EstablishIdempotencyKey string

	CurrentExecutorID *string
	WaitKind          *WaitKind
	WaitDetail        json.RawMessage
	ResumeTokenHash   []byte
	CheckpointRef     *string

	CancelActor           *string
	CancelReason          *string
	CancelIdempotencyKey  *string
	CancelRequestedAt     *time.Time
	CancelGraceDeadlineAt *time.Time

	ErrorCode    *ErrorCode
	CauseSummary *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	TerminalAt   *time.Time
}

type Transition struct {
	WorkspaceID         string
	RunID               string
	Seq                 int64
	FromStatus          *Status
	ToStatus            Status
	Generation          TeamRunGeneration
	ExecutionLeaseEpoch ExecutionLeaseEpoch
	ResumeGeneration    ResumeGeneration
	Actor               string
	Source              string
	IdempotencyKey      string
	ErrorCode           *ErrorCode
	CauseSummary        *string
	OccurredAt          time.Time
	Orphaned            bool
}

type EstablishRequest struct {
	WorkspaceID             string
	ProjectID               string
	RunID                   string
	TeamID                  string
	WorkflowID              string
	WorkflowVersion         int
	RunSnapshotID           string
	SourceKind              SourceKind
	SourceTaskID            string
	EstablishIdempotencyKey string
	Actor                   string
	Source                  string
	OccurredAt              time.Time
}

type ClaimRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExecutorID                  string
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type CancelQueuedRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type ParkRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExecutorID                  string
	WaitKind                    WaitKind
	WaitDetail                  json.RawMessage
	ResumeTokenHash             []byte
	CheckpointRef               string
	SessionLeaseEpoch           *SessionLeaseEpoch
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type SucceedRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExecutorID                  string
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type FailRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExecutorID                  string
	ErrorCode                   ErrorCode
	Cause                       error
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type RequestCancelRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	CancelActor                 string
	CancelReason                string
	GraceDeadlineAt             time.Time
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type ConfirmCancelRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type AbandonCancelGraceRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type ReclaimRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExecutorID                  string
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type AbandonUnrecoverableRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	Cause                       error
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type ResumeRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExpectedWaitKind            WaitKind
	ExpectedResumeTokenHash     []byte
	ExpectedSessionLeaseEpoch   *SessionLeaseEpoch
	ExecutorID                  string
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

type FailWaitTimeoutRequest struct {
	WorkspaceID                 string
	RunID                       string
	ExpectedStatus              Status
	ExpectedTeamRunGeneration   TeamRunGeneration
	ExpectedExecutionLeaseEpoch ExecutionLeaseEpoch
	ExpectedResumeGeneration    ResumeGeneration
	ExpectedWaitKind            WaitKind
	ErrorCode                   ErrorCode
	Cause                       error
	IdempotencyKey              string
	Actor                       string
	Source                      string
	OccurredAt                  time.Time
}

func ValidateStatus(status Status) error {
	switch status {
	case StatusQueued, StatusRunning, StatusParked, StatusCancelRequested,
		StatusSucceeded, StatusFailed, StatusCancelled, StatusAbandoned:
		return nil
	default:
		return fmt.Errorf("status %q is invalid", status)
	}
}
