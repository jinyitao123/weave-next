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
)

const WorkflowCheckpointSchemaVersion = 1

var ErrWorkflowCheckpointMissing = errors.New("workflow checkpoint is missing")

type WorkflowRunStamp struct {
	WorkspaceID     string `json:"workspace_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	RunSnapshotID   string `json:"run_snapshot_id"`
}

type WorkflowCheckpointV1 struct {
	SchemaVersion       int                        `json:"schema_version"`
	Stamp               WorkflowRunStamp           `json:"stamp"`
	RunID               string                     `json:"run_id"`
	TeamRunGeneration   TeamRunGeneration          `json:"team_run_generation"`
	ExecutionLeaseEpoch ExecutionLeaseEpoch        `json:"execution_lease_epoch"`
	NodeID              string                     `json:"node_id"`
	CompletedOutputs    map[string]json.RawMessage `json:"completed_outputs"`
	// Usage is the persisted serial-machine usage accumulator checkpoint
	// (confirmed per-node contributions plus derived totals). Old checkpoints
	// omit it and resume with zero usage.
	Usage json.RawMessage `json:"usage,omitempty"`
	// UsageComplete/UsageIncompleteReason persist the usage-completeness
	// annotation across park/resume so a fanout park inside a round-bound
	// candidate run still reports usage_complete=false after resume. Old
	// checkpoints omit both fields and resume as usage-complete.
	UsageComplete         bool      `json:"usage_complete,omitempty"`
	UsageIncompleteReason string    `json:"usage_incomplete_reason,omitempty"`
	WrittenAt             time.Time `json:"written_at"`
}

type PGCheckpointStore struct{}

func NewPGCheckpointStore() *PGCheckpointStore { return &PGCheckpointStore{} }

func CheckpointNamespace(workspaceID string) string {
	return "teamrun-checkpoint:" + workspaceID
}

func CheckpointKey(runID string) string { return runID }

func CheckpointRef(workspaceID, runID string) string {
	return CheckpointNamespace(workspaceID) + "/" + CheckpointKey(runID)
}

func DecodeWorkflowCheckpointV1(raw []byte) (WorkflowCheckpointV1, error) {
	var checkpoint WorkflowCheckpointV1
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return WorkflowCheckpointV1{}, fmt.Errorf("%w: decode checkpoint: %v", ErrTeamRunSnapshotUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return WorkflowCheckpointV1{}, fmt.Errorf("%w: decode checkpoint: %v", ErrTeamRunSnapshotUnavailable, err)
	}
	if err := validateWorkflowCheckpoint(checkpoint); err != nil {
		return WorkflowCheckpointV1{}, err
	}
	return checkpoint, nil
}

func ValidateCheckpointRun(checkpoint WorkflowCheckpointV1, run TeamRun) error {
	if checkpoint.Stamp.WorkspaceID != run.WorkspaceID ||
		checkpoint.Stamp.WorkflowID != run.WorkflowID ||
		checkpoint.Stamp.WorkflowVersion != run.WorkflowVersion ||
		checkpoint.Stamp.RunSnapshotID != run.RunSnapshotID ||
		checkpoint.RunID != run.RunID {
		return fmt.Errorf("%w: checkpoint stamp differs from TeamRun", ErrTeamRunIdentityMismatch)
	}
	if checkpoint.TeamRunGeneration > run.Generation ||
		checkpoint.ExecutionLeaseEpoch > run.ExecutionLeaseEpoch {
		return fmt.Errorf("%w: checkpoint fencing facts are ahead of TeamRun", ErrTeamRunSnapshotUnavailable)
	}
	return nil
}

func ValidateCheckpointSnapshot(
	checkpoint WorkflowCheckpointV1,
	runSnapshot *snapshot.TeamRunSnapshot,
) error {
	if runSnapshot == nil ||
		runSnapshot.SnapshotSchemaVersion != 2 ||
		runSnapshot.Mode != "fixed_workflow" ||
		runSnapshot.WorkspaceID == "" ||
		runSnapshot.RunID == "" ||
		runSnapshot.WorkflowID == "" ||
		runSnapshot.WorkflowVersion < 1 ||
		runSnapshot.ArtifactWorkflowID == "" ||
		runSnapshot.ArtifactWorkflowVersion < 1 {
		return fmt.Errorf("%w: fixed workflow snapshot is unavailable", ErrTeamRunSnapshotUnavailable)
	}
	if checkpoint.Stamp.WorkspaceID != runSnapshot.WorkspaceID ||
		checkpoint.Stamp.WorkflowID != runSnapshot.WorkflowID ||
		checkpoint.Stamp.WorkflowVersion != runSnapshot.WorkflowVersion ||
		checkpoint.Stamp.WorkflowID != runSnapshot.ArtifactWorkflowID ||
		checkpoint.Stamp.WorkflowVersion != runSnapshot.ArtifactWorkflowVersion ||
		checkpoint.Stamp.RunSnapshotID != runSnapshot.RunID {
		return fmt.Errorf("%w: checkpoint stamp differs from snapshot", ErrTeamRunIdentityMismatch)
	}
	return nil
}

func (store *PGCheckpointStore) PutTx(
	ctx context.Context,
	tx pgx.Tx,
	checkpoint WorkflowCheckpointV1,
) (string, error) {
	if store == nil || tx == nil {
		return "", errors.New("checkpoint store and transaction are required")
	}
	if err := validateWorkflowCheckpoint(checkpoint); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return "", fmt.Errorf("encode workflow checkpoint: %w", err)
	}
	namespace := CheckpointNamespace(checkpoint.Stamp.WorkspaceID)
	key := CheckpointKey(checkpoint.RunID)
	if _, err := tx.Exec(ctx, `INSERT INTO loom_store (namespace,key,value,updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (namespace,key) DO UPDATE
		SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`,
		namespace, key, encoded, checkpoint.WrittenAt,
	); err != nil {
		return "", fmt.Errorf("write workflow checkpoint: %w", err)
	}
	return CheckpointRef(checkpoint.Stamp.WorkspaceID, checkpoint.RunID), nil
}

func (store *PGCheckpointStore) GetTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
) (WorkflowCheckpointV1, error) {
	if store == nil || tx == nil {
		return WorkflowCheckpointV1{}, errors.New("checkpoint store and transaction are required")
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT value FROM loom_store
		WHERE namespace=$1 AND key=$2`,
		CheckpointNamespace(workspaceID), CheckpointKey(runID),
	).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkflowCheckpointV1{}, fmt.Errorf(
				"%w: %w",
				ErrTeamRunSnapshotUnavailable,
				ErrWorkflowCheckpointMissing,
			)
		}
		return WorkflowCheckpointV1{}, fmt.Errorf("read workflow checkpoint: %w", err)
	}
	return DecodeWorkflowCheckpointV1(raw)
}
