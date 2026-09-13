package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type HumanTimeoutSweeper struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Checkpoints  *PGCheckpointStore
	Tasks        HumanResumeTaskStore
	BatchSize    int
	Now          func() time.Time
}

func (s *HumanTimeoutSweeper) Sweep(ctx context.Context) (int, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil || s.Checkpoints == nil || s.Tasks == nil {
		return 0, errors.New("human timeout sweeper dependencies are unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	limit := s.BatchSize
	if limit < 1 {
		limit = 32
	}
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin human timeout sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs, err := s.Runs.ListHumanDeadlineExpiredTx(ctx, tx, now, limit)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		if err := s.resumeTimeoutTx(ctx, tx, run, now); err != nil {
			return 0, fmt.Errorf("resume expired human run %q: %w", run.RunID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit human timeout sweep: %w", err)
	}
	return len(runs), nil
}

func (s *HumanTimeoutSweeper) resumeTimeoutTx(
	ctx context.Context,
	tx pgx.Tx,
	locked TeamRun,
	now time.Time,
) error {
	if locked.Status != StatusParked || locked.WaitKind == nil || *locked.WaitKind != WaitHuman ||
		locked.CheckpointRef == nil || *locked.CheckpointRef != CheckpointRef(locked.WorkspaceID, locked.RunID) {
		return ErrTeamRunResumeInvalid
	}
	detail, err := DecodeHumanWaitDetailV1(locked.WaitDetail)
	if err != nil || detail.DeadlineAt == nil || detail.DeadlineAt.After(now) || detail.TimeoutNodeID == "" {
		if err == nil {
			err = errors.New("human deadline is invalid or not expired")
		}
		return fmt.Errorf("%w: %v", ErrTeamRunResumeInvalid, err)
	}
	checkpoint, err := s.Checkpoints.GetTx(ctx, tx, locked.WorkspaceID, locked.RunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, locked); err != nil {
		return err
	}
	if checkpoint.NodeID != detail.NodeID {
		return fmt.Errorf("%w: human timeout node differs from checkpoint", ErrTeamRunIdentityMismatch)
	}
	idempotencyKey := fmt.Sprintf("teamrun-human-timeout:%s:%d", locked.RunID, locked.ResumeGeneration)
	executorID := humanResumeExecutorID(locked.RunID, idempotencyKey)
	resumed, err := s.Runs.ResumeRunningTx(ctx, tx, ResumeRequest{
		WorkspaceID: locked.WorkspaceID, RunID: locked.RunID,
		ExpectedStatus: StatusParked, ExpectedTeamRunGeneration: locked.Generation,
		ExpectedExecutionLeaseEpoch: locked.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    locked.ResumeGeneration, ExpectedWaitKind: WaitHuman,
		ExpectedResumeTokenHash: locked.ResumeTokenHash, HumanTimeout: true,
		ExecutorID: executorID, IdempotencyKey: idempotencyKey,
		Actor: "teamrun-human-timeout-sweeper", Source: "human_resume", OccurredAt: now,
	})
	if err != nil {
		return err
	}
	if checkpoint.CompletedOutputs == nil {
		checkpoint.CompletedOutputs = make(map[string]json.RawMessage)
	}
	timeoutOutput, _ := json.Marshal(map[string]any{"status": "timeout", "node_id": checkpoint.NodeID})
	checkpoint.CompletedOutputs[checkpoint.NodeID] = timeoutOutput
	checkpoint.NodeID = detail.TimeoutNodeID
	checkpoint.TeamRunGeneration = resumed.Generation
	checkpoint.ExecutionLeaseEpoch = resumed.ExecutionLeaseEpoch
	checkpoint.WrittenAt = now
	if _, err := s.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
		return err
	}
	taskID := humanResumeTaskID(locked.WorkspaceID, locked.RunID, idempotencyKey)
	taskPayload, err := json.Marshal(HumanResumeTaskPayloadV1{
		SchemaVersion: 1, Kind: "human_resume", RunID: locked.RunID,
		IdempotencyKey: idempotencyKey, Disposition: "timeout",
	})
	if err != nil {
		return err
	}
	if err := s.Tasks.EnqueueTx(ctx, tx, &taskqueue.Task{
		ID: taskID, WorkspaceID: resumed.WorkspaceID, ProjectID: resumed.ProjectID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: resumed.WorkflowID, WorkflowVersion: resumed.WorkflowVersion,
		RunSnapshotID: resumed.RunSnapshotID, Source: "human_resume", Kind: "team_workflow",
		ContextKey: resumed.RunID, Payload: taskPayload,
	}); err != nil {
		return fmt.Errorf("enqueue human timeout continuation: %w", err)
	}
	return nil
}
