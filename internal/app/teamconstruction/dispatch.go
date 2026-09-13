package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

const TaskKind = "team_build"

type ExecutionSubmission struct {
	WorkspaceID string `json:"workspace_id"`
	BuildRunID  string `json:"build_run_id"`
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
}

type BuildExecutor interface {
	Execute(context.Context, string, string) (teamorch.Result, error)
}

// Dispatcher assembles a business command with platform admission. It owns no
// polling loop, leases, retry worker or execution-state table.
type Dispatcher struct {
	Pool     *pgxpool.Pool
	Runs     *teambuild.Store
	Tasks    *taskqueue.Store
	Worker   *taskqueue.Worker
	Executor BuildExecutor
}

func taskID(workspaceID, buildRunID string) string {
	return "team-build-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(workspaceID+"\x00"+buildRunID)).String()
}

func (d *Dispatcher) Submit(ctx context.Context, workspaceID, buildRunID string) (ExecutionSubmission, error) {
	if d == nil || d.Pool == nil || d.Runs == nil || d.Tasks == nil || d.Executor == nil {
		return ExecutionSubmission{}, errors.New("team construction is unavailable")
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return ExecutionSubmission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize submission against cancellation and budget reauthorization.
	run, err := d.Runs.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return ExecutionSubmission{}, err
	}
	result := ExecutionSubmission{WorkspaceID: workspaceID, BuildRunID: buildRunID, Status: run.Status}
	if !activeStatus(run.Status) {
		if run.Status == teambuild.StatusPlanning {
			return ExecutionSubmission{}, errors.New("team build requires authorization")
		}
		return result, nil
	}
	id := taskID(workspaceID, buildRunID)
	queued, err := d.Tasks.GetTx(ctx, tx, workspaceID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		payload, _ := json.Marshal(map[string]string{"build_run_id": buildRunID})
		queued = &taskqueue.Task{DeadlineAt: &run.ExpiresAt, OutcomeSensitive: true, ID: id, WorkspaceID: workspaceID, BuildRunID: buildRunID,
			Kind: TaskKind, Source: TaskKind, IdentityKind: taskqueue.IdentityTeamBuild,
			IdentitySchemaVersion: 2, ContextKey: buildRunID, Payload: payload, Status: taskqueue.StatusQueued}
		err = d.Tasks.EnqueueTx(ctx, tx, queued)
	} else if err == nil && queued.IsTerminal() {
		queued, err = d.Tasks.ResumeTaskTx(ctx, tx, workspaceID, id, TaskKind, buildRunID)
	}
	if err != nil {
		return ExecutionSubmission{}, err
	}
	if queued.Status == taskqueue.StatusCancelRequested {
		return ExecutionSubmission{}, errors.New("team build execution has not stopped")
	}
	if err := tx.Commit(ctx); err != nil {
		return ExecutionSubmission{}, err
	}
	result.TaskID, result.Status = id, queued.Status
	return result, nil
}

// Cancel commits business cancellation and dispatch fencing together. A running
// handler must still return before the platform queue acknowledges physical stop.
func (d *Dispatcher) Cancel(ctx context.Context, workspaceID, buildRunID, actor, reason string) (teambuild.TeamBuildRun, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := d.Runs.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if run.Status != teambuild.StatusCancelled {
		run, err = d.Runs.TransitionStatusTx(ctx, tx, workspaceID, buildRunID, run.Status, teambuild.StatusCancelled, actor, reason)
		if err != nil {
			return teambuild.TeamBuildRun{}, err
		}
	}
	id := taskID(workspaceID, buildRunID)
	queued, err := d.Tasks.GetTx(ctx, tx, workspaceID, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return teambuild.TeamBuildRun{}, err
	}
	shouldCancel := queued != nil && (!queued.IsTerminal() || queued.WorkerID != "")
	if shouldCancel {
		if err := d.Tasks.CancelTx(ctx, tx, workspaceID, id); err != nil {
			return teambuild.TeamBuildRun{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if shouldCancel && d.Worker != nil {
		_ = d.Worker.CancelTask(ctx, workspaceID, id)
	}
	return run, nil
}

func (d *Dispatcher) ExecuteTask(ctx context.Context, task taskqueue.Task) (taskqueue.TaskResult, error) {
	if task.IdentityKind != taskqueue.IdentityTeamBuild || task.IdentitySchemaVersion != 2 || task.Kind != TaskKind || task.Source != TaskKind || task.BuildRunID == "" || task.ID != taskID(task.WorkspaceID, task.BuildRunID) {
		return taskqueue.TaskResult{}, errors.New("invalid team build task identity")
	}
	var payload struct {
		BuildRunID string `json:"build_run_id"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil || payload.BuildRunID != task.BuildRunID {
		return taskqueue.TaskResult{}, errors.New("team build payload differs from durable identity")
	}
	run, err := d.Runs.GetBuildRun(ctx, task.WorkspaceID, task.BuildRunID)
	if err != nil {
		return taskqueue.TaskResult{}, err
	}
	if !activeStatus(run.Status) {
		encoded, _ := json.Marshal(teamorch.Result{Status: run.Status})
		return taskqueue.TaskResult{Result: encoded}, nil
	}
	result, err := d.Executor.Execute(ctx, task.WorkspaceID, task.BuildRunID)
	if err != nil {
		if ctx.Err() == nil {
			if blockErr := d.blockAfterFailure(ctx, task, err); blockErr != nil {
				return taskqueue.TaskResult{}, errors.Join(err, blockErr)
			}
		}
		return taskqueue.TaskResult{}, err
	}
	if activeStatus(result.Status) {
		delay := time.Duration(0)
		if strings.TrimSpace(result.StopReason) != "" {
			delay = 30 * time.Second
		}
		return taskqueue.TaskResult{Continue: true, ContinueAfter: delay}, nil
	}
	encoded, err := json.Marshal(result)
	return taskqueue.TaskResult{Result: encoded}, err
}

func activeStatus(status string) bool {
	return status == teambuild.StatusAuthorized || status == teambuild.StatusRoundRunning || status == teambuild.StatusPublishing
}

func (d *Dispatcher) blockAfterFailure(ctx context.Context, task taskqueue.Task, executionErr error) error {
	run, err := d.Runs.GetBuildRun(ctx, task.WorkspaceID, task.BuildRunID)
	if err != nil {
		return err
	}
	if !activeStatus(run.Status) {
		return nil
	}
	class := "infra"
	if errors.Is(executionErr, teambuild.ErrCompilerRevisionRequired) {
		class = "blueprint_required"
	}
	reason := fmt.Sprintf("execution_failed:%s: %s", class, strings.TrimSpace(executionErr.Error()))
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err = d.Runs.TransitionStatus(ctx, task.WorkspaceID, task.BuildRunID, run.Status, teambuild.StatusBlocked, "team-build-worker", reason)
	return err
}
