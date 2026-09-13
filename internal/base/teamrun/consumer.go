package teamrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type TransactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type SnapshotReader interface {
	GetByRunID(context.Context, string, string) (*snapshot.TeamRunSnapshot, error)
}

type ConsumerRunStore interface {
	EstablishQueuedTx(context.Context, pgx.Tx, EstablishRequest) (TeamRun, error)
}

type ConsumerTaskStore interface {
	FailClaimed(context.Context, string, string, string) error
}

type Consumer struct {
	Transactions TransactionBeginner
	Snapshots    SnapshotReader
	Runs         ConsumerRunStore
	Tasks        ConsumerTaskStore
	Now          func() time.Time
}

type ConsumerError struct {
	Code  ErrorCode
	Cause error
}

func (e *ConsumerError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *ConsumerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ConsumeClaimed validates only the claimed task's durable identity against
// its immutable snapshot, then idempotently establishes the queued TeamRun.
func (c *Consumer) ConsumeClaimed(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
) (TeamRun, error) {
	if c == nil || c.Transactions == nil || c.Snapshots == nil ||
		c.Runs == nil || c.Tasks == nil {
		return TeamRun{}, errors.New("team run consumer dependencies are unavailable")
	}
	if task == nil || workerID == "" {
		return TeamRun{}, errors.New("claimed task and worker_id are required")
	}
	if err := validateWorkflowTaskIdentity(task); err != nil {
		return TeamRun{}, c.failClaimed(
			ctx, task, workerID, ErrorCodeIdentityMismatch, err,
		)
	}
	sourceKind, err := workflowTaskSourceKind(task.Source)
	if err != nil {
		return TeamRun{}, c.failClaimed(
			ctx, task, workerID, ErrorCodeIdentityMismatch, err,
		)
	}

	runSnapshot, err := c.Snapshots.GetByRunID(
		ctx, task.WorkspaceID, task.RunSnapshotID,
	)
	if err != nil {
		return TeamRun{}, c.failClaimed(
			ctx, task, workerID, ErrorCodeSnapshotUnavailable, err,
		)
	}
	if err := validateTaskSnapshotIdentity(task, runSnapshot); err != nil {
		code := ErrorCodeIdentityMismatch
		if errors.Is(err, errSnapshotDamaged) {
			code = ErrorCodeSnapshotUnavailable
		}
		return TeamRun{}, c.failClaimed(ctx, task, workerID, code, err)
	}

	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now()
	}
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return TeamRun{}, fmt.Errorf("begin team run establish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-read and lock the durable queue row before establishing the TeamRun.
	// A cancellation may have changed the task after Claim returned; trusting
	// only the caller's stale task value would let model execution start after
	// the stop request committed.
	var durableStatus, durableWorkerID string
	var durableClaimEpoch int64
	var durableLeaseExpiresAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT status,COALESCE(worker_id,''),claim_epoch,lease_expires_at
		FROM weave_task_queue
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, task.WorkspaceID, task.ID).Scan(
		&durableStatus, &durableWorkerID, &durableClaimEpoch, &durableLeaseExpiresAt,
	)
	if err != nil {
		return TeamRun{}, fmt.Errorf("lock claimed team workflow task: %w", err)
	}
	if durableStatus != taskqueue.StatusRunning || durableWorkerID != workerID ||
		durableClaimEpoch != task.ClaimEpoch || durableLeaseExpiresAt == nil ||
		durableLeaseExpiresAt.Before(now) {
		return TeamRun{}, ErrClaimFenced
	}

	run, err := c.Runs.EstablishQueuedTx(ctx, tx, EstablishRequest{
		WorkspaceID:             task.WorkspaceID,
		ProjectID:               runSnapshot.ProjectID,
		RunID:                   runSnapshot.RunID,
		TeamID:                  runSnapshot.TeamID,
		WorkflowID:              task.WorkflowID,
		WorkflowVersion:         task.WorkflowVersion,
		RunSnapshotID:           task.RunSnapshotID,
		SourceKind:              sourceKind,
		SourceTaskID:            task.ID,
		EstablishIdempotencyKey: establishKey(task.ID),
		Actor:                   consumerActor,
		Source:                  consumerSource,
		OccurredAt:              now,
	})
	if err != nil {
		if errors.Is(err, ErrTeamRunIdentityMismatch) {
			// Release the queue-row fence before FailClaimed opens its own
			// transaction and updates the same task.
			_ = tx.Rollback(ctx)
			return TeamRun{}, c.failClaimed(
				ctx, task, workerID, ErrorCodeIdentityMismatch, err,
			)
		}
		return TeamRun{}, fmt.Errorf("establish queued team run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamRun{}, fmt.Errorf("commit queued team run establish: %w", err)
	}
	return run, nil
}

const (
	consumerActor  = "teamrun-consumer"
	consumerSource = "team_workflow_task"
)

func establishKey(taskID string) string {
	return "teamrun-establish:" + taskID
}

func validateWorkflowTaskIdentity(task *taskqueue.Task) error {
	if task == nil {
		return errors.New("task durable workflow identity is invalid")
	}
	if task.ID == "" ||
		task.WorkspaceID == "" ||
		task.Kind != "team_workflow" ||
		task.IdentityKind != taskqueue.IdentityTeamWorkflow ||
		task.IdentitySchemaVersion != 2 ||
		task.WorkflowID == "" ||
		task.WorkflowVersion < 1 ||
		task.RunSnapshotID == "" {
		return errors.New("task durable workflow identity is invalid")
	}
	if _, err := workflowTaskSourceKind(task.Source); err != nil {
		return errors.New("task durable workflow identity is invalid")
	}
	if task.Agent != "" ||
		task.AgentID != "" ||
		task.AgentVersion != 0 ||
		task.ExecutionScope != "" ||
		task.RuntimeID != "" {
		return errors.New("team workflow task contains agent execution identity")
	}
	return nil
}

func workflowTaskSourceKind(source string) (SourceKind, error) {
	switch source {
	case "schedule":
		return SourceSchedule, nil
	case "manual":
		return SourceManual, nil
	case "session":
		return SourceSession, nil
	case "api":
		return SourceAPI, nil
	default:
		return "", fmt.Errorf("workflow task source %q is invalid", source)
	}
}

var errSnapshotDamaged = errors.New("team run snapshot is damaged")

// ErrClaimFenced means durable queue state changed after the caller received
// its claimed task. The worker must stop without establishing or executing a
// TeamRun; cancellation and lease recovery both use this fence.
var ErrClaimFenced = errors.New("team workflow task claim is no longer current")

func validateTaskSnapshotIdentity(
	task *taskqueue.Task,
	runSnapshot *snapshot.TeamRunSnapshot,
) error {
	if runSnapshot == nil {
		return fmt.Errorf("%w: snapshot is nil", errSnapshotDamaged)
	}
	if runSnapshot.RunID == "" ||
		runSnapshot.WorkspaceID == "" ||
		runSnapshot.TeamID == "" ||
		runSnapshot.SnapshotSchemaVersion != 2 ||
		runSnapshot.Mode != "fixed_workflow" ||
		runSnapshot.WorkflowID == "" ||
		runSnapshot.WorkflowVersion < 1 ||
		runSnapshot.ArtifactWorkflowID == "" ||
		runSnapshot.ArtifactWorkflowVersion < 1 {
		return fmt.Errorf("%w: fixed workflow fields are invalid", errSnapshotDamaged)
	}
	if runSnapshot.LeadAvatarID != "" ||
		runSnapshot.LeadAvatarVersion != 0 ||
		len(runSnapshot.WorkerVersions) != 0 ||
		len(runSnapshot.TeamWorkerSnapshot) != 0 ||
		runSnapshot.ArtifactRef != "" ||
		len(runSnapshot.InlineDependencies) != 0 ||
		runSnapshot.TriggerSource != "" {
		return fmt.Errorf("%w: forbidden fixed workflow fields are present", errSnapshotDamaged)
	}
	if runSnapshot.WorkspaceID != task.WorkspaceID ||
		runSnapshot.ProjectID != task.ProjectID ||
		runSnapshot.RunID != task.RunSnapshotID ||
		runSnapshot.WorkflowID != task.WorkflowID ||
		runSnapshot.WorkflowVersion != task.WorkflowVersion ||
		runSnapshot.ArtifactWorkflowID != task.WorkflowID ||
		runSnapshot.ArtifactWorkflowVersion != task.WorkflowVersion {
		return errors.New("task and snapshot workflow identity differ")
	}
	candidateIdentity := runSnapshot.BuildRunID != "" || runSnapshot.CandidateContentHash != ""
	switch task.Source {
	case "schedule":
		var trigger struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
			SourceRef     string `json:"source_ref"`
			OccurrenceKey string `json:"occurrence_key"`
		}
		if err := decodeExact(runSnapshot.TriggerSourceV2, &trigger); err != nil ||
			trigger.SchemaVersion != 1 ||
			trigger.Type != task.Source ||
			trigger.SourceRef == "" ||
			len(trigger.OccurrenceKey) != 64 {
			return fmt.Errorf("%w: schedule trigger is invalid", errSnapshotDamaged)
		}
		for _, value := range []byte(trigger.OccurrenceKey) {
			if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
				return fmt.Errorf("%w: occurrence key is invalid", errSnapshotDamaged)
			}
		}
	case "manual":
		var trigger struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
			SourceRef     string `json:"source_ref"`
		}
		if err := decodeExact(runSnapshot.TriggerSourceV2, &trigger); err != nil ||
			trigger.SchemaVersion != 1 ||
			trigger.Type != task.Source ||
			trigger.SourceRef == "" {
			return fmt.Errorf("%w: manual trigger is invalid", errSnapshotDamaged)
		}
	case "session":
		var trigger struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
			SourceRef     string `json:"source_ref"`
		}
		if err := decodeExact(runSnapshot.TriggerSourceV2, &trigger); err != nil ||
			trigger.SchemaVersion != 1 ||
			trigger.Type != "conversation_explicit" ||
			trigger.SourceRef == "" {
			return fmt.Errorf("%w: session trigger is invalid", errSnapshotDamaged)
		}
	case "api":
		var trigger struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
			SourceRef     string `json:"source_ref"`
		}
		if err := decodeExact(runSnapshot.TriggerSourceV2, &trigger); err != nil ||
			trigger.SchemaVersion != 1 ||
			trigger.Type != "api" ||
			trigger.SourceRef == "" ||
			candidateIdentity && trigger.SourceRef != runSnapshot.BuildRunID {
			return fmt.Errorf("%w: api trigger is invalid", errSnapshotDamaged)
		}
	default:
		return errors.New("task and snapshot trigger source are unsupported")
	}
	if candidateIdentity {
		if task.Source != "api" ||
			runSnapshot.BuildRunID == "" ||
			runSnapshot.CandidateContentHash == "" {
			return fmt.Errorf("%w: candidate snapshot identity is invalid", errSnapshotDamaged)
		}
	}
	var associations struct {
		SchemaVersion    int     `json:"schema_version"`
		ParentRunID      *string `json:"parent_run_id"`
		SourceSnapshotID *string `json:"source_snapshot_id"`
		TaskGroupID      *string `json:"task_group_id"`
	}
	if err := decodeExact(runSnapshot.RunAssociations, &associations); err != nil ||
		associations.SchemaVersion != 1 ||
		associations.ParentRunID != nil ||
		associations.SourceSnapshotID != nil ||
		associations.TaskGroupID != nil {
		return fmt.Errorf("%w: run associations are invalid", errSnapshotDamaged)
	}
	return nil
}

func decodeExact(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (c *Consumer) failClaimed(
	ctx context.Context,
	task *taskqueue.Task,
	workerID string,
	code ErrorCode,
	cause error,
) error {
	consumerErr := &ConsumerError{Code: code, Cause: cause}
	if task == nil || task.ID == "" {
		return consumerErr
	}
	if err := c.Tasks.FailClaimed(
		ctx, task.ID, workerID, string(code),
	); err != nil {
		return errors.Join(consumerErr, fmt.Errorf("fail claimed team workflow task: %w", err))
	}
	return consumerErr
}
