package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

type teamDispatchPoolStore struct {
	loom.Store
	pool *pgxpool.Pool
}

func (s teamDispatchPoolStore) Pool() *pgxpool.Pool { return s.pool }

func newTeamDispatchTestServer(t *testing.T) (*Server, *pgxpool.Pool) {
	return newTeamDispatchTestServerWithGraph(t, json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`))
}

func newTeamDispatchTestServerWithGraph(t *testing.T, graph json.RawMessage) (*Server, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	trigger := json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	if _, report := machine.DecodeTriggerConfigV1(trigger); report != nil {
		t.Fatalf("fixture trigger: %+v", report.Issues)
	}
	if _, report := machine.DecodeGraphDefinitionV1(graph); report != nil {
		t.Fatalf("fixture graph: %+v", report.Issues)
	}
	artifact := frozen.ArtifactPayloadV1{
		SchemaVersion: 1, TriggerConfig: trigger, GraphDefinition: graph,
		Team:    frozen.ArtifactTeamV1{WorkspaceID: "ws", TeamID: "team", LeadAgentID: "lead", LeadAgentVersion: 1, LeadAgentContentHash: strings.Repeat("a", 64)},
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
	if _, err := pool.Exec(ctx, `
 INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','unused','admin'),('user-other','ws','user-other','unused','member'),('other-user','ws','other-user','unused','member'),('user-a','ws','user-a','unused','member'),('user-b','ws','user-b','unused','member');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('lead','ws','lead','avatar','{}');
 INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','ws','team','lead','active');
 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES('ws','flow','team','flow');
 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
 VALUES('ws','flow',1,'draft',$1::jsonb,$2::jsonb,'user');
 UPDATE weave_team_workflow_versions SET status='published',published_at=now() WHERE workspace_id='ws' AND workflow_id='flow';
 INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload)
 VALUES('ws','flow',1,1,'rfc8785+jcs-preorder',1,'sha256',$3,$4::jsonb);
 INSERT INTO weave_workflow_version_admission_statuses(workspace_id,workflow_id,workflow_version,blocked) VALUES('ws','flow',1,false);
 UPDATE weave_team_workflows SET published_version=1 WHERE workspace_id='ws' AND id='flow';
 UPDATE weave_teams SET default_workflow_id='flow' WHERE workspace_id='ws' AND id='team';
 `, string(trigger), string(graph), digest, string(payload)); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: teamDispatchPoolStore{pool: pool}, OrgStore: orgstore.NewStore(pool), Registry: agentcatalog.New(pool),
		Workflow: workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil)), WorkflowArtifacts: workflow.NewArtifactStore(pool, nil), Deliverables: deliveryverify.NewStore(pool), ScheduleTransactions: pool, Snapshots: snapshot.NewStore(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, teamconstruction.NewPublicationAuthority(pool, nil))
	return server, pool
}

func TestTeamWorkflowDispatchKeepsInputIdentityAndAdmissionRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	input := teamDispatchRequest{Task: "甲：三条材料。\n乙：保留换行与“引号”。", ClientRequestID: "00000000-0000-0000-0000-000000000001"}
	dispatch := func(request teamDispatchRequest, wantStatus int) workflowManualRunResponse {
		t.Helper()
		body, _ := json.Marshal(request)
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(http.MethodPost, "/v1/teams/team/dispatch", bytes.NewReader(body))
		httpRequest = httpRequest.WithContext(execution.WithSubject(httpRequest.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
		c := echo.New().NewContext(httpRequest, recorder)
		c.SetPath("/v1/teams/:id/dispatch")
		c.SetParamNames("id")
		c.SetParamValues("team")
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		if err := server.handleDispatchTeam(c); err != nil || recorder.Code != wantStatus {
			t.Fatalf("dispatch status=%d want=%d body=%s err=%v", recorder.Code, wantStatus, recorder.Body.String(), err)
		}
		if c.Path() != "/v1/teams/:id/dispatch" || c.Param("id") != "team" {
			t.Fatal("team dispatch rewrote the request into a retired workflow endpoint")
		}
		var result workflowManualRunResponse
		if wantStatus < 300 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	first := dispatch(input, http.StatusCreated)
	if replay := dispatch(input, http.StatusOK); replay != first {
		t.Fatalf("replay changed identity: %+v != %+v", replay, first)
	}
	task, err := server.Tasks.Get(ctx, "ws", first.TaskID)
	var taskText string
	if err != nil || json.Unmarshal(task.Payload, &taskText) != nil || taskText != input.Task || task.RunSnapshotID != first.RunID {
		t.Fatalf("task lost original input or run identity: %+v %v", task, err)
	}
	// A Workbench request may name a conversation while omitting its project.
	// Admission derives the project, and an identical replay must keep that identity.
	if _, err := pool.Exec(ctx, `
 INSERT INTO weave_projects(id,workspace_id,avatar_id,name,team_id) VALUES('project','ws','lead','Project','team');
 INSERT INTO weave_conversations(id,workspace_id,agent_id,user_id,project_id) VALUES('conversation','ws','lead','user','project');
 `); err != nil {
		t.Fatal(err)
	}
	server.Projects = projects.New(pool, nil)
	conversationInput := input
	conversationInput.ConversationID = "conversation"
	conversationInput.ClientRequestID = "00000000-0000-0000-0000-000000000003"
	attributed := dispatch(conversationInput, http.StatusCreated)
	if attributed.ProjectID != "project" {
		t.Fatalf("inferred project lost: %+v", attributed)
	}
	if replay := dispatch(conversationInput, http.StatusOK); replay != attributed {
		t.Fatalf("conversation replay changed: %+v", replay)
	}
	mismatched := conversationInput
	mismatched.ClientRequestID = "00000000-0000-0000-0000-000000000004"
	mismatched.ProjectID = "different-project"
	dispatch(mismatched, http.StatusConflict)
	changed := input
	changed.Task = "changed"
	dispatch(changed, http.StatusConflict)
	if _, err := pool.Exec(ctx, `UPDATE weave_workflow_version_admission_statuses SET blocked=true WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	dispatch(input, http.StatusOK)
	input.ClientRequestID = "00000000-0000-0000-0000-000000000002"
	dispatch(input, http.StatusConflict)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("replay or rejected admission queued more work: %d %v", count, err)
	}
}
