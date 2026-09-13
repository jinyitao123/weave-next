package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestRunActivityRetryWaitsForRemoteExitRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
 INSERT INTO weave_teams(id,workspace_id,name) VALUES('team','ws','team');`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"current", "other"} {
		_, err := snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
			RunID: id, WorkspaceID: "ws", TeamID: "team", SnapshotSchemaVersion: 2, Mode: "free_collab", LeadAvatarID: "agent", LeadAvatarVersion: 1,
			WorkerVersions: json.RawMessage(`{}`), TeamWorkerSnapshot: json.RawMessage(`[]`), InlineDependencies: json.RawMessage(`{}`), RuntimeAssignment: json.RawMessage(`{}`),
			AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-05T00:00:00Z"}`),
			RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
			TriggerSourceV2:   json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	for _, id := range []string{"current", "other"} {
		task := &taskqueue.Task{ID: id, WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent,
			IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamFreeCollab, RunSnapshotID: id, Kind: "engine_exec", Payload: json.RawMessage(`{}`)}
		if err := tasks.Enqueue(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='failed',error=$2,worker_id='runtime-worker' WHERE id=$1`, id, taskqueue.RuntimeLeaseExpiredError); err != nil {
			t.Fatal(err)
		}
	}
	run := teamrun.TeamRun{WorkspaceID: "ws", RunID: "current", RunSnapshotID: "current", Status: teamrun.StatusParked, WaitKind: ptrWait(teamrun.WaitRuntime), WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"worker"}`)}
	server := &Server{Tasks: tasks}
	check := func(want bool) {
		t.Helper()
		members := []runActivityMember{{Stages: []runActivityMemberStage{{NodeID: "worker", Status: "failed", Retryable: true}}}}
		server.reconcileRunActivityRecovery(ctx, run, members)
		if members[0].Stages[0].Retryable != want {
			t.Fatalf("retryable=%v want=%v", members[0].Stages[0].Retryable, want)
		}
	}
	check(false)
	if err := tasks.AcknowledgeExecutionStopped(ctx, "current", "wrong-worker"); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := tasks.AcknowledgeExecutionStopped(ctx, "current", "runtime-worker"); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The unacknowledged attempt from another run must not disable this run.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := taskqueue.RuntimeFailuresStoppedTx(ctx, tx, "ws", "current")
	_ = tx.Rollback(ctx)
	if err != nil || !stopped {
		t.Fatalf("command/read eligibility diverged: %v %v", stopped, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_final_deliverables(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
 VALUES('saved','ws','user','agent','session','event','current','current','Saved','retained body','text/plain','{}')`); err != nil {
		t.Fatal(err)
	}
	for _, status := range []teamrun.Status{teamrun.StatusCancelled, teamrun.StatusAbandoned, teamrun.StatusFailed} {
		run.Status = status
		members := []runActivityMember{{Status: "completed", Stages: []runActivityMemberStage{{NodeID: "done", Status: "completed", OutputRefs: []string{"saved"}}}},
			{Status: "running", Stages: []runActivityMemberStage{{NodeID: "worker", Status: "running", Retryable: true}}},
			{Status: "failed", Stages: []runActivityMemberStage{{NodeID: "later", Status: "not_recorded"}}}}
		server.reconcileRunActivityRecovery(ctx, run, members)
		summarizeRunActivityMembers(members)
		want := "failed"
		if status == teamrun.StatusCancelled {
			want = "cancelled"
		}
		if members[1].Status != want || members[1].Stages[0].Status != want || members[1].Stages[0].Retryable || members[1].Stages[0].CompletedAt != nil {
			t.Fatalf("invented terminal execution facts: %+v", members)
		}
		if members[0].Status != "completed" || members[0].Stages[0].OutputRefs[0] != "saved" {
			t.Fatal("lost completed stage")
		}
		unobserved := "not_recorded"
		if status == teamrun.StatusCancelled {
			unobserved = "cancelled"
		}
		if members[2].Status != unobserved || members[2].Stages[0].Status != unobserved {
			t.Fatalf("unobserved member inherited another member's failure: %+v", members[2])
		}
		saved, err := deliverable.New(pool).Get(ctx, "ws", "saved")
		if err != nil || saved.Content != "retained body" {
			t.Fatalf("lost saved output: %+v %v", saved, err)
		}
	}
}

func ptrWait(kind teamrun.WaitKind) *teamrun.WaitKind { return &kind }
