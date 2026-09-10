package api

import (
	"encoding/json"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"strings"
	"testing"
	"time"
)

func TestMemberCheckpointProjectionAndRecoveryRemainScopedRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"ours", "other"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workflow_member_runs(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state,checkpoint_seq)
 VALUES($1,'root','member-'||$1,'call','snapshot','worker',1,repeat('a',64),'{}',2)`, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO weave_run_attempt_leases(workspace_id,run_id,attempt_generation,attempt_id,graph_name,run_started_at,attempt_started_at,state,heartbeat_at,lease_expires_at,retry_count)
 VALUES($1,'member-'||$1,1,'00000000-0000-4000-8000-000000000001','graph-'||$1,'2026-09-07T00:00:00Z',now(),'active',now(),now()+interval '1 minute',0)`, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES('checkpoint:graph-'||$1,'member-'||$1,$2,now())`, ws, []byte(`{"private_reasoning":"must-never-project"}`)); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{StoreExt: storeext.New(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	run := teamrun.TeamRun{WorkspaceID: "ours", RunID: "root", RunSnapshotID: "snapshot", Status: teamrun.StatusParked, WaitKind: ptrWait(teamrun.WaitRuntime), WaitDetail: json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"worker"}`)}
	members := []runActivityMember{{Stages: []runActivityMemberStage{{NodeID: "worker"}}}}
	server.projectMemberCheckpoints(t.Context(), run, members)
	stage := members[0].Stages[0]
	if stage.MemberRunID != "member-ours" || stage.CheckpointSavedAt == nil {
		t.Fatalf("projection=%+v", stage)
	}
	encoded, _ := json.Marshal(members)
	if strings.Contains(string(encoded), "must-never-project") || strings.Contains(string(encoded), "member-other") {
		t.Fatal("private or cross-workspace state leaked")
	}
	server.reconcileRunActivityRecovery(t.Context(), run, members)
	if members[0].Stages[0].Retryable {
		t.Fatal("live member owner offered continuation")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_run_attempt_leases SET state='yielded' WHERE workspace_id='ours'`); err != nil {
		t.Fatal(err)
	}
	server.reconcileRunActivityRecovery(t.Context(), run, members)
	if !members[0].Stages[0].Retryable {
		t.Fatal("stopped member cannot continue")
	}
	run.WaitDetail = json.RawMessage(`{"schema_version":1,"wait_type":"runtime","node_id":"worker","recovery_blocked":true}`)
	server.reconcileRunActivityRecovery(t.Context(), run, members)
	if members[0].Stages[0].Retryable || members[0].Stages[0].FailureReason != "tool outcome requires reconciliation before continuing" {
		t.Fatal("unconfirmed tool outcome was not blocked")
	}
}
