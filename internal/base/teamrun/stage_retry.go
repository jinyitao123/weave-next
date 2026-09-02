package teamrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type StageRetryRequest struct {
	WorkspaceID string
	RunID       string
	NodeID      string
}

type StageRetryResult struct {
	RunID                    string   `json:"run_id"`
	NodeID                   string   `json:"node_id"`
	Status                   string   `json:"status"`
	AffectedNodeIDs          []string `json:"affected_node_ids"`
	PreservedCompletedStages bool     `json:"preserved_completed_stages"`
}

type failedTaskRequeuer interface {
	Get(context.Context, string, string) (*taskqueue.Task, error)
	RequeueFailedTask(context.Context, string, string, string, string) (*taskqueue.Task, error)
}

type StageRetryService struct {
	Runs  *PGStore
	Tasks failedTaskRequeuer
}

// Retry requeues one infrastructure-failed fanout branch. The parked parent,
// frozen inputs, successful siblings, and their deliverables are preserved.
func (s *StageRetryService) Retry(ctx context.Context, request StageRetryRequest) (StageRetryResult, error) {
	if s == nil || s.Runs == nil || s.Tasks == nil || request.WorkspaceID == "" || request.RunID == "" || request.NodeID == "" {
		return StageRetryResult{}, errors.New("stage retry dependencies and identity are required")
	}
	run, err := s.Runs.Get(ctx, request.WorkspaceID, request.RunID)
	if err != nil {
		return StageRetryResult{}, err
	}
	if run.Status != StatusParked || run.WaitKind == nil || *run.WaitKind != WaitFanout {
		return StageRetryResult{}, fmt.Errorf("%w: run is not waiting on a recoverable team stage", ErrTeamRunStateConflict)
	}
	var wait fanout.FanoutWaitPayload
	if err := decodeFanoutExact(run.WaitDetail, &wait); err != nil || wait.GroupID == "" || wait.Generation == "" || wait.ParentRunID != run.RunID {
		return StageRetryResult{}, fmt.Errorf("%w: fanout wait identity is invalid", ErrTeamRunSnapshotUnavailable)
	}
	taskID, err := fanout.DeriveLegTaskID(wait.GroupID, request.NodeID, wait.Generation)
	if err != nil {
		return StageRetryResult{}, err
	}
	task, err := s.Tasks.Get(ctx, request.WorkspaceID, taskID)
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
	if _, err := s.Tasks.RequeueFailedTask(ctx, request.WorkspaceID, taskID, run.RunSnapshotID, wait.GroupID); err != nil {
		latest, getErr := s.Tasks.Get(ctx, request.WorkspaceID, taskID)
		if getErr != nil || latest.RunSnapshotID != run.RunSnapshotID || latest.ContextKey != wait.GroupID ||
			(latest.Status != taskqueue.StatusQueued && latest.Status != taskqueue.StatusDispatched && latest.Status != taskqueue.StatusRunning) {
			return StageRetryResult{}, err
		}
		result.Status = latest.Status
		return result, nil
	}
	result.Status = taskqueue.StatusQueued
	return result, nil
}
