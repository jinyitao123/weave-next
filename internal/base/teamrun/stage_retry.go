package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type StageRetryRequest struct {
	IdempotencyKey string
	WorkspaceID    string
	RunID          string
	NodeID         string
}

type StageRetryResult struct {
	Replayed                 bool `json:"replayed,omitempty"`
	failureTaskID            string
	failureTaskUpdatedAt     time.Time
	RunID                    string   `json:"run_id"`
	NodeID                   string   `json:"node_id"`
	Status                   string   `json:"status"`
	AffectedNodeIDs          []string `json:"affected_node_ids"`
	PreservedCompletedStages bool     `json:"preserved_completed_stages"`
}

type failedTaskRequeuer interface {
	Get(context.Context, string, string) (*taskqueue.Task, error)
	GetTx(context.Context, pgx.Tx, string, string) (*taskqueue.Task, error)
	RequeueFailedTaskTx(context.Context, pgx.Tx, string, string, string, string) (*taskqueue.Task, error)
	EnqueueTx(context.Context, pgx.Tx, *taskqueue.Task) error
}

type StageRetryService struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Checkpoints  *PGCheckpointStore
	Tasks        failedTaskRequeuer
	Now          func() time.Time
}

type RuntimeWaitDetailV1 struct {
	SchemaVersion   int    `json:"schema_version"`
	WaitType        string `json:"wait_type"`
	NodeID          string `json:"node_id"`
	RecoveryBlocked bool   `json:"recovery_blocked,omitempty"`
}

type RuntimeRetryTaskPayloadV1 struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	RunID          string `json:"run_id"`
	NodeID         string `json:"node_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Retry binds a user command to one durable result. Replaying that command
// cannot consume a later failure even if the previous response was lost.
func (s *StageRetryService) Retry(ctx context.Context, request StageRetryRequest) (StageRetryResult, error) {
	if s == nil || s.Runs == nil || s.Tasks == nil || request.WorkspaceID == "" || request.RunID == "" || request.NodeID == "" {
		return StageRetryResult{}, errors.New("stage retry dependencies and identity are required")
	}
	if len(request.IdempotencyKey) > 256 || request.IdempotencyKey != strings.TrimSpace(request.IdempotencyKey) {
		return StageRetryResult{}, ErrTeamRunResumeInvalid
	}
	transactions := s.Transactions
	if transactions == nil {
		transactions = s.Runs.Transactions
	}
	if transactions == nil {
		return StageRetryResult{}, errors.New("stage retry transactions are unavailable")
	}
	tx, err := transactions.Begin(ctx)
	if err != nil {
		return StageRetryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := s.Runs.GetForUpdateTx(ctx, tx, request.WorkspaceID, request.RunID)
	if err != nil {
		return StageRetryResult{}, err
	}
	if result, found, err := replayStageRetryReceiptTx(ctx, tx, request); err != nil || found {
		return result, err
	}
	var result StageRetryResult
	switch {
	case run.Status == StatusParked && run.WaitKind != nil && *run.WaitKind == WaitRuntime:
		result, err = s.retryRuntimeStageTx(ctx, tx, run, request)
	case run.Status == StatusParked && run.WaitKind != nil && *run.WaitKind == WaitFanout:
		result, err = s.retryFanoutStageTx(ctx, tx, run, request)
	default:
		result, err = s.replayRuntimeRetryTx(ctx, tx, run, request)
	}
	if err != nil {
		return StageRetryResult{}, err
	}
	if err := recordStageRetryReceiptTx(ctx, tx, run, request, result); err != nil {
		return StageRetryResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StageRetryResult{}, err
	}
	return result, nil
}

func (s *StageRetryService) retryFanoutStageTx(ctx context.Context, tx pgx.Tx, run TeamRun, request StageRetryRequest) (StageRetryResult, error) {
	if err := requireRuntimeFailuresStoppedTx(ctx, tx, run); err != nil {
		return StageRetryResult{}, err
	}
	var wait fanout.FanoutWaitPayload
	if err := decodeFanoutExact(run.WaitDetail, &wait); err != nil || wait.GroupID == "" || wait.Generation == "" || wait.ParentRunID != run.RunID {
		return StageRetryResult{}, fmt.Errorf("%w: fanout wait identity is invalid", ErrTeamRunSnapshotUnavailable)
	}
	taskID, err := fanout.DeriveLegTaskID(wait.GroupID, request.NodeID, wait.Generation)
	if err != nil {
		return StageRetryResult{}, err
	}
	task, err := s.Tasks.GetTx(ctx, tx, request.WorkspaceID, taskID)
	if err != nil {
		return StageRetryResult{}, err
	}
	if task.RunSnapshotID != run.RunSnapshotID || task.ContextKey != wait.GroupID {
		return StageRetryResult{}, fmt.Errorf("%w: selected stage is not a failed branch of this run", ErrTeamRunStateConflict)
	}
	result := StageRetryResult{RunID: run.RunID, NodeID: request.NodeID,
		AffectedNodeIDs: []string{request.NodeID}, PreservedCompletedStages: true}
	// A response can be lost after the durable requeue. Repeating the same
	// exact-stage command is a successful no-op while that task is already
	// queued or executing; it must not turn recovery into a false conflict.
	if task.Status == taskqueue.StatusQueued || task.Status == taskqueue.StatusDispatched || task.Status == taskqueue.StatusRunning {
		result.Status = task.Status
		return result, nil
	}
	if task.Status != taskqueue.StatusFailed {
		return StageRetryResult{}, fmt.Errorf("%w: selected stage is not failed", ErrTeamRunStateConflict)
	}
	failure := ClassifyFailure(errors.New(task.Error))
	if !failure.Retryable || failure.Class != FailureClassInfrastructure {
		return StageRetryResult{}, fmt.Errorf("%w: selected stage did not fail for a retryable infrastructure reason", ErrTeamRunStateConflict)
	}
	if _, err := s.Tasks.RequeueFailedTaskTx(ctx, tx, request.WorkspaceID, taskID, run.RunSnapshotID, wait.GroupID); err != nil {
		return StageRetryResult{}, err
	}
	result.failureTaskID, result.failureTaskUpdatedAt = task.ID, task.UpdatedAt
	result.Status = taskqueue.StatusQueued
	return result, nil
}

func DecodeRuntimeWaitDetailV1(raw json.RawMessage) (RuntimeWaitDetailV1, error) {
	var detail RuntimeWaitDetailV1
	if err := decodeExact(raw, &detail); err != nil || detail.SchemaVersion != 1 ||
		detail.WaitType != "runtime" || detail.NodeID == "" {
		return RuntimeWaitDetailV1{}, fmt.Errorf("%w: runtime wait detail is invalid", ErrTeamRunResumeInvalid)
	}
	return detail, nil
}

func runtimeRetryKey(runID, nodeID string, generation ResumeGeneration) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("runtime-retry:%s:%s:%d", runID, nodeID, generation)))
	return "runtime-retry:" + hex.EncodeToString(digest[:16])
}

func runtimeRetryTaskID(workspaceID, runID, nodeID string, generation ResumeGeneration) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("runtime-retry-task:%s:%s:%s:%d", workspaceID, runID, nodeID, generation)))
	return "rrt1_" + hex.EncodeToString(digest[:16])
}

func runtimeRetryExecutorID(runID, nodeID string, generation ResumeGeneration) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("runtime-retry-executor:%s:%s:%d", runID, nodeID, generation)))
	return "teamrun-runtime-retry:" + hex.EncodeToString(digest[:16])
}

func (s *StageRetryService) replayRuntimeRetryTx(ctx context.Context, tx pgx.Tx, run TeamRun, request StageRetryRequest) (StageRetryResult, error) {
	if run.Status != StatusRunning && run.Status != StatusSucceeded {
		return StageRetryResult{}, ErrTeamRunStateConflict
	}
	if run.Status == StatusRunning && (run.CurrentExecutorID == nil ||
		*run.CurrentExecutorID != runtimeRetryExecutorID(run.RunID, request.NodeID, run.ResumeGeneration)) {
		return StageRetryResult{}, ErrTeamRunStateConflict
	}
	task, err := s.Tasks.GetTx(ctx, tx, request.WorkspaceID, runtimeRetryTaskID(request.WorkspaceID, request.RunID, request.NodeID, run.ResumeGeneration))
	if err != nil || task.RunSnapshotID != run.RunSnapshotID || task.ContextKey != run.RunID ||
		task.WorkflowID != run.WorkflowID || task.WorkflowVersion != run.WorkflowVersion ||
		(task.Status != taskqueue.StatusQueued && task.Status != taskqueue.StatusDispatched &&
			task.Status != taskqueue.StatusRunning && task.Status != taskqueue.StatusCompleted) {
		return StageRetryResult{}, fmt.Errorf("%w: run is not waiting on a recoverable team stage", ErrTeamRunStateConflict)
	}
	return StageRetryResult{RunID: request.RunID, NodeID: request.NodeID, Status: task.Status,
		AffectedNodeIDs: []string{request.NodeID}, PreservedCompletedStages: true}, nil
}

func (s *StageRetryService) retryRuntimeStageTx(ctx context.Context, tx pgx.Tx, run TeamRun, request StageRetryRequest) (StageRetryResult, error) {
	if s.Checkpoints == nil {
		return StageRetryResult{}, errors.New("runtime retry checkpoints unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if err := requireRuntimeFailuresStoppedTx(ctx, tx, run); err != nil {
		return StageRetryResult{}, err
	}
	detail, err := DecodeRuntimeWaitDetailV1(run.WaitDetail)
	if err != nil || detail.NodeID != request.NodeID {
		return StageRetryResult{}, fmt.Errorf("%w: selected stage differs from runtime wait", ErrTeamRunStateConflict)
	}
	if detail.RecoveryBlocked {
		return StageRetryResult{}, fmt.Errorf("%w: tool outcome requires reconciliation before continuing", ErrTeamRunResumeInvalid)
	}
	checkpoint, err := s.Checkpoints.GetTx(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil {
		return StageRetryResult{}, err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil || checkpoint.NodeID != request.NodeID ||
		checkpoint.TeamRunGeneration+1 != run.Generation || checkpoint.ExecutionLeaseEpoch != run.ExecutionLeaseEpoch {
		return StageRetryResult{}, fmt.Errorf("%w: runtime retry checkpoint differs", ErrTeamRunStateConflict)
	}
	key := runtimeRetryKey(run.RunID, request.NodeID, run.ResumeGeneration)
	executorID := runtimeRetryExecutorID(run.RunID, request.NodeID, run.ResumeGeneration)
	resumed, err := s.Runs.ResumeRunningTx(ctx, tx, ResumeRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedStatus: StatusParked,
		ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration: run.ResumeGeneration, ExpectedWaitKind: WaitRuntime,
		ExpectedResumeTokenHash: run.ResumeTokenHash, ExecutorID: executorID,
		IdempotencyKey: key, Actor: "stage-retry", Source: "runtime_retry", OccurredAt: now,
	})
	if err != nil {
		return StageRetryResult{}, err
	}
	checkpoint.TeamRunGeneration = resumed.Generation
	checkpoint.ExecutionLeaseEpoch = resumed.ExecutionLeaseEpoch
	checkpoint.WrittenAt = now
	if _, err := s.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
		return StageRetryResult{}, err
	}
	payload, err := json.Marshal(RuntimeRetryTaskPayloadV1{SchemaVersion: 1, Kind: "runtime_retry",
		RunID: run.RunID, NodeID: request.NodeID, IdempotencyKey: key})
	if err != nil {
		return StageRetryResult{}, err
	}
	taskID := runtimeRetryTaskID(run.WorkspaceID, run.RunID, request.NodeID, run.ResumeGeneration)
	if err := s.Tasks.EnqueueTx(ctx, tx, &taskqueue.Task{ID: taskID, WorkspaceID: resumed.WorkspaceID,
		ProjectID: resumed.ProjectID, IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: resumed.WorkflowID, WorkflowVersion: resumed.WorkflowVersion, RunSnapshotID: resumed.RunSnapshotID,
		Source: "runtime_retry", Kind: "team_workflow", ContextKey: resumed.RunID, Payload: payload}); err != nil {
		return StageRetryResult{}, err
	}
	return StageRetryResult{RunID: run.RunID, NodeID: request.NodeID, Status: taskqueue.StatusQueued,
		AffectedNodeIDs: []string{request.NodeID}, PreservedCompletedStages: true}, nil
}

// Lease expiry cannot prove process exit. Reconnection delivers the original
// worker's exit acknowledgement before a user retry may start another attempt.
func requireRuntimeFailuresStoppedTx(ctx context.Context, tx pgx.Tx, run TeamRun) error {
	stopped, err := taskqueue.RuntimeFailuresStoppedTx(ctx, tx, run.WorkspaceID, run.RunSnapshotID)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("%w: runtime execution stop is not yet acknowledged", ErrTeamRunStateConflict)
	}
	stopped, err = MembersStoppedTx(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("%w: member execution stop is not yet acknowledged", ErrTeamRunStateConflict)
	}
	return nil
}
