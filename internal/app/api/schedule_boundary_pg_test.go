package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func scheduledAdmissionFixture(t *testing.T) (*Server, *pgxpool.Pool, time.Time, func() string) {
	t.Helper()
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
	workflowStore := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, Workflow: workflowStore, KernelPublication: openAPIKernelPublication(t, ctx, pool, teamconstruction.NewPublicationAuthority(pool, nil)), AgentSchedules: store, WorkflowScheduleAdmission: NewWorkflowScheduleAdmissionService(workflowStore, workflow.NewArtifactStore(pool, nil)),
		ScheduleTransactions: pool, Snapshots: snapshot.NewStore(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	return server, pool, now, legacyState
}

func TestWorkflowScheduleSkipsRetiredAgentAndKeepsAdmissionRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool, now, legacyState := scheduledAdmissionFixture(t)
	before := legacyState()
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

func TestScheduleRecoversPreparedIntentAndUnknownKernelCommitRealPG(t *testing.T) {
	for _, lostKernelResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "product-prepared", true: "kernel-accepted"}[lostKernelResponse], func(t *testing.T) {
			ctx := context.Background()
			server, pool, now, _ := scheduledAdmissionFixture(t)
			real := server.KernelPublication
			if lostKernelResponse {
				wrapper := &ambiguousPublishedKernel{Service: real, published: real.(publication.PublishedService), reconciler: real.(publication.PublishedReconciler)}
				wrapper.lose.Store(true)
				server.KernelPublication = wrapper
			} else {
				server.WorkflowScheduleStepHook = func(_ context.Context, stage WorkflowScheduleStage) error {
					if stage == WorkflowScheduleStageCommitAfter {
						return errors.New("process stopped after preparing occurrence")
					}
					return nil
				}
			}
			if err := server.SweepSchedules(ctx, now); err == nil {
				t.Fatal("interruption did not reach caller")
			}
			var tasks, pending int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_schedule_occurrences WHERE status='pending')`).Scan(&tasks, &pending); err != nil {
				t.Fatal(err)
			}
			want := 0
			if lostKernelResponse {
				want = 1
			}
			if tasks != want || pending != 1 {
				t.Fatalf("interrupted state %d/%d", tasks, pending)
			}
			// Recreate the product adapter with only durable request/occurrence state.
			server.WorkflowScheduleStepHook = nil
			server.KernelPublication = real
			if err := server.SweepSchedules(ctx, now); err != nil {
				t.Fatal(err)
			}
			var committed int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_schedule_occurrences WHERE status='committed')`).Scan(&tasks, &committed); err != nil || tasks != 1 || committed != 1 {
				t.Fatalf("schedule replay %d/%d %v", tasks, committed, err)
			}
		})
	}
}
