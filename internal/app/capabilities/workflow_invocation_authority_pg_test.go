package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

type workflowInvocationTestResolver struct{}

func (workflowInvocationTestResolver) ResolvePublished(context.Context, string, string, *int) (frozen.ArtifactEnvelopeV1, error) {
	return frozen.ArtifactEnvelopeV1{}, errors.New("unexpected workflow resolution")
}

type workflowInvocationTestPublisher struct{}

func (workflowInvocationTestPublisher) AdmitPublished(context.Context, publication.PublishedRunRequest) (publication.AdmissionReceipt, error) {
	return publication.AdmissionReceipt{}, errors.New("unexpected workflow admission")
}

func TestWorkflowRuntimeCredentialRechecksApplicationGrantRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	definitions := NewService(store, store)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workspaces(id,slug,name) VALUES('workflow-auth','workflow-auth','Workflow Auth');
	 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('workflow-auth-user','workflow-auth','owner','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	lead := &registry.AgentRecord{Name: "workflow-auth-lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
	if err := agentcatalog.New(pool).Put(t.Context(), "workflow-auth", lead); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)
	 VALUES('workflow-auth-team','workflow-auth','Workflow Auth Team',$1,'active');
	 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name,status)
	 VALUES('workflow-auth','flow','workflow-auth-team','Flow','active');
	 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
	 VALUES('workflow-auth','flow',7,'draft','{"schema_version":1,"type":"conversation_explicit","config":{}}',
	 '{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"json"},"output_contract":{"type":"json","schema":{"type":"object"}},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}','workflow-auth-user');
	 UPDATE weave_team_workflow_versions SET status='published',published_at=now()
	 WHERE workspace_id='workflow-auth' AND workflow_id='flow' AND version=7;
	 UPDATE weave_team_workflows SET published_version=7 WHERE workspace_id='workflow-auth' AND id='flow'`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_published_artifact_contents(
	 workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,
	 canonicalization_version,hash_algorithm,content_hash,payload)
	 VALUES('workflow-auth','flow',7,1,'rfc8785+jcs-preorder',1,'sha256',repeat('a',64),'{}')`); err != nil {
		t.Fatal(err)
	}
	definition := capability.Definition{SchemaVersion: 1, CapabilityID: "workflow-cap", Name: "Workflow capability",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Workflow: &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 7}}
	if err := definitions.SaveDraft(t.Context(), DraftRequest{WorkspaceID: "workflow-auth", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	published, err := definitions.Publish(t.Context(), "workflow-auth", definition.CapabilityID, 1)
	if err != nil {
		t.Fatal(err)
	}
	access := NewAccessStore(pool)
	app, err := access.CreateApplication(t.Context(), "workflow-auth", "workflow-auth-user", "invoker")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := access.IssueCredential(t.Context(), "workflow-auth", "workflow-auth-user", app.ID, "invoker key", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	grant := VersionGrant{AppID: app.ID, CapabilityID: definition.CapabilityID, Revision: 1, Enabled: true}
	if err := access.SetGrant(t.Context(), "workflow-auth", "workflow-auth-user", grant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,actor_subject,request_digest,request,target)
	 VALUES('workflow-auth','admission','{"workspace_id":"workflow-auth","user_id":"workflow-auth-user"}',repeat('a',64),'{}','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_capability_workflow_invocations(
	 workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,admission_request_id,run_id,task_id)
	 VALUES('workflow-auth',$1,'request','11111111-1111-4111-8111-111111111111',$2,'workflow-cap',1,$3,
	 'flow',7,'{}'::jsonb,$4,'admission','run-1','task-1')`,
		app.ID, credential.ID, published.DefinitionHash, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	subject := execution.Subject{WorkspaceID: "workflow-auth", ServiceID: "capability-app:" + app.ID}
	ctx := execution.WithSubject(t.Context(), subject)
	tasks := taskqueue.New(pool, nil, 0)
	encodedSubject, err := json.Marshal(subject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_run_snapshots(
	 run_id,workspace_id,team_id,workflow_id,workflow_version,worker_versions,team_worker_snapshot,
	 artifact_workflow_id,artifact_workflow_version,admission_decision,run_associations,trigger_source,
	 snapshot_schema_version,mode,trigger_source_v2,actor_subject)
	 VALUES('run-1','workflow-auth','workflow-auth-team','flow',7,NULL,NULL,'flow',7,
	 '{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-10-15T10:00:00Z"}',
	 '{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}',
	 'conversation_explicit',2,'fixed_workflow',
	 '{"schema_version":1,"type":"conversation_explicit","source_ref":"workflow-auth-test"}',$1::jsonb)`, string(encodedSubject)); err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{WorkspaceID: "workflow-auth", ID: "task-1", IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: "flow", WorkflowVersion: 7, RunSnapshotID: "run-1",
		Source: "manual", Kind: "team_workflow", Subject: subject, Payload: json.RawMessage(`{}`)}
	if err := tasks.Enqueue(ctx, task); err != nil {
		t.Fatalf("enqueue authoritative invocation task: %v", err)
	}
	service := NewWorkflowInvocationService(pool, definitions, workflowInvocationTestResolver{}, workflowInvocationTestPublisher{}, tasks, nil)
	run := teamrun.TeamRun{WorkspaceID: "workflow-auth", RunID: "run-1", RunSnapshotID: "run-1",
		SourceTaskID: task.ID, WorkflowID: "flow", WorkflowVersion: 7}
	ref := frozen.CredentialReference{SchemaVersion: 1, Scope: frozen.CredentialScopeWorkspaceService, ServiceID: "provider:provider",
		WorkspaceID: run.WorkspaceID, Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key"}
	if err := service.AuthorizeRuntimeCredential(ctx, run, task, subject, ref); err != nil {
		t.Fatalf("active exact application grant rejected provider resolution: %v", err)
	}
	personalRef := ref
	personalRef.Scope, personalRef.ServiceID, personalRef.UserID = frozen.CredentialScopeUser, "", "workflow-auth-user"
	if err := service.AuthorizeRuntimeCredential(ctx, run, task, subject, personalRef); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("application grant authorized an employee-owned provider credential: %v", err)
	}
	wrongTask := *task
	wrongTask.ID = "different-task"
	if err := service.AuthorizeRuntimeCredential(ctx, run, &wrongTask, subject, ref); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("application provider grant crossed to another task: %v", err)
	}
	grant.Enabled = false
	if err := access.SetGrant(t.Context(), "workflow-auth", "workflow-auth-user", grant); err != nil {
		t.Fatal(err)
	}
	if err := service.AuthorizeRuntimeCredential(ctx, run, task, subject, ref); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("runtime credential remained available after grant revocation: %v", err)
	}
	other := execution.Subject{WorkspaceID: run.WorkspaceID, ServiceID: "capability-app:another-app"}
	if err := service.AuthorizeRuntimeCredential(execution.WithSubject(t.Context(), other), run, task, other, ref); !errors.Is(err, frozen.ErrCredentialSubjectDenied) {
		t.Fatalf("another application reused a provider credential grant: %v", err)
	}
}
