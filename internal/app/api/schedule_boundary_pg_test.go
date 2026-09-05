package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestWorkflowScheduleSkipsRetiredAgentAndKeepsAdmissionRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	trigger := json.RawMessage(`{"schema_version":1,"type":"schedule","config":{"schedule_id":"team-schedule"},"delivery":{"kind":"job_record"}}`)
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"json"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"literal","value":"saved result"}}}],"edges":[]}`)
	artifact := frozen.ArtifactPayloadV1{
		SchemaVersion: 1, TriggerConfig: trigger, GraphDefinition: graph,
		Team:    frozen.ArtifactTeamV1{WorkspaceID: "ws", TeamID: "team", LeadAgentID: "lead"},
		Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	digest, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: 1, ArtifactSchemaVersion: 1,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1,
		HashAlgorithm: frozen.ArtifactHashAlgorithm, Payload: artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := frozen.Canonicalize(artifact, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	// Only this test's temporary schema is changed. Disable the timezone trigger
	// while seeding a damaged legacy row, then restore it before exercising code.
	if _, err := pool.Exec(ctx, `
 INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('lead','ws','lead','avatar','{}');
 INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','ws','team','lead','active');
 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES('ws','flow','team','flow');
 INSERT INTO weave_schedule(id,workspace_id,target_kind,agent,message,target_workflow_id,kind,run_at,timezone)
 VALUES('team-schedule','ws','team_workflow',NULL,NULL,'flow','once',$5,'UTC');
 ALTER TABLE weave_schedule DISABLE TRIGGER weave_schedule_timezone_guard;
 INSERT INTO weave_schedule(id,workspace_id,target_kind,agent,message,kind,time_of_day,timezone)
 VALUES('old-agent','ws','agent','lead','old work','daily','00:00','Legacy/Missing');
 ALTER TABLE weave_schedule ENABLE TRIGGER weave_schedule_timezone_guard;
 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
 VALUES('ws','flow',1,'draft',$1::jsonb,$2::jsonb,'user');
 UPDATE weave_team_workflow_versions SET status='published',published_at=now() WHERE workspace_id='ws' AND workflow_id='flow';
 INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload)
 VALUES('ws','flow',1,1,'rfc8785+jcs-preorder',1,'sha256',$3,$4::jsonb);
 INSERT INTO weave_workflow_version_admission_statuses(workspace_id,workflow_id,workflow_version,blocked) VALUES('ws','flow',1,false);
 UPDATE weave_team_workflows SET published_version=1 WHERE workspace_id='ws' AND id='flow';
 `, string(trigger), string(graph), digest, string(payload), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	legacyState := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT row_to_json(s)::text FROM weave_schedule s WHERE id='old-agent'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := legacyState()
	store := schedule.New(pool, nil)
	due, err := store.DueSchedules(ctx, now)
	if err != nil || len(due) != 1 || due[0].ID != "team-schedule" {
		t.Fatalf("legacy row affected team scheduling: due=%+v err=%v", due, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, lockErr := store.LockDueTx(ctx, tx, "ws", "old-agent", now)
	_ = tx.Rollback(ctx)
	if lockErr == nil {
		t.Fatal("retired agent schedule can still acquire an execution lock")
	}
	workflowStore := workflow.New(pool, nil)
	server := &Server{AgentSchedules: store, WorkflowScheduleAdmission: NewWorkflowScheduleAdmissionService(workflowStore),
		ScheduleTransactions: pool, Snapshots: snapshot.NewStore(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	for range 2 {
		if err := server.SweepSchedules(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	var tasks, occurrences int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_schedule_occurrences WHERE status='committed'`).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	var kind, workflowID string
	var identity taskqueue.IdentityKind
	if err := pool.QueryRow(ctx, `SELECT kind,identity_kind,workflow_id FROM weave_task_queue`).Scan(&kind, &identity, &workflowID); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || occurrences != 1 || kind != "team_workflow" || identity != taskqueue.IdentityTeamWorkflow || workflowID != "flow" {
		t.Fatalf("workflow scheduling lost admission or replay safety: tasks=%d occurrences=%d kind=%s identity=%s workflow=%s", tasks, occurrences, kind, identity, workflowID)
	}
	if after := legacyState(); after != before {
		t.Fatal("sweep changed the retired agent schedule's stored history")
	}
}
