package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestRunActivityRecoveryUsesCurrentWaitAndPreservesOutputs(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    teamrun.Status
		kind      teamrun.WaitKind
		detail    string
		cancelled bool
		wantNode  string
		wantRetry bool
	}{
		{"runtime queue unavailable", teamrun.StatusParked, teamrun.WaitRuntime, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`, false, "deliver", false},
		{"resumed", teamrun.StatusRunning, teamrun.WaitRuntime, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`, false, "", false},
		{"stopped", teamrun.StatusCancelled, teamrun.WaitRuntime, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`, false, "", false},
		{"stop requested", teamrun.StatusParked, teamrun.WaitRuntime, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`, true, "deliver", false},
		{"completed", teamrun.StatusSucceeded, teamrun.WaitRuntime, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver"}`, false, "", false},
		{"invalid runtime", teamrun.StatusParked, teamrun.WaitRuntime, `{"schema_version":2,"wait_type":"runtime","node_id":"deliver"}`, false, "", false},
		{"human", teamrun.StatusParked, teamrun.WaitHuman, `{"schema_version":1,"wait_type":"human","node_id":"review","success_node_id":"deliver","resume_schema":{},"task":{"title":"Review","instructions":"Confirm"}}`, false, "review", false},
		{"correction", teamrun.StatusParked, teamrun.WaitCorrection, `{"schema_version":1,"wait_type":"correction","correction_id":"c-1","target_kind":"team","instruction":"Revise","safe_node_id":"deliver","restart_node_id":"review","affected_node_ids":["review","deliver"]}`, false, "deliver", false},
		{"timer", teamrun.StatusParked, teamrun.WaitTimer, `{"wake_at":"2026-09-05T00:00:00Z","node_id":"pause"}`, false, "pause", false},
		{"fanout unavailable", teamrun.StatusParked, teamrun.WaitFanout, `{"group_id":"group","generation":"1","parent_run_id":"run","join_node_id":"join","resume_token":"private-token"}`, false, "join", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := teamrun.TeamRun{RunID: "run", Status: test.status, WaitKind: &test.kind, WaitDetail: json.RawMessage(test.detail)}
			if test.cancelled {
				now := time.Now()
				run.CancelRequestedAt = &now
			}
			members := []runActivityMember{{AgentID: "writer", Stages: []runActivityMemberStage{
				{NodeID: "research", Status: "completed", OutputRefs: []string{"retained-evidence"}, Retryable: true},
				{NodeID: "old-failure", Status: "failed", Retryable: true},
				{NodeID: "deliver", Status: "pending"}, // The failure event may be outside the bounded activity window.
			}}}
			node := (&Server{}).reconcileRunActivityRecovery(context.Background(), run, members)
			stages := members[0].Stages
			if node != test.wantNode || stages[2].Retryable != test.wantRetry || stages[0].Retryable || stages[1].Retryable {
				t.Fatalf("incorrect recovery: node=%q stages=%#v", node, stages)
			}
			if stages[0].Status != "completed" || len(stages[0].OutputRefs) != 1 || stages[0].OutputRefs[0] != "retained-evidence" {
				t.Fatal("recovery projection changed completed work")
			}
			if test.wantRetry && (stages[2].Status != "failed" || stages[2].FailureClass != "infrastructure" || stages[2].FailureReason == "") {
				t.Fatalf("missing current failure explanation: %#v", stages[2])
			}
		})
	}
}

func TestRunActivityFanoutRetryRequiresCurrentFailedBranch(t *testing.T) {
	run := teamrun.TeamRun{WorkspaceID: "workspace", RunSnapshotID: "snapshot"}
	wait := fanout.FanoutWaitPayload{GroupID: "group"}
	valid := taskqueue.Task{WorkspaceID: "workspace", RunSnapshotID: "snapshot", ContextKey: "group", Status: taskqueue.StatusFailed, Error: "connection refused"}
	stage := runActivityMemberStage{}
	if !applyRunActivityBranch(run, wait, &valid, &stage) || !stage.Retryable {
		t.Fatal("current infrastructure failure cannot be retried")
	}
	for _, change := range []func(*taskqueue.Task){
		func(task *taskqueue.Task) { task.WorkspaceID = "other" },
		func(task *taskqueue.Task) { task.RunSnapshotID = "other" },
		func(task *taskqueue.Task) { task.ContextKey = "other" },
		func(task *taskqueue.Task) { task.Status = taskqueue.StatusQueued },
		func(task *taskqueue.Task) { task.Status = taskqueue.StatusRunning },
		func(task *taskqueue.Task) { task.Status = taskqueue.StatusCompleted },
		func(task *taskqueue.Task) { task.Status = taskqueue.StatusCancelled },
		func(task *taskqueue.Task) { task.Error = "invalid output" },
	} {
		task := valid
		change(&task)
		stage := runActivityMemberStage{}
		applyRunActivityBranch(run, wait, &task, &stage)
		if stage.Retryable {
			t.Fatalf("stale or nonrecoverable task offered retry: %#v", task)
		}
	}
}

func TestRunActivityFanoutQueuedRetryClearsOldFailure(t *testing.T) {
	run := teamrun.TeamRun{WorkspaceID: "workspace", RunSnapshotID: "snapshot"}
	wait := fanout.FanoutWaitPayload{GroupID: "group"}
	task := taskqueue.Task{WorkspaceID: "workspace", RunSnapshotID: "snapshot", ContextKey: "group", Status: taskqueue.StatusQueued}
	completedAt := time.Now()
	stage := runActivityMemberStage{Status: "failed", StartedAt: &completedAt, CompletedAt: &completedAt, DurationMs: 400, ToolCalls: 2, Retryable: true,
		FailureClass: "infrastructure", FailureReason: "Old failure", OutputRefs: []string{"retained-output"}}
	if !applyRunActivityBranch(run, wait, &task, &stage) || stage.Status != "pending" || stage.Retryable ||
		stage.StartedAt != nil || stage.CompletedAt != nil || stage.DurationMs != 0 || stage.ToolCalls != 0 ||
		stage.FailureClass != "" || stage.FailureReason != "" || len(stage.OutputRefs) != 1 {
		t.Fatalf("queued retry still looks failed or lost saved output: %#v", stage)
	}
}
