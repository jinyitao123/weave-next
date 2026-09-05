package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
)

// Project only the public node identity; wait details may contain resume tokens.
func runActivityWaitNode(run teamrun.TeamRun) string {
	if run.Status != teamrun.StatusParked || run.WaitKind == nil {
		return ""
	}
	switch *run.WaitKind {
	case teamrun.WaitRuntime:
		if detail, err := teamrun.DecodeRuntimeWaitDetailV1(run.WaitDetail); err == nil {
			return detail.NodeID
		}
	case teamrun.WaitHuman:
		if detail, err := teamrun.DecodeHumanWaitDetailV1(run.WaitDetail); err == nil {
			return detail.NodeID
		}
	case teamrun.WaitCorrection:
		if detail, err := teamrun.DecodeCorrectionWaitDetailV1(run.WaitDetail); err == nil {
			return detail.SafeNodeID
		}
	case teamrun.WaitTimer:
		var detail struct {
			WakeAt time.Time `json:"wake_at"`
			NodeID string    `json:"node_id"`
		}
		if json.Unmarshal(run.WaitDetail, &detail) == nil && !detail.WakeAt.IsZero() {
			return detail.NodeID
		}
	case teamrun.WaitFanout:
		if detail, ok := runActivityFanoutWait(run); ok {
			return detail.JoinNodeID
		}
	}
	return ""
}

func runActivityFanoutWait(run teamrun.TeamRun) (fanout.FanoutWaitPayload, bool) {
	var wait fanout.FanoutWaitPayload
	ok := run.Status == teamrun.StatusParked && run.WaitKind != nil && *run.WaitKind == teamrun.WaitFanout &&
		json.Unmarshal(run.WaitDetail, &wait) == nil && wait.GroupID != "" && wait.Generation != "" && wait.ParentRunID == run.RunID
	return wait, ok
}

func applyRunActivityBranch(run teamrun.TeamRun, wait fanout.FanoutWaitPayload, task *taskqueue.Task, stage *runActivityMemberStage) bool {
	if task == nil || task.WorkspaceID != run.WorkspaceID || task.RunSnapshotID != run.RunSnapshotID ||
		task.ContextKey != wait.GroupID {
		return false
	}
	switch task.Status {
	case taskqueue.StatusFailed:
		failure := teamrun.ClassifyFailure(errors.New(task.Error))
		stage.Status = "failed"
		stage.FailureClass, stage.FailureReason = string(failure.Class), failure.Reason
		stage.Retryable = failure.Retryable && failure.Class == teamrun.FailureClassInfrastructure
	case taskqueue.StatusQueued, taskqueue.StatusDispatched, taskqueue.StatusRunning:
		if task.Status == taskqueue.StatusQueued || stage.Status != "running" {
			stage.StartedAt = nil
			stage.DurationMs, stage.ToolCalls, stage.Tools = 0, 0, nil
		}
		stage.Status = "running"
		if task.Status == taskqueue.StatusQueued {
			stage.Status = "pending"
		}
		stage.CompletedAt = nil
		stage.FailureClass, stage.FailureReason, stage.Retryable = "", "", false
	default:
		return false
	}
	return true
}

// Events describe history. Retry actions must instead match the current durable
// wait and, for parallel branches, the current generation's queue entry.
func (s *Server) reconcileRunActivityRecovery(ctx context.Context, run teamrun.TeamRun, members []runActivityMember) string {
	waitNode := runActivityWaitNode(run)
	stopUnconfirmed := s.runActivityStopUnconfirmed(ctx, run)
	wait, fanoutWait := runActivityFanoutWait(run)
	stopped, stopKnown := false, false
	if run.Status == teamrun.StatusParked && s.Tasks != nil {
		var err error
		stopped, err = s.Tasks.RuntimeFailuresStopped(ctx, run.WorkspaceID, run.RunSnapshotID)
		stopKnown = err == nil
	}
	for memberIndex := range members {
		member := &members[memberIndex]
		for stageIndex := range member.Stages {
			stage := &member.Stages[stageIndex]
			stage.Retryable = false
			if run.Status.Terminal() {
				projectTerminalRunActivity(run, member, stage, stopUnconfirmed)
				continue
			}
			if run.CancelRequestedAt != nil || run.Status != teamrun.StatusParked || run.WaitKind == nil {
				continue
			}
			if *run.WaitKind == teamrun.WaitRuntime && waitNode != "" && stage.NodeID == waitNode {
				stage.Retryable = stopped && stopKnown
				stage.Status = "failed"
				stage.FailureClass = string(teamrun.FailureClassInfrastructure)
				if stage.FailureReason == "" {
					stage.FailureReason = "The execution environment stopped before this stage could finish."
				}
				if stopKnown && !stopped {
					stage.FailureReason = runtimeStopPendingReason
				}
				member.Status = "failed"
				continue
			}
			if !fanoutWait || s.Tasks == nil {
				continue
			}
			taskID, err := fanout.DeriveLegTaskID(wait.GroupID, stage.NodeID, wait.Generation)
			if err != nil {
				continue
			}
			task, err := s.Tasks.Get(ctx, run.WorkspaceID, taskID)
			if err != nil || !applyRunActivityBranch(run, wait, task, stage) {
				continue
			}
			if stage.Retryable {
				stage.Retryable = stopped && stopKnown
				if stopKnown && !stopped {
					stage.FailureReason = runtimeStopPendingReason
				}
			}
			member.Status = stage.Status
		}
	}
	return waitNode
}

const runtimeStopPendingReason = "Reconnect the runtime and wait for the previous execution to confirm it has stopped."

func projectTerminalRunActivity(run teamrun.TeamRun, member *runActivityMember, stage *runActivityMemberStage, stopUnconfirmed bool) {
	if stage.Status == "completed" || stage.Status == "failed" {
		return
	}
	// A failed run does not prove that every later member executed and failed.
	if (run.Status == teamrun.StatusFailed || run.Status == teamrun.StatusAbandoned) &&
		(stage.Status == "pending" || stage.Status == "not_recorded") &&
		stage.StartedAt == nil && stage.CompletedAt == nil && stage.ToolCalls == 0 &&
		len(stage.Tools) == 0 && len(stage.OutputRefs) == 0 && len(stage.PublicUpdates) == 0 {
		stage.Status, member.Status = "not_recorded", "not_recorded"
		return
	}
	switch run.Status {
	case teamrun.StatusCancelled:
		stage.Status, stage.FailureClass = "cancelled", string(teamrun.FailureClassCancelled)
		stage.FailureReason = "The run was stopped; completed outputs were preserved."
	case teamrun.StatusAbandoned:
		stage.Status, stage.FailureClass = "failed", string(teamrun.FailureClassInfrastructure)
		stage.FailureReason = "The run ended before this stage completed."
		if stopUnconfirmed {
			stage.FailureReason = "The run ended without confirmation that execution stopped."
		}
	case teamrun.StatusFailed:
		stage.Status = "failed"
		if stage.FailureReason == "" {
			stage.FailureReason = "The run ended before this stage completed."
		}
	default:
		stage.Status = "not_recorded" // success does not invent missing stage evidence
	}
	member.Status = stage.Status
}

// Terminal rows clear cancellation fields. Only the recorded transition can
// distinguish unconfirmed stop from unrelated abandoned execution failures.
func (s *Server) runActivityStopUnconfirmed(ctx context.Context, run teamrun.TeamRun) bool {
	if run.Status != teamrun.StatusAbandoned || s.Pool == nil {
		return false
	}
	var unconfirmed bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_run_transitions
 WHERE workspace_id=$1 AND run_id=$2 AND team_run_generation=$3
 AND from_status='cancel_requested' AND to_status='abandoned' AND NOT orphaned)`, run.WorkspaceID, run.RunID, run.Generation).Scan(&unconfirmed)
	return err == nil && unconfirmed
}
