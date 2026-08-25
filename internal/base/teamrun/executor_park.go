package teamrun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

func (e *Executor) newResumeTokenHash() ([]byte, error) {
	if e.ResumeTokenHash != nil {
		return e.ResumeTokenHash()
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("generate resume token hash: %w", err)
	}
	return value, nil
}

func checkpointFromPark(
	run TeamRun,
	park RuntimePark,
	writtenAt time.Time,
) WorkflowCheckpointV1 {
	return WorkflowCheckpointV1{
		SchemaVersion: WorkflowCheckpointSchemaVersion,
		Stamp: WorkflowRunStamp{
			WorkspaceID:     run.WorkspaceID,
			WorkflowID:      run.WorkflowID,
			WorkflowVersion: run.WorkflowVersion,
			RunSnapshotID:   run.RunSnapshotID,
		},
		RunID:                 run.RunID,
		TeamRunGeneration:     run.Generation,
		ExecutionLeaseEpoch:   run.ExecutionLeaseEpoch,
		NodeID:                park.NodeID,
		CompletedOutputs:      park.CompletedOutputs,
		Usage:                 park.UsageCheckpoint,
		UsageComplete:         park.UsageComplete,
		UsageIncompleteReason: park.UsageIncompleteReason,
		WrittenAt:             writtenAt,
	}
}

func (e *Executor) parkRunning(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	executorID string,
	park RuntimePark,
) (TeamRun, error) {
	if park.WaitKind != WaitTimer && park.WaitKind != WaitFanout {
		return TeamRun{}, executionError(
			ErrorCodeUnexpectedInteractiveYield,
			errors.New("workflow executor does not support this park kind"),
		)
	}
	var (
		tokenHash   []byte
		resumeToken string
		prepare     FanoutPrepareRequest
	)
	switch park.WaitKind {
	case WaitFanout:
		if e.Fanout == nil {
			return TeamRun{}, executionError(ErrorCodeRuntimeIncompatible, errors.New("fanout coordinator is unavailable"))
		}
		if err := decodeExact(park.WaitDetail, &prepare); err != nil {
			return TeamRun{}, executionError(ErrorCodeRuntimeIncompatible, fmt.Errorf("decode fanout park plan: %w", err))
		}
		resumeToken = uuid.NewString()
		digest := sha256.Sum256([]byte(resumeToken))
		tokenHash = digest[:]
		prepare.ResumeToken = resumeToken
	default:
		var err error
		tokenHash, err = e.newResumeTokenHash()
		if err != nil {
			return TeamRun{}, err
		}
	}
	now := e.now()
	checkpoint := checkpointFromPark(run, park, now)
	tx, err := e.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run park: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if park.WaitKind == WaitFanout {
		intent, err := e.Fanout.PreparePark(ctx, tx, prepare)
		if err != nil {
			return TeamRun{}, fmt.Errorf("prepare fanout park: %w", err)
		}
		park.WaitDetail, err = json.Marshal(map[string]any{
			"wait_type": "fanout_group", "parked": true, "intent_id": intent.IntentID,
			"group_id": intent.GroupID, "generation": intent.Generation,
			"resume_token": resumeToken, "parent_run_id": run.RunID,
			"join_node_id": park.NodeID,
		})
		if err != nil {
			return TeamRun{}, fmt.Errorf("encode fanout wait detail: %w", err)
		}
	}
	checkpointRef := CheckpointRef(run.WorkspaceID, run.RunID)
	parked, err := e.Runs.ParkTx(ctx, tx, ParkRequest{
		WorkspaceID:                 run.WorkspaceID,
		RunID:                       run.RunID,
		ExpectedStatus:              StatusRunning,
		ExpectedTeamRunGeneration:   run.Generation,
		ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    run.ResumeGeneration,
		ExecutorID:                  executorID,
		WaitKind:                    park.WaitKind,
		WaitDetail:                  park.WaitDetail,
		ResumeTokenHash:             tokenHash,
		CheckpointRef:               checkpointRef,
		SessionLeaseEpoch:           nil,
		IdempotencyKey:              executorParkKey(task.ID, run.ResumeGeneration),
		Actor:                       executorID,
		Source:                      consumerSource,
		OccurredAt:                  now,
	})
	if err != nil {
		if errors.Is(err, ErrTeamRunStateConflict) ||
			errors.Is(err, ErrTeamRunCancelled) ||
			errors.Is(err, ErrTeamRunExecutionUnrecoverable) {
			_ = tx.Commit(ctx)
		}
		return TeamRun{}, fmt.Errorf("park team run: %w", err)
	}
	writtenRef, err := e.Checkpoints.PutTx(ctx, tx, checkpoint)
	if err != nil {
		return TeamRun{}, err
	}
	if writtenRef != checkpointRef {
		return TeamRun{}, errors.New("checkpoint store returned a non-canonical reference")
	}
	if park.WaitKind == WaitFanout {
		if err := writeFanoutYieldMarkerTx(ctx, tx, run, parked, checkpoint, now); err != nil {
			return TeamRun{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit team run park: %w", err)
	}
	if park.WaitKind == WaitFanout {
		var detail struct {
			IntentID   string `json:"intent_id"`
			Generation string `json:"generation"`
		}
		_ = json.Unmarshal(park.WaitDetail, &detail)
		if err := e.Fanout.ActivatePark(ctx, FanoutActivationRequest{
			WorkspaceID: run.WorkspaceID, IntentID: detail.IntentID, ParentRunID: run.RunID,
			CheckpointSequence: int64(parked.ResumeGeneration), Generation: detail.Generation,
			ResumeToken: resumeToken,
		}); err != nil {
			// Reconciler owns this post-checkpoint crash window.
			return parked, nil
		}
	}
	return parked, nil
}

func writeFanoutYieldMarkerTx(
	ctx context.Context,
	tx pgx.Tx,
	running TeamRun,
	parked TeamRun,
	checkpoint WorkflowCheckpointV1,
	now time.Time,
) error {
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, present, err := stateStore.ReadAttemptLeaseForUpdate(ctx, tx, running.WorkspaceID, running.RunID)
	if err != nil || !present {
		if err == nil {
			err = errors.New("fanout creator attempt lease is missing")
		}
		return fmt.Errorf("read fanout creator attempt lease: %w", err)
	}
	if lease.AttemptGeneration != int64(running.ExecutionLeaseEpoch) || lease.AttemptID != frozenAttemptID(running) ||
		lease.State != loomruntime.AttemptLeaseActive {
		return errors.New("fanout creator attempt lease differs from running TeamRun")
	}
	teamID, workflowID, snapshotID := running.TeamID, running.WorkflowID, running.RunSnapshotID
	workflowVersion := int32(running.WorkflowVersion)
	graphName := frozenGraphName(running)
	checkpointSequence := int64(parked.ResumeGeneration)
	auditVersion := int16(3)
	marker := loomruntime.TerminalMarkerV1{
		WorkspaceID: running.WorkspaceID, RunID: running.RunID, SchemaVersion: 1,
		AttemptGeneration: lease.AttemptGeneration, AttemptID: lease.AttemptID,
		Agent: graphName, AttributionScope: loomruntime.TerminalAttributionFixedWorkflow,
		TeamID: &teamID, WorkflowID: &workflowID, WorkflowVersion: &workflowVersion,
		RunSnapshotID: &snapshotID, RunStartedAt: lease.RunStartedAt,
		Phase: loomruntime.TerminalMarkerPhaseYielded, Status: loomruntime.TerminalMarkerStatusYielded,
		StopReason: "yielded", Source: loomruntime.TerminalMarkerSourceNormal, TerminalAt: now,
		EvidenceKind: loomruntime.TerminalMarkerEvidenceCheckpoint, CheckpointGraph: &graphName,
		CheckpointSeq: &checkpointSequence, CheckpointSavedAt: &checkpoint.WrittenAt,
		AuditState: loomruntime.TerminalMarkerAuditMaterialized, AuditSchemaVersion: &auditVersion,
		LineageState: loomruntime.TerminalMarkerLineagePending, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := stateStore.ApplyTerminalMarkerTransition(ctx, tx, marker); err != nil {
		return fmt.Errorf("write fanout yielded marker: %w", err)
	}
	return nil
}

func (e *Executor) finishRuntimeResult(
	ctx context.Context,
	run TeamRun,
	task *taskqueue.Task,
	workerID string,
	executorID string,
	result RuntimeResult,
	completeTask bool,
) error {
	switch result.Status {
	case RuntimeCompleted:
		succeeded, err := e.succeedRunning(
			ctx, run, task, executorID, result.Usage,
			result.UsageComplete, result.UsageIncompleteReason,
		)
		if err != nil {
			return err
		}
		if completeTask {
			return e.finishTerminalTask(ctx, task, workerID, succeeded, result.Output)
		}
		return nil
	case RuntimeParked:
		if result.Park == nil {
			return errors.New("parked runtime result lacks park facts")
		}
		parked, err := e.parkRunning(ctx, run, task, executorID, *result.Park)
		if err != nil {
			return err
		}
		if !completeTask {
			return nil
		}
		return e.finishParkedTask(ctx, task, workerID, parked)
	case RuntimeFailed:
		return errors.New("runtime returned failed status without an error")
	default:
		return fmt.Errorf("runtime result status %q is invalid", result.Status)
	}
}

func (e *Executor) finishParkedTask(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	run TeamRun,
) error {
	output, err := json.Marshal(map[string]any{
		"status": "parked", "run_id": run.RunID,
	})
	if err != nil {
		return err
	}
	return e.Tasks.CompleteClaimed(ctx, task.ID, workerID, output, run.RunID)
}
