package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

func TestServiceAppCredentialsReleaseAndGrantRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "capability-contract-" + uuid.NewString()
	admin, err := users.NewStore(pool).Create(ctx, workspaceID, "admin", "password", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	store := New(pool, fixedClock{now: now})
	app, err := store.CreateApp(ctx, CreateAppRequest{
		WorkspaceID: workspaceID, Name: "order-checker", Description: "Checks order material",
		MaxConcurrentInvocations: 4, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, firstRaw, err := store.CreateCredential(ctx, CreateCredentialRequest{
		WorkspaceID: workspaceID, AppID: app.ID, Name: "initial",
		Scopes: []string{"read", "invoke", "cancel"}, CreatedBy: admin.ID,
	})
	if err != nil || !strings.HasPrefix(firstRaw, credentialPrefix) {
		t.Fatalf("create first credential: credential=%+v error=%v", first, err)
	}
	second, secondRaw, err := store.CreateCredential(ctx, CreateCredentialRequest{
		WorkspaceID: workspaceID, AppID: app.ID, Name: "rotated",
		Scopes: []string{"cancel", "read", "invoke"}, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.ValidateCredential(ctx, secondRaw)
	if err != nil || principal.AppID != app.ID || !principal.HasScope("invoke") {
		t.Fatalf("validate rotated credential: principal=%+v error=%v", principal, err)
	}
	if err := store.RevokeCredential(ctx, workspaceID, first.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateCredential(ctx, firstRaw); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("revoked credential validation = %v", err)
	}
	if principal, err = store.ValidateCredential(ctx, secondRaw); err != nil || principal.AppID != app.ID {
		t.Fatalf("rotation changed stable app identity: principal=%+v error=%v", principal, err)
	}

	capability, err := store.CreateCapability(ctx, CreateCapabilityRequest{
		WorkspaceID: workspaceID, Key: "order_material_check", Name: "订单材料检查",
		Description: "返回缺项与待确认项", CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedNoToolArtifact(t, pool, workspaceID, "team-order-check", "flow-order-check", 1)
	limits := ExecutionLimits{
		MaxInputBytes: 65536, MaxOutputBytes: 32768, MaxNestingDepth: 20,
		MaxObjectFields: 256, MaxArrayItems: 1000, MaxStringBytes: 8192,
		QueueTimeoutSeconds: 60, ExecutionTimeoutSeconds: 300, MaxOutputTokens: 1200,
	}
	release, err := store.PublishRelease(ctx, PublishReleaseRequest{
		WorkspaceID: workspaceID, CapabilityID: capability.ID, Version: 1,
		WorkflowID: "flow-order-check", WorkflowVersion: 1,
		ExecutionLimits: limits,
		ResultPolicy:    ResultPolicy{ExposedFields: []string{"issues", "summary"}, IncludeContractEvidence: true},
		CreatedBy:       admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != 1 || release.WorkflowVersion != 1 ||
		release.ContractDialect != ContractDialectV1 || release.Normalization != NormalizationV1 ||
		len(release.ArtifactContentHash) != 64 || release.ResultPolicy.ExposedFields[0] != "issues" {
		t.Fatalf("unexpected release: %+v", release)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_capability_releases SET workflow_version=2 WHERE workspace_id=$1 AND id=$2`, workspaceID, release.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable release update error = %v", err)
	}
	if _, err := store.PublishRelease(ctx, PublishReleaseRequest{
		WorkspaceID: workspaceID, CapabilityID: capability.ID, Version: 3,
		WorkflowID: "flow-order-check", WorkflowVersion: 1,
		ExecutionLimits: limits, ResultPolicy: ResultPolicy{ExposedFields: []string{"summary"}},
		CreatedBy: admin.ID,
	}); !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("release version gap error = %v", err)
	}

	grant, err := store.GrantRelease(ctx, GrantReleaseRequest{
		WorkspaceID: workspaceID, AppID: app.ID, CapabilityID: capability.ID,
		ReleaseVersion: 1, MaxConcurrent: 2, GrantedBy: admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantRelease(ctx, GrantReleaseRequest{
		WorkspaceID: workspaceID, AppID: app.ID, CapabilityID: capability.ID,
		ReleaseVersion: 1, MaxConcurrent: 2, GrantedBy: admin.ID,
	}); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("duplicate active grant error = %v", err)
	}
	if err := store.RevokeGrant(ctx, workspaceID, grant.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	regained, err := store.GrantRelease(ctx, GrantReleaseRequest{
		WorkspaceID: workspaceID, AppID: app.ID, CapabilityID: capability.ID,
		ReleaseVersion: 1, MaxConcurrent: 1, GrantedBy: admin.ID,
	})
	if err != nil || regained.ID == grant.ID {
		t.Fatalf("regrant = %+v error=%v", regained, err)
	}
	if second.ID == first.ID {
		t.Fatal("rotated credential reused identity")
	}
}

func seedNoToolArtifact(t *testing.T, pool *pgxpool.Pool, workspaceID, teamID, workflowID string, version int) {
	t.Helper()
	payload, contentHash, graph := noToolArtifact(t, workspaceID, teamID, workflowID, version)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agents(id,workspace_id,name,display_name,role,spec,version,deleted)
		VALUES($1,$2,$1,'订单材料检查负责人','avatar','{"role":"avatar"}',1,false)
	`, "lead-"+teamID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agent_versions(workspace_id,agent_id,version,spec)
		VALUES($1,$2,1,'{"role":"avatar"}')
	`, workspaceID, "lead-"+teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agents(id,workspace_id,name,display_name,role,spec,version,deleted)
		VALUES('worker-order-check',$1,'worker-order-check','订单材料检查','worker',
		       '{"role":"worker"}',1,false)
	`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_agent_versions(workspace_id,agent_id,version,spec)
		VALUES($1,'worker-order-check',1,'{"role":"worker"}')
	`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)
		VALUES($1,$2,$1,$3,'active')
	`, teamID, workspaceID, "lead-"+teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_workers(
			workspace_id,team_id,worker_agent_id,allowed_kinds,default_kind,enabled
		) VALUES($1,$2,'worker-order-check',ARRAY['consult']::text[],'consult',true)
	`, workspaceID, teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_workflows(
			workspace_id,id,team_id,name,status,published_version
		) VALUES($1,$2,$3,$2,'active',NULL)
	`, workspaceID, workflowID, teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_workflow_versions(
			workspace_id,workflow_id,version,status,trigger_config,graph_definition,
			created_by
		) VALUES($1,$2,$3,'draft',
			'{"schema_version":1,"type":"conversation_explicit","config":{}}',
			$4,$5)
	`, workspaceID, workflowID, version, graph, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_workflow_versions
		SET status='published',published_at=now(),updated_at=now()
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
	`, workspaceID, workflowID, version); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_published_artifact_contents(
			workspace_id,workflow_id,workflow_version,artifact_schema_version,
			canonicalization_algorithm,canonicalization_version,hash_algorithm,
			content_hash,payload
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, workspaceID, workflowID, version, frozen.ArtifactSchemaVersion,
		frozen.ArtifactCanonicalizationAlgorithm, frozen.ArtifactCanonicalizationVersion,
		frozen.ArtifactHashAlgorithm, contentHash, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workflow_version_admission_statuses(
			workspace_id,workflow_id,workflow_version,blocked
		) VALUES($1,$2,$3,false)
	`, workspaceID, workflowID, version); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_workflows SET published_version=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, workflowID, version); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func noToolArtifact(t *testing.T, workspaceID, teamID, workflowID string, version int) (json.RawMessage, string, json.RawMessage) {
	t.Helper()
	inputSchema := json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"},"materials":{"type":"array","items":{"type":"string"}}},"required":["order_id","materials"],"additionalProperties":false}`)
	outputSchema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"issues":{"type":"array","items":{"type":"string"}}},"required":["summary","issues"],"additionalProperties":false}`)
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"check","input_contract":{"type":"json","schema":` + string(inputSchema) + `},"output_contract":{"type":"json","schema":` + string(outputSchema) + `},"nodes":[{"id":"check","type":"worker","inputs":{"task":{"expected_type":"json","value":{"source":"run_input","path":""}}},"output":{"type":"json","schema":` + string(outputSchema) + `},"config":{"agent_id":"worker-order-check","agent_version":1,"kind":"consult","result_requirement":"Return the declared JSON result"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"check","path":""}}}],"edges":[{"id":"deliver-result","from_node_id":"check","to_node_id":"deliver","route":"success"}]}`)
	if _, report := machine.DecodeGraphDefinitionV1(graph); report != nil && len(report.Issues) > 0 {
		t.Fatalf("invalid no-tool test graph: %+v", report.Issues)
	}
	manifestHash, manifest, err := frozen.BuildFrozenDependencyManifest(nil)
	if err != nil || manifestHash == "" {
		t.Fatal(err)
	}
	runtimeRef := frozen.CredentialReference{
		SchemaVersion: 1, WorkspaceID: workspaceID,
		Kind: frozen.CredentialRuntimeAccess, ResourceID: "runtime-order-check", Slot: "access",
	}
	agentHash := strings.Repeat("a", 64)
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion:   1,
		TriggerConfig:   json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
		GraphDefinition: graph,
		Team: frozen.ArtifactTeamV1{
			WorkspaceID: workspaceID, TeamID: teamID, LeadAgentID: "worker-order-check",
			LeadAgentVersion: 1, LeadAgentContentHash: agentHash,
		},
		Bundles: []frozen.FrozenExecutionBundle{{
			SchemaVersion: 1,
			FactoryKey:    frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "1", CompilerABI: "weave-graph-abi-v1"},
			Agent: frozen.FrozenAgentRecord{
				SchemaVersion: 1, WorkspaceID: workspaceID, AgentID: "worker-order-check", AgentVersion: 1,
				Name: "worker-order-check", DisplayName: "订单材料检查", Role: "worker",
				Engine: "codex", RuntimeID: "runtime-order-check", Model: "gpt-6-astra",
				SystemPrompt: "Check the supplied order materials.",
				Identity:     frozen.FrozenAgentIdentity{Core: "Check the supplied order materials.", Raw: "Check the supplied order materials."},
				Profiles:     map[string]frozen.FrozenAgentProfile{}, Permissions: frozen.FrozenPermissions{Deny: []string{"*"}, Allow: []string{}, Ask: []string{}},
				MemoryConfig: &frozen.FrozenMemoryConfig{}, MemorySlots: []frozen.FrozenMemorySlot{},
				OutputSchema: outputSchema, Fallback: frozen.FrozenFallback{Models: []string{}},
				GraphType: "standard", FactoryInput: json.RawMessage(`{}`),
			},
			Skills: []frozen.FrozenSkill{}, MCPBindings: []frozen.FrozenMCPBinding{},
			FallbackModels: []frozen.FrozenModelBinding{},
			Runtime: &frozen.FrozenRuntimeBinding{
				SchemaVersion: 1, WorkspaceID: workspaceID, RuntimeID: "runtime-order-check",
				Engine: "codex", RuntimeRevision: 1, AccessRef: runtimeRef,
				ContentHash: strings.Repeat("b", 64),
			},
			Credentials: []frozen.CredentialReference{}, Dependencies: manifest,
			Capability: frozen.CapabilityManifest{
				SchemaVersion: frozen.CapabilityManifestSchemaVersion, Role: "worker",
				AgentContentHash: agentHash, InteractiveStepIDs: []string{},
				InteractiveToolIDs: []string{}, AgentStepIDs: []string{},
			},
		}},
		DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	canonical, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: version,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical, hash, graph
}
