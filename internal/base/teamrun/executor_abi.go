package teamrun

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type ExecutorTaskStore interface {
	Claim(context.Context, string, taskqueue.ClaimFilter) (*taskqueue.Task, error)
	Get(context.Context, string, string) (*taskqueue.Task, error)
	Heartbeat(context.Context, string, string) error
	CompleteClaimed(context.Context, string, string, json.RawMessage, string) error
	FailClaimed(context.Context, string, string, string) error
}

type ExecutorRunStore interface {
	GetForUpdateTx(context.Context, pgx.Tx, string, string) (TeamRun, error)
	ClaimRunningTx(context.Context, pgx.Tx, ClaimRequest) (TeamRun, error)
	ParkTx(context.Context, pgx.Tx, ParkRequest) (TeamRun, error)
	ReclaimRunningTx(context.Context, pgx.Tx, ReclaimRequest) (TeamRun, error)
	AbandonUnrecoverableTx(context.Context, pgx.Tx, AbandonUnrecoverableRequest) (TeamRun, error)
	ResumeRunningTx(context.Context, pgx.Tx, ResumeRequest) (TeamRun, error)
	FailWaitTimeoutTx(context.Context, pgx.Tx, FailWaitTimeoutRequest) (TeamRun, error)
	SucceedTx(context.Context, pgx.Tx, SucceedRequest) (TeamRun, error)
	FailTx(context.Context, pgx.Tx, FailRequest) (TeamRun, error)
}

type ExecutorCheckpointStore interface {
	PutTx(context.Context, pgx.Tx, WorkflowCheckpointV1) (string, error)
	GetTx(context.Context, pgx.Tx, string, string) (WorkflowCheckpointV1, error)
}

type RuntimeResultStatus string

const (
	RuntimeCompleted RuntimeResultStatus = "completed"
	RuntimeParked    RuntimeResultStatus = "parked"
	RuntimeFailed    RuntimeResultStatus = "failed"
)

type RuntimePark struct {
	NodeID           string
	CompletedOutputs map[string]json.RawMessage
	WaitKind         WaitKind
	WaitDetail       json.RawMessage
	// UsageCheckpoint persists the serial machine's usage accumulator so a
	// park/resume cycle never loses or duplicates confirmed usage.
	UsageCheckpoint json.RawMessage
	// UsageComplete is false when the run reached a node whose usage cannot
	// be measured (candidate fanout legs / CLI node without a receipt);
	// UsageIncompleteReason names the unmeasured part.
	UsageComplete         bool
	UsageIncompleteReason string
}

type RuntimeResult struct {
	Status RuntimeResultStatus
	Output json.RawMessage
	Park   *RuntimePark
	// Usage is the accumulated confirmed logical usage of the serial machine.
	// It is set on every terminal outcome (completed, parked, failed) so a
	// failed run still charges its observed usage.
	Usage loomruntime.UsageTotals
	// UsageComplete is false when the run result only covers the measured
	// serial/loop contributions; UsageIncompleteReason names the unmeasured
	// part (see the UsageIncompleteReason* constants).
	UsageComplete         bool
	UsageIncompleteReason string
}

type RuntimeRunner interface {
	Execute(context.Context, TeamRun, *taskqueue.Task) (RuntimeResult, error)
	ResumeCheckpoint(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (RuntimeResult, error)
	TimerResumeTarget(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (string, bool, error)
}

type FanoutLegPlan struct {
	LegID           string
	BranchID        string
	BranchOrdinal   int
	FrozenBundleRef json.RawMessage
	InputRef        json.RawMessage
	MayYieldProof   json.RawMessage
}

type FanoutPrepareRequest struct {
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
	JoinPolicy                 json.RawMessage
	Legs                       []FanoutLegPlan
}

type FanoutParkIntent struct {
	IntentID   string
	GroupID    string
	Generation string
}

type FanoutActivationRequest struct {
	WorkspaceID        string
	IntentID           string
	ParentRunID        string
	CheckpointSequence int64
	Generation         string
	ResumeToken        string
}

type FanoutLegCompletion struct {
	WorkspaceID string
	GroupID     string
	LegID       string
	Generation  string
	Terminal    string
	Result      json.RawMessage
	ErrorCode   string
	CompletedAt time.Time
}

type ExecutorFanout interface {
	PreparePark(context.Context, pgx.Tx, FanoutPrepareRequest) (FanoutParkIntent, error)
	ActivatePark(context.Context, FanoutActivationRequest) error
	RecordLegCompletion(context.Context, FanoutLegCompletion) error
}

type FanoutLegRunner interface {
	ExecuteFanoutLeg(context.Context, *taskqueue.Task, FanoutLegTaskPayloadV1) (json.RawMessage, error)
}

type Executor struct {
	Tasks             ExecutorTaskStore
	Consumer          *Consumer
	Transactions      TransactionBeginner
	Runs              ExecutorRunStore
	Checkpoints       ExecutorCheckpointStore
	Runtime           RuntimeRunner
	Fanout            ExecutorFanout
	RuntimeRecords    loomruntime.TerminalRecordStore
	Now               func() time.Time
	ResumeTokenHash   func() ([]byte, error)
	HeartbeatInterval time.Duration
	// ClaimWorkspaceID and ClaimRunSnapshotID optionally restrict ProcessNext
	// to one admitted run. Ordinary daemon workers leave them empty and keep
	// claiming the shared queue; synchronous candidate drivers set both so a
	// healthy build cannot be hijacked by another conversation's task.
	ClaimWorkspaceID   string
	ClaimRunSnapshotID string
}

type FanoutLegTaskPayloadV1 struct {
	SchemaVersion   int             `json:"schema_version"`
	Kind            string          `json:"kind"`
	WorkspaceID     string          `json:"workspace_id"`
	ParentRunID     string          `json:"parent_run_id"`
	IntentID        string          `json:"intent_id"`
	GroupID         string          `json:"group_id"`
	LegID           string          `json:"leg_id"`
	BranchID        string          `json:"branch_id"`
	BranchOrdinal   int             `json:"branch_ordinal"`
	Generation      string          `json:"generation"`
	FrozenBundleRef json.RawMessage `json:"frozen_bundle_ref"`
	InputRef        json.RawMessage `json:"input_ref"`
	MayYieldProof   json.RawMessage `json:"may_yield_proof"`
}
