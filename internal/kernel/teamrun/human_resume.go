package teamrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

const (
	HumanResumePayloadMaxBytes = 64 * 1024
	HumanWaitDetailMaxBytes    = 16 * 1024
)

type HumanTaskDetail struct {
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
	AudienceRef  string `json:"audience_ref,omitempty"`
}

type HumanWaitDetailV1 struct {
	SchemaVersion int             `json:"schema_version"`
	WaitType      string          `json:"wait_type"`
	NodeID        string          `json:"node_id"`
	SuccessNodeID string          `json:"success_node_id"`
	TimeoutNodeID string          `json:"timeout_node_id,omitempty"`
	ResumeSchema  json.RawMessage `json:"resume_schema"`
	Task          HumanTaskDetail `json:"task"`
	DeadlineAt    *time.Time      `json:"deadline_at,omitempty"`
}

func DecodeHumanWaitDetailV1(raw json.RawMessage) (HumanWaitDetailV1, error) {
	var detail HumanWaitDetailV1
	if len(raw) == 0 || len(raw) > HumanWaitDetailMaxBytes {
		return detail, errors.New("human wait detail size is invalid")
	}
	if err := decodeExact(raw, &detail); err != nil {
		return HumanWaitDetailV1{}, fmt.Errorf("decode human wait detail: %w", err)
	}
	var schema map[string]json.RawMessage
	deadlineEncodingValid := true
	if detail.DeadlineAt != nil {
		_, offset := detail.DeadlineAt.Zone()
		deadlineEncodingValid = offset == 0 && detail.DeadlineAt.Nanosecond() == 0
	}
	if detail.SchemaVersion != 1 || detail.WaitType != "human" || detail.NodeID == "" ||
		detail.SuccessNodeID == "" || detail.Task.Title == "" || len(detail.Task.Title) > 256 ||
		detail.Task.Instructions == "" || len(detail.Task.Instructions) > 4*1024 || !deadlineEncodingValid ||
		json.Unmarshal(detail.ResumeSchema, &schema) != nil || schema == nil ||
		(detail.DeadlineAt != nil && (detail.DeadlineAt.IsZero() || detail.TimeoutNodeID == "")) ||
		(detail.DeadlineAt == nil && detail.TimeoutNodeID != "") {
		return HumanWaitDetailV1{}, errors.New("human wait detail fields are invalid")
	}
	return detail, nil
}

type HumanResumeTaskStore interface {
	EnqueueTx(context.Context, pgx.Tx, *taskqueue.Task) error
}

// HumanInteractionID identifies one parked question, including a later visit
// to the same workflow node. It remains stable while that wait is unchanged.
func HumanInteractionID(run TeamRun) string {
	if run.Status != StatusParked || run.WaitKind == nil || *run.WaitKind != WaitHuman {
		return ""
	}
	detail, err := DecodeHumanWaitDetailV1(run.WaitDetail)
	if err != nil {
		return ""
	}
	identity, _ := json.Marshal([]any{run.WorkspaceID, run.RunID, detail.NodeID, run.Generation, run.ResumeGeneration})
	digest := sha256.Sum256(identity)
	return "human_" + hex.EncodeToString(digest[:16])
}

type CompleteHumanWaitRequest struct {
	// InteractionID is optional for legacy tool callers. Product forms bind
	// their answer to the exact question returned by the inbox/detail API.
	InteractionID string
	// ValidatePayload runs against the locked current question after replay
	// and identity checks. The app supplies schema policy without an upward import.
	ValidatePayload func(schema, payload json.RawMessage) error
	WorkspaceID     string
	RunID           string
	Payload         json.RawMessage
	PayloadDigest   []byte
	IdempotencyKey  string
	Actor           string
	OccurredAt      time.Time
}

type CompleteHumanWaitResult struct {
	Run        TeamRun
	TaskID     string
	Idempotent bool
}

type HumanResumeService struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Checkpoints  *PGCheckpointStore
	Tasks        HumanResumeTaskStore
	Now          func() time.Time
}

func (s *HumanResumeService) Complete(
	ctx context.Context,
	req CompleteHumanWaitRequest,
) (CompleteHumanWaitResult, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil || s.Checkpoints == nil || s.Tasks == nil {
		return CompleteHumanWaitResult{}, errors.New("human resume service dependencies are unavailable")
	}
	if req.WorkspaceID == "" || req.RunID == "" || req.IdempotencyKey == "" || req.Actor == "" ||
		len(req.Payload) == 0 || len(req.Payload) > HumanResumePayloadMaxBytes || !json.Valid(req.Payload) ||
		len(req.PayloadDigest) != sha256.Size || len(req.InteractionID) > 256 {
		return CompleteHumanWaitResult{}, errors.New("human resume request is invalid")
	}
	digest := sha256.Sum256(req.Payload)
	if !bytes.Equal(digest[:], req.PayloadDigest) {
		return CompleteHumanWaitResult{}, errors.New("human resume payload digest differs")
	}
	now := req.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
		if s.Now != nil {
			now = s.Now().UTC()
		}
	}
	taskID := humanResumeTaskID(req.WorkspaceID, req.RunID, req.IdempotencyKey)
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return CompleteHumanWaitResult{}, fmt.Errorf("begin human resume: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.Runs.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.RunID)
	if err != nil {
		return CompleteHumanWaitResult{}, err
	}
	meta := commandMeta{
		workspaceID: req.WorkspaceID, runID: req.RunID, idempotencyKey: req.IdempotencyKey,
		payloadDigest: append([]byte(nil), req.PayloadDigest...), actor: req.Actor,
		source: "human_resume", occurredAt: now,
	}
	// A lost response can be retried after this run has reached another human
	// question. Replay the recorded command before inspecting the current wait.
	transition, present, readErr := readTransitionByKey(ctx, tx, req.WorkspaceID, req.RunID, req.IdempotencyKey)
	if readErr != nil {
		return CompleteHumanWaitResult{}, readErr
	}
	if present {
		if !sameTransition(transition, StatusParked, StatusRunning, meta, nil, nil) {
			return CompleteHumanWaitResult{}, ErrTeamRunStateConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CompleteHumanWaitResult{}, fmt.Errorf("commit human resume replay: %w", err)
		}
		return CompleteHumanWaitResult{Run: locked, TaskID: taskID, Idempotent: true}, nil
	}
	if locked.Status != StatusParked {
		return CompleteHumanWaitResult{}, ErrTeamRunStateConflict
	}
	if req.InteractionID != "" && req.InteractionID != HumanInteractionID(locked) {
		return CompleteHumanWaitResult{}, ErrTeamRunResumeStale
	}
	if locked.WaitKind == nil || *locked.WaitKind != WaitHuman || locked.CheckpointRef == nil ||
		*locked.CheckpointRef != CheckpointRef(locked.WorkspaceID, locked.RunID) {
		return CompleteHumanWaitResult{}, ErrTeamRunResumeInvalid
	}
	detail, err := DecodeHumanWaitDetailV1(locked.WaitDetail)
	if err != nil {
		return CompleteHumanWaitResult{}, fmt.Errorf("%w: %v", ErrTeamRunResumeInvalid, err)
	}
	if req.ValidatePayload != nil {
		if err := req.ValidatePayload(detail.ResumeSchema, req.Payload); err != nil {
			return CompleteHumanWaitResult{}, err
		}
	}
	checkpoint, err := s.Checkpoints.GetTx(ctx, tx, locked.WorkspaceID, locked.RunID)
	if err != nil {
		return CompleteHumanWaitResult{}, err
	}
	if err := ValidateCheckpointRun(checkpoint, locked); err != nil {
		return CompleteHumanWaitResult{}, err
	}
	if checkpoint.NodeID != detail.NodeID {
		return CompleteHumanWaitResult{}, fmt.Errorf("%w: human wait node differs from checkpoint", ErrTeamRunIdentityMismatch)
	}
	executorID := humanResumeExecutorID(req.RunID, req.IdempotencyKey)
	resumed, err := s.Runs.ResumeRunningTx(ctx, tx, ResumeRequest{
		WorkspaceID: locked.WorkspaceID, RunID: locked.RunID,
		ExpectedStatus: StatusParked, ExpectedTeamRunGeneration: locked.Generation,
		ExpectedExecutionLeaseEpoch: locked.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    locked.ResumeGeneration, ExpectedWaitKind: WaitHuman,
		ExpectedResumeTokenHash: locked.ResumeTokenHash, Payload: req.Payload,
		PayloadDigest: req.PayloadDigest, ExecutorID: executorID,
		IdempotencyKey: req.IdempotencyKey, Actor: req.Actor, Source: "human_resume", OccurredAt: now,
	})
	if err != nil {
		return CompleteHumanWaitResult{}, err
	}
	if checkpoint.CompletedOutputs == nil {
		checkpoint.CompletedOutputs = make(map[string]json.RawMessage)
	}
	checkpoint.CompletedOutputs[checkpoint.NodeID] = append(json.RawMessage(nil), req.Payload...)
	checkpoint.NodeID = detail.SuccessNodeID
	checkpoint.TeamRunGeneration = resumed.Generation
	checkpoint.ExecutionLeaseEpoch = resumed.ExecutionLeaseEpoch
	checkpoint.WrittenAt = now
	if _, err := s.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
		return CompleteHumanWaitResult{}, err
	}
	taskPayload, err := json.Marshal(HumanResumeTaskPayloadV1{
		SchemaVersion: 1, Kind: "human_resume", RunID: req.RunID,
		IdempotencyKey: req.IdempotencyKey, Disposition: "completed",
		PayloadDigest: hex.EncodeToString(req.PayloadDigest),
	})
	if err != nil {
		return CompleteHumanWaitResult{}, err
	}
	// The reviewer is the transition actor, while the continuation must retain
	// the immutable execution subject frozen in RunSnapshotID. Mask the request
	// subject only for task admission so taskqueue inherits from that snapshot.
	if err := s.Tasks.EnqueueTx(execution.WithoutAuthenticatedSubject(ctx), tx, &taskqueue.Task{
		ID: taskID, WorkspaceID: resumed.WorkspaceID, ProjectID: resumed.ProjectID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: resumed.WorkflowID, WorkflowVersion: resumed.WorkflowVersion,
		RunSnapshotID: resumed.RunSnapshotID, Source: "human_resume", Kind: "team_workflow",
		ContextKey: resumed.RunID, Payload: taskPayload,
	}); err != nil {
		return CompleteHumanWaitResult{}, fmt.Errorf("enqueue human resume: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteHumanWaitResult{}, fmt.Errorf("commit human resume: %w", err)
	}
	return CompleteHumanWaitResult{Run: resumed, TaskID: taskID}, nil
}

type HumanResumeTaskPayloadV1 struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	RunID          string `json:"run_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Disposition    string `json:"disposition"`
	PayloadDigest  string `json:"payload_digest,omitempty"`
}

func humanResumeTaskID(workspaceID, runID, idempotencyKey string) string {
	digest := sha256.Sum256([]byte("human-resume-task:" + workspaceID + ":" + runID + ":" + idempotencyKey))
	return "hrt1_" + hex.EncodeToString(digest[:16])
}

func humanResumeExecutorID(runID, idempotencyKey string) string {
	digest := sha256.Sum256([]byte("human-resume-executor:" + runID + ":" + idempotencyKey))
	return "teamrun-human-resume:" + hex.EncodeToString(digest[:16])
}

func (e *Executor) processHumanResume(ctx context.Context, task *taskqueue.Task, workerID string) error {
	var payload HumanResumeTaskPayloadV1
	if err := decodeExact(task.Payload, &payload); err != nil || payload.SchemaVersion != 1 ||
		payload.Kind != "human_resume" || payload.RunID == "" || payload.RunID != task.ContextKey ||
		payload.IdempotencyKey == "" || (payload.Disposition != "completed" && payload.Disposition != "timeout") {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	payloadDigest, err := hex.DecodeString(payload.PayloadDigest)
	if err != nil || (payload.Disposition == "completed" && len(payloadDigest) != sha256.Size) ||
		(payload.Disposition == "timeout" && len(payloadDigest) != 0) {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin human continuation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := e.Runs.GetForUpdateTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	executorID := humanResumeExecutorID(payload.RunID, payload.IdempotencyKey)
	if run.Status != StatusRunning || run.CurrentExecutorID == nil || *run.CurrentExecutorID != executorID ||
		run.WorkflowID != task.WorkflowID || run.WorkflowVersion != task.WorkflowVersion ||
		run.RunSnapshotID != task.RunSnapshotID {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	transition, present, err := readTransitionByKey(ctx, tx, task.WorkspaceID, payload.RunID, payload.IdempotencyKey)
	if err != nil {
		return err
	}
	if !present || transition.FromStatus == nil || *transition.FromStatus != StatusParked ||
		transition.ToStatus != StatusRunning || !bytes.Equal(transition.PayloadDigest, payloadDigest) {
		return e.Tasks.FailClaimed(ctx, task.ID, workerID, string(ErrorCodeIdentityMismatch))
	}
	checkpoint, err := e.Checkpoints.GetTx(ctx, tx, task.WorkspaceID, payload.RunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit human continuation read: %w", err)
	}
	result, runErr := e.executeWithHeartbeat(ctx, task, workerID, func(execCtx context.Context) (RuntimeResult, error) {
		return e.Runtime.ResumeCheckpoint(execCtx, run, task, checkpoint)
	})
	if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, errTaskLeaseLost)) {
		return runErr
	}
	if runErr != nil {
		failed, failErr := e.failRunning(
			ctx, run, task, executorID, runErr,
			result.Usage, result.UsageCoverage, result.UsageComplete, result.UsageIncompleteReason,
		)
		if failErr != nil {
			return failErr
		}
		return e.finishTerminalTask(ctx, task, workerID, failed, nil)
	}
	return e.finishRuntimeResult(ctx, run, task, workerID, executorID, result, true)
}
