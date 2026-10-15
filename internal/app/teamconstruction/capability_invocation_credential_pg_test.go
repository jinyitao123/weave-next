package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestPublishedCapabilityAppRechecksExactProviderGrantRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "app-provider", UserID: "alice"})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role)VALUES('alice','app-provider','alice','unused','admin');
	 INSERT INTO weave_workspaces(id,slug,name)VALUES('app-provider','app-provider','App Provider')`); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	provider := llmrouter.ProviderConfig{ID: "provider", Name: "Provider", BaseURL: "https://fixture.invalid/v1",
		APIKey: "fixture-secret", Models: []string{"fixture"}, CredentialScope: frozen.CredentialScopeWorkspaceService,
		CredentialServiceID: "provider:provider"}
	providerContext := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "app-provider", ServiceID: provider.CredentialServiceID})
	if err := providers.Upsert(providerContext, "app-provider", provider); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
	if err := agents.Put(ctx, "app-provider", lead); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)VALUES('team','app-provider','Team',$1,'building')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	flows := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: "app-provider", TeamID: "team", Name: "Flow"}, workflow.DraftInput{
		CreatedBy: "alice", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"lead","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"lead","type":"lead","config":{"instruction":"Review"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"lead","path":""}}}],"edges":[{"id":"done","from_node_id":"lead","to_node_id":"deliver","route":"success"}]}`),
	})
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
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "app-provider", WorkflowID: "flow", WorkflowVersion: draft.Version})
	_ = tx.Rollback(ctx)
	if err != nil || candidate == nil || report == nil || len(report.Issues) != 0 {
		t.Fatalf("build credential-bound workflow candidate: report=%+v err=%v", report, err)
	}
	envelope, err := workflow.CandidateEnvelope(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var providerRef frozen.CredentialReference
	for _, bundle := range candidate.Payload.Bundles {
		for _, ref := range bundle.Credentials {
			if ref.Kind == frozen.CredentialProviderAPIKey {
				providerRef = ref
			}
		}
	}
	if providerRef.ResourceID == "" {
		t.Fatal("workflow candidate has no frozen provider reference")
	}
	if providerRef.Scope != frozen.CredentialScopeWorkspaceService || providerRef.ServiceID != "provider:provider" {
		t.Fatalf("published app provider credential is not shared service-scoped: %+v", providerRef)
	}

	definitionsStore := appcapabilities.NewPGStore(pool)
	definitions := appcapabilities.NewService(definitionsStore, definitionsStore)
	capabilityDefinition := machineCapabilityDefinition(envelope.WorkflowID, envelope.WorkflowVersion)
	if err := definitions.SaveDraft(ctx, appcapabilities.DraftRequest{WorkspaceID: "app-provider", Definition: capabilityDefinition}); err != nil {
		t.Fatal(err)
	}
	capabilityRevision, err := definitions.Publish(ctx, "app-provider", capabilityDefinition.CapabilityID, 1)
	if err != nil {
		t.Fatal(err)
	}
	access := appcapabilities.NewAccessStore(pool)
	app, err := access.CreateApplication(ctx, "app-provider", "alice", "Workflow caller")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := access.IssueCredential(ctx, "app-provider", "alice", app.ID, "Workflow caller key", []string{"invoke", "read", "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	grant := appcapabilities.VersionGrant{AppID: app.ID, CapabilityID: capabilityDefinition.CapabilityID, Revision: 1, Enabled: true}
	if err := access.SetGrant(ctx, "app-provider", "alice", grant); err != nil {
		t.Fatal(err)
	}
	invocationID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	inputHash := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_capability_workflow_invocations(
	 workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,admission_request_id,run_id,task_id)
	 VALUES('app-provider',$1,'app-request',$2,$3,$4,1,$5,$6,$7,'{}'::jsonb,$8,$9,'app-run','app-task')`,
		app.ID, invocationID, credential.ID, capabilityDefinition.CapabilityID, capabilityRevision.DefinitionHash,
		envelope.WorkflowID, envelope.WorkflowVersion, inputHash, "admission-"+invocationID); err != nil {
		t.Fatal(err)
	}
	request := publication.PublishedRunRequest{Version: publication.ContractVersion, RequestID: "admission-" + invocationID,
		Revision: publication.CandidateRevision(envelope), RunID: "app-run", TaskID: "app-task",
		Input: json.RawMessage(`"{}"`), InputVersion: "sha256:" + inputHash, ContextKey: invocationID,
		Trigger: publication.PublishedTrigger{Type: "manual", SourceRef: app.ID}}
	serviceSubject := execution.Subject{WorkspaceID: "app-provider", ServiceID: "capability-app:" + app.ID}
	serviceContext := execution.WithSubject(context.Background(), serviceSubject)
	if err := authority.AuthorizePublishedCredential(serviceContext, request, envelope, providerRef); err != nil {
		t.Fatalf("authorized application could not resolve exact frozen provider credential: %v", err)
	}
	personalRef := providerRef
	personalRef.Scope, personalRef.UserID, personalRef.ServiceID = frozen.CredentialScopeUser, "alice", ""
	if err := authority.AuthorizePublishedCredential(serviceContext, request, envelope, personalRef); err == nil {
		t.Fatal("application grant expanded to an employee-owned provider credential")
	}
	otherRef := providerRef
	otherRef.ResourceID = "another-provider"
	if err := authority.AuthorizePublishedCredential(serviceContext, request, envelope, otherRef); err == nil {
		t.Fatal("application grant expanded to an unfrozen provider credential")
	}
	grant.Enabled = false
	if err := access.SetGrant(ctx, "app-provider", "alice", grant); err != nil {
		t.Fatal(err)
	}
	if err := authority.AuthorizePublishedCredential(serviceContext, request, envelope, providerRef); !errors.Is(err, appcapabilities.ErrAccessDenied) {
		t.Fatalf("revoked application grant still resolved provider credential: %v", err)
	}
}

func machineCapabilityDefinition(workflowID string, workflowVersion int) capability.Definition {
	return capability.Definition{
		SchemaVersion: 1, CapabilityID: "provider-cap", Name: "Provider capability",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Workflow: &capability.WorkflowReference{WorkflowID: workflowID, WorkflowVersion: workflowVersion},
	}
}
