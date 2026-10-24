package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// runtimeCredentialFixture is one published capability run of a team with a
// lead and one worker, resolving its frozen provider credential the way a
// model call does: through the pool credential resolver, whose transaction
// the runtime authorization reuses.
type runtimeCredentialFixture struct {
	pool          *pgxpool.Pool
	access        *appcapabilities.AccessStore
	providers     *credentials.Store
	grant         appcapabilities.VersionGrant
	appID         string
	credentialID  string
	workerID      string
	ref           frozen.CredentialReference
	call          func() error
	authorization *appcapabilities.RuntimeCredentialAuthorizations
}

const runtimeCredentialWorkspace = "runtime-revocation"

func newRuntimeCredentialFixture(t *testing.T) *runtimeCredentialFixture {
	t.Helper()
	pool := testutil.PostgresPool(t)
	ws := runtimeCredentialWorkspace
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, UserID: "alice"})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role)VALUES('alice',$1,'alice','unused','admin');
	 INSERT INTO weave_workspaces(id,slug,name)VALUES($1,$1,'Runtime revocation')`, ws); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	provider := llmrouter.ProviderConfig{ID: "provider", Name: "Provider", BaseURL: "https://fixture.invalid/v1",
		APIKey: "fixture-secret", Models: []string{"fixture"}, CredentialScope: frozen.CredentialScopeWorkspaceService,
		CredentialServiceID: "provider:provider"}
	if err := providers.Upsert(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, ServiceID: provider.CredentialServiceID}), ws, provider); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
	worker := &registry.AgentRecord{Name: "worker", Role: "worker", Engine: "loom", Model: "fixture", GraphType: "standard"}
	for _, record := range []*registry.AgentRecord{lead, worker} {
		if err := agents.Put(ctx, ws, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)VALUES('team',$1,'Team',$2,'active')`, ws, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := agentcatalog.NewTeamWorkerRepository(pool).Create(ctx, ws, registry.TeamWorker{TeamID: "team", WorkerAgentID: worker.ID,
		Duty: "Review", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "Review", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	graph := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"entry_node_id":"brief","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"brief","type":"lead","config":{"instruction":"Brief"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"review","type":"worker","config":{"kind":"consult","agent_id":%q,"agent_version":%d,"result_requirement":"Review"},"inputs":{"task":{"value":{"source":"node_output","node_id":"brief","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"review","path":""}}}],"edges":[{"id":"a","from_node_id":"brief","to_node_id":"review","route":"success"},{"id":"b","from_node_id":"review","to_node_id":"deliver","route":"success"}]}`, worker.ID, worker.Version))
	flows := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: ws, TeamID: "team", Name: "Flow"}, workflow.DraftInput{
		CreatedBy: "alice", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: graph})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := compiler.NewDescriptorRegistry()
	if err := descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
		t.Fatal(err)
	}
	builder := workflowcatalog.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), descriptors)
	authority := NewPublicationAuthority(pool, builder)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: ws, WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		_ = tx.Rollback(ctx)
		t.Fatalf("build candidate: report=%+v err=%v", report, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	kernel, err := publicationservice.Open(ctx, connection.String(), authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernel.Close)
	command, err := PublicationCommandForCandidate("runtime-revocation", candidate, PublicationTarget{TeamID: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := NewProductPublication(pool, kernel, authority.AuthorizeProduct).Publish(ctx, command); err != nil || receipt.State != PublicationActivated {
		t.Fatalf("publish: %+v %v", receipt, err)
	}
	var ref frozen.CredentialReference
	for _, bundle := range candidate.Payload.Bundles {
		for _, candidateRef := range bundle.Credentials {
			if candidateRef.Kind == frozen.CredentialProviderAPIKey {
				ref = candidateRef
			}
		}
	}
	if ref.ResourceID != "provider" || ref.Scope != frozen.CredentialScopeWorkspaceService {
		t.Fatalf("published provider reference = %+v", ref)
	}

	definitions := appcapabilities.NewService(appcapabilities.NewPGStore(pool))
	definition := machineCapabilityDefinition("flow", draft.Version)
	if err := definitions.SaveDraft(ctx, appcapabilities.DraftRequest{WorkspaceID: ws, Definition: definition}); err != nil {
		t.Fatal(err)
	}
	published, err := definitions.Publish(ctx, ws, definition.CapabilityID, 1)
	if err != nil {
		t.Fatal(err)
	}
	access := appcapabilities.NewAccessStore(pool)
	app, err := access.CreateApplication(ctx, ws, "alice", "caller")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := access.IssueCredential(ctx, ws, "alice", app.ID, "caller key", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	grant := appcapabilities.VersionGrant{AppID: app.ID, CapabilityID: definition.CapabilityID, Revision: 1, Enabled: true}
	if err := access.SetGrant(ctx, ws, "alice", grant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,actor_subject,request_digest,request,target)
	 VALUES($1,'admission','{"workspace_id":"runtime-revocation","user_id":"alice"}',repeat('a',64),'{}','{}');
	 INSERT INTO weave_capability_workflow_invocations(
	 workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,admission_request_id,run_id,task_id)
	 VALUES($1,$2,'request','11111111-1111-4111-8111-111111111111',$3,$4,1,$5,'flow',$6,'{}'::jsonb,repeat('a',64),'admission','run-1','task-1')`,
		ws, app.ID, credential.ID, definition.CapabilityID, published.DefinitionHash, draft.Version); err != nil {
		t.Fatal(err)
	}
	subject := execution.Subject{WorkspaceID: ws, ServiceID: "capability-app:" + app.ID}
	encodedSubject, err := json.Marshal(subject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_run_snapshots(
	 run_id,workspace_id,team_id,workflow_id,workflow_version,worker_versions,team_worker_snapshot,
	 artifact_workflow_id,artifact_workflow_version,admission_decision,run_associations,trigger_source,
	 snapshot_schema_version,mode,trigger_source_v2,actor_subject)
	 VALUES('run-1',$1,'team','flow',$2,NULL,NULL,'flow',$2,
	 '{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-10-23T10:00:00Z"}',
	 '{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}',
	 'conversation_explicit',2,'fixed_workflow',
	 '{"schema_version":1,"type":"conversation_explicit","source_ref":"runtime-revocation"}',$3::jsonb)`, ws, draft.Version, string(encodedSubject)); err != nil {
		t.Fatal(err)
	}
	subjectContext := execution.WithSubject(context.Background(), subject)
	tasks := taskqueue.New(pool, nil, 0)
	task := &taskqueue.Task{WorkspaceID: ws, ID: "task-1", IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: "flow", WorkflowVersion: draft.Version, RunSnapshotID: "run-1",
		Source: "manual", Kind: "team_workflow", Subject: subject, Payload: json.RawMessage(`{}`)}
	if err := tasks.Enqueue(subjectContext, task); err != nil {
		t.Fatal(err)
	}
	authorizations := &appcapabilities.RuntimeCredentialAuthorizations{Team: authority}
	service := appcapabilities.NewWorkflowInvocationService(pool, definitions, unusedWorkflowResolver{}, unusedWorkflowPublisher{}, tasks, nil)
	service.UseRuntimeAuthorizations(authorizations)
	run := teamrun.TeamRun{WorkspaceID: ws, RunID: "run-1", RunSnapshotID: "run-1", TeamID: "team",
		SourceTaskID: task.ID, WorkflowID: "flow", WorkflowVersion: draft.Version}
	unavailable := func(context.Context, pgx.Tx, frozen.CredentialReference) error { return errors.New("unused") }
	resolver, err := credentials.NewPoolCredentialResolver(pool, ws, credentials.TxCredentialSources{
		ProviderGate: providers.ValidateReferenceTx, ProviderResolve: providers.ResolveProviderAPIKeyTx,
		MCPGate: unavailable, RuntimeGate: unavailable, DeliveryGate: unavailable,
		MCPResolve: func(context.Context, pgx.Tx, frozen.CredentialReference) (credentials.SecretMaterial, error) {
			return credentials.SecretMaterial{}, errors.New("unused")
		},
		RuntimeResolve: func(context.Context, pgx.Tx, frozen.CredentialReference, string) (credentials.SecretMaterial, error) {
			return credentials.SecretMaterial{}, errors.New("unused")
		},
		DeliveryResolve: func(context.Context, pgx.Tx, frozen.CredentialReference) (credentials.SecretMaterial, error) {
			return credentials.SecretMaterial{}, errors.New("unused")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	callContext := frozen.WithServiceReferenceAuthorization(subjectContext, func(ctx context.Context, subject execution.Subject, ref frozen.CredentialReference) error {
		return service.AuthorizeRuntimeCredential(ctx, run, task, subject, ref)
	})
	return &runtimeCredentialFixture{pool: pool, access: access, providers: providers, grant: grant, appID: app.ID,
		credentialID: credential.ID, workerID: worker.ID, ref: ref, authorization: authorizations,
		call: func() error {
			material, err := resolver.Resolve(callContext, credentials.ResolveRequest{Reference: ref})
			if err == nil && string(material.Value()) != "fixture-secret" {
				return errors.New("resolved another secret")
			}
			return err
		}}
}

type unusedWorkflowResolver struct{}

func (unusedWorkflowResolver) ResolvePublished(context.Context, string, string, *int) (frozen.ArtifactEnvelopeV1, error) {
	return frozen.ArtifactEnvelopeV1{}, errors.New("unexpected workflow resolution")
}

type unusedWorkflowPublisher struct{}

func (unusedWorkflowPublisher) AdmitPublished(context.Context, publication.PublishedRunRequest) (publication.AdmissionReceipt, error) {
	return publication.AdmissionReceipt{}, errors.New("unexpected workflow admission")
}

// TestRuntimeCredentialRefusesTheNextCallAfterEachRevocationRealPG checks
// every change that withdraws a published capability run's authorization:
// the attempt's later calls reuse its first full check while nothing moved,
// and the call after the change is refused.
func TestRuntimeCredentialRefusesTheNextCallAfterEachRevocationRealPG(t *testing.T) {
	ws := runtimeCredentialWorkspace
	for _, revocation := range []struct {
		name   string
		revoke func(*testing.T, *runtimeCredentialFixture)
	}{
		{"application grant", func(t *testing.T, f *runtimeCredentialFixture) {
			grant := f.grant
			grant.Enabled = false
			if err := f.access.SetGrant(context.Background(), ws, "alice", grant); err != nil {
				t.Fatal(err)
			}
		}},
		{"application credential", func(t *testing.T, f *runtimeCredentialFixture) {
			if err := f.access.RevokeCredential(context.Background(), ws, "alice", f.appID, f.credentialID); err != nil {
				t.Fatal(err)
			}
		}},
		{"application disabled", func(t *testing.T, f *runtimeCredentialFixture) {
			if err := f.access.SetApplicationEnabled(context.Background(), ws, "alice", f.appID, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"team member disabled", func(t *testing.T, f *runtimeCredentialFixture) {
			f.exec(t, `UPDATE weave_team_workers SET enabled=false WHERE workspace_id=$1 AND worker_agent_id=$2`, ws, f.workerID)
		}},
		{"team members changed", func(t *testing.T, f *runtimeCredentialFixture) {
			// Exact member versions are immutable; what changes is which
			// members the team has. With another lead the published
			// version's members are no longer the team's.
			other := &registry.AgentRecord{Name: "other lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
			if err := agentcatalog.New(f.pool).Put(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, UserID: "alice"}), ws, other); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE weave_teams SET lead_avatar_id=$2 WHERE workspace_id=$1 AND id='team'`, ws, other.ID)
		}},
		{"provider revoked", func(t *testing.T, f *runtimeCredentialFixture) {
			f.exec(t, `UPDATE weave_provider_credentials SET revoked_at=now() WHERE workspace_id=$1 AND id='provider'`, ws)
		}},
		{"provider deleted", func(t *testing.T, f *runtimeCredentialFixture) {
			if err := f.providers.Delete(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: ws, ServiceID: "provider:provider"}), ws, "provider"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(revocation.name, func(t *testing.T) {
			f := newRuntimeCredentialFixture(t)
			if err := f.call(); err != nil {
				t.Fatalf("first call of the attempt: %v", err)
			}
			if err := f.call(); err != nil {
				t.Fatalf("second call of the attempt: %v", err)
			}
			revocation.revoke(t, f)
			if err := f.call(); err == nil {
				t.Fatal("the call after the revocation resolved the provider credential")
			}
		})
	}
}

// TestRuntimeCredentialReusesTheAttemptsCheckUntilAVersionMovesRealPG shows
// that a later call does not repeat the full check: a change made without
// moving a version (triggers bypassed) is not seen, and moving the version
// makes the next call check in full again.
func TestRuntimeCredentialReusesTheAttemptsCheckUntilAVersionMovesRealPG(t *testing.T) {
	ws := runtimeCredentialWorkspace
	f := newRuntimeCredentialFixture(t)
	if err := f.call(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	conn, err := f.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `UPDATE weave_capability_grants SET enabled=false WHERE workspace_id=$1 AND app_id=$2`, ws, f.appID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `RESET session_replication_role`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if err := f.call(); err != nil {
		t.Fatalf("a later call repeated the full check although no version moved: %v", err)
	}
	f.exec(t, `SELECT weave_bump_authorization_epoch($1,'capability_app',$2)`, ws, f.appID)
	if err := f.call(); err == nil {
		t.Fatal("the call after the version moved resolved the credential of a revoked grant")
	}
}

func (f *runtimeCredentialFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
