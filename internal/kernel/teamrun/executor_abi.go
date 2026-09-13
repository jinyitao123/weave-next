package teamrun

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
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
