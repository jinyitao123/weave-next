package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

// ProcessNext claims and executes at most one team_workflow task.
func (e *Executor) ProcessNext(ctx context.Context, workerID string) (bool, error) {
	if e == nil || e.Tasks == nil || e.Consumer == nil ||
		e.Transactions == nil || e.Runs == nil || e.Checkpoints == nil ||
		e.Runtime == nil {
		return false, errors.New("team run executor dependencies are unavailable")
	}
	if workerID == "" {
		return false, errors.New("worker_id is required")
	}
	task, err := e.Tasks.Claim(ctx, workerID, taskqueue.ClaimFilter{
		Kind:          "team_workflow",
		WorkspaceID:   e.ClaimWorkspaceID,
		RunSnapshotID: e.ClaimRunSnapshotID,
		IdentityKind:  taskqueue.IdentityTeamWorkflow,
	})
	if err != nil {
		return false, fmt.Errorf("claim team workflow task: %w", err)
	}
	if task == nil {
		return false, nil
	}
	return true, e.processClaimedWorkflowTask(ctx, task, workerID)
}

func (e *Executor) processClaimedWorkflowTask(ctx context.Context, task *taskqueue.Task, workerID string) error {
	if task.WorkerID == "" {
		task.WorkerID = workerID
	}
	if task.WorkerID != workerID {
		return fmt.Errorf("claimed task worker_id %q differs from %q", task.WorkerID, workerID)
	}
	if kind, err := workflowTaskPayloadKind(task.Payload); err == nil {
		switch kind {
		case "fanout_leg":
			return e.processFanoutLeg(ctx, task, workerID)
		case "fanout_resume":
			return e.processFanoutResume(ctx, task, workerID)
		case "human_resume":
			return e.processHumanResume(ctx, task, workerID)
		case "correction_resume":
			return e.processCorrectionResume(ctx, task, workerID)
		}
	}

	run, err := e.Consumer.ConsumeClaimed(ctx, task, workerID)
	if err != nil {
		return err
	}
	return e.processConsumedWorkflowRun(ctx, task, workerID, run)
}

func workflowTaskPayloadKind(raw json.RawMessage) (string, error) {
	var header struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.SchemaVersion != 1 || header.Kind == "" {
		return "", errors.New("workflow task payload has no runtime kind")
	}
	return header.Kind, nil
}
