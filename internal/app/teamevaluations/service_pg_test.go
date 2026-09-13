package teamevaluations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// The evaluation service has no metateam dependency; paired with the API's
// disabled-config handler test, this exercises the real-PG off-mode path.
func TestPostTemplateEvaluationPGMetaTeamDisabledPathConcurrentAndBaselineCAS(t *testing.T) {
	fixture := newEvaluationPGFixture(t)
	outcome := fixture.start(t, fixture.contract, uuid.NewString())
	if outcome.Status != teambuild.StatusAuthorized {
		t.Fatalf("outcome = %#v", outcome)
	}
	if _, err := fixture.service.Evaluate(fixture.ctx, fixture.workspaceID, "admin-1", fixture.team.ID, Request{
		Contract: fixture.contract, IdempotencyKey: uuid.NewString(), Budget: Budget{MaxCostUSD: 5},
	}); !errors.Is(err, ErrConcurrentEvaluation) {
		t.Fatalf("concurrent Evaluate() error = %v, want ErrConcurrentEvaluation", err)
	}
	run, err := fixture.builds.GetBuildRun(fixture.ctx, fixture.workspaceID, outcome.BuildRunID)
	if err != nil || !run.EvaluationOnly || run.EvaluationTeamID != fixture.team.ID {
		t.Fatalf("evaluation run = %#v err = %v", run, err)
	}
	revision, err := fixture.builds.GetLatestBlueprintRevision(fixture.ctx, fixture.workspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	var changeSet teamforge.ChangeSetV1
	if err := json.Unmarshal(revision.ChangeSetJSON, &changeSet); err != nil {
		t.Fatal(err)
	}
	for _, operation := range changeSet.Operations {
		switch operation.Type {
		case teamforge.OperationWorkflowCompile, teamforge.OperationCandidateRun, teamforge.OperationPublish:
		default:
			t.Fatalf("evaluation ChangeSet contains asset mutation %s", operation.Type)
		}
	}
	fixture.toPublishing(t, run.BuildRunID)
	if _, err := fixture.pool.Exec(fixture.ctx, `
		UPDATE weave_teams SET primary_scenario='changed during evaluation', updated_at=now() + interval '1 second'
		WHERE workspace_id=$1 AND id=$2
	`, fixture.workspaceID, fixture.team.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fixture.ctx) }()
	if _, err := fixture.builds.VerifyEvaluationBaselineTx(fixture.ctx, tx, fixture.workspaceID, run.BuildRunID); !errors.Is(err, teambuild.ErrEvaluationBaselineChanged) {
		t.Fatalf("VerifyEvaluationBaselineTx() error = %v, want baseline CAS failure", err)
	}
}

func TestPostTemplateEvaluationPGAtomicCertification(t *testing.T) {
	fixture := newEvaluationPGFixture(t)
	fixture.ctx = execution.WithSubject(fixture.ctx, execution.Subject{WorkspaceID: fixture.workspaceID, UserID: "admin-1"})
	outcome := fixture.start(t, fixture.contract, uuid.NewString())
	fixture.toPublishing(t, outcome.BuildRunID)

	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fixture.ctx) }()
	baselineHash, err := fixture.builds.VerifyEvaluationBaselineTx(fixture.ctx, tx, fixture.workspaceID, outcome.BuildRunID)
	if err != nil {
		t.Fatalf("VerifyEvaluationBaselineTx() error = %v", err)
	}
	version, err := fixture.workflows.GetVersion(fixture.ctx, fixture.workspaceID, fixture.workflowID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, report := machine.DecodeGraphDefinitionV1(version.GraphDefinition); report != nil && len(report.Issues) != 0 {
		t.Fatalf("fixture workflow graph is invalid: %#v", report.Issues)
	}
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion: frozen.ArtifactSchemaVersion, TriggerConfig: version.TriggerConfig,
		GraphDefinition: version.GraphDefinition,
		Team: frozen.ArtifactTeamV1{
			WorkspaceID: fixture.workspaceID, TeamID: fixture.team.ID,
			LeadAgentID: fixture.team.LeadAvatarID,
		},
		Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	contentHash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: fixture.workspaceID, WorkflowID: fixture.workflowID, WorkflowVersion: 1,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payload,
	})
	if err != nil {
		t.Fatalf("hash publication artifact: %v", err)
	}
	payloadJSON, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatalf("encode publication artifact: %v", err)
	}
	// Frozen content is a pre-existing kernel receipt fixture. Its independent
	// writes do not participate in the product certification transaction below.
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload) VALUES($1,$2,1,$3,$4,$5,$6,$7,$8::jsonb)`, fixture.workspaceID, fixture.workflowID, frozen.ArtifactSchemaVersion, frozen.ArtifactCanonicalizationAlgorithm, frozen.ArtifactCanonicalizationVersion, frozen.ArtifactHashAlgorithm, contentHash, string(payloadJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO weave_workflow_version_admission_statuses(workspace_id,workflow_id,workflow_version,blocked)VALUES($1,$2,1,false)`, fixture.workspaceID, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(fixture.ctx, `UPDATE weave_team_workflow_versions SET status='published',published_at=now(),updated_at=now() WHERE workspace_id=$1 AND workflow_id=$2 AND version=1`, fixture.workspaceID, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(fixture.ctx, `UPDATE weave_team_workflows SET published_version=1,updated_at=now() WHERE workspace_id=$1 AND id=$2`, fixture.workspaceID, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := teamconstruction.MarkBuildPublicationTx(fixture.ctx, tx, fixture.builds, fixture.workspaceID, outcome.BuildRunID, "judge", teambuild.FinalRef{
		Ref: contentHash, TeamID: fixture.team.ID,
	}, baselineHash); err != nil {
		t.Fatalf("MarkPublishedTx() error = %v", err)
	}
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	team, err := fixture.org.GetTeam(fixture.ctx, fixture.workspaceID, fixture.team.ID)
	if err != nil || team.Evaluation != org.TeamEvaluationEvaluated ||
		team.EvaluationBuildRunID != outcome.BuildRunID || team.EvaluationContractHash == "" || team.EvaluatedAt == nil {
		t.Fatalf("certified team = %#v err = %v", team, err)
	}
	run, err := fixture.builds.GetBuildRun(fixture.ctx, fixture.workspaceID, outcome.BuildRunID)
	if err != nil || run.Status != teambuild.StatusPassed {
		t.Fatalf("passed run = %#v err = %v", run, err)
	}
	wf, err := fixture.workflows.Get(fixture.ctx, fixture.workspaceID, fixture.workflowID)
	if err != nil || wf.PublishedVersion == nil || *wf.PublishedVersion != 1 {
		t.Fatalf("published workflow = %#v err = %v", wf, err)
	}

	dispatchTx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dispatchTx.Rollback(fixture.ctx) }()
	admitted, err := fixture.workflows.AdmitWorkflowManualRunTx(fixture.ctx, dispatchTx, workflow.WorkflowManualRunAdmissionRequest{
		WorkspaceID: fixture.workspaceID, WorkflowID: fixture.workflowID,
		SourceRef: "m2b-acceptance", TriggerType: "conversation_explicit",
	})
	if err != nil {
		t.Fatalf("admit published workflow dispatch: %v", err)
	}
	createdSnapshot, err := snapshot.NewStore(fixture.pool).CreateTx(fixture.ctx, dispatchTx, admitted)
	if err != nil {
		t.Fatalf("persist published workflow dispatch snapshot: %v", err)
	}
	task := &taskqueue.Task{
		ID: "task-" + uuid.NewString(), WorkspaceID: fixture.workspaceID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: fixture.workflowID, WorkflowVersion: 1, RunSnapshotID: createdSnapshot.RunID,
		Source: "session", Kind: "team_workflow", Payload: json.RawMessage(`"真实评测后派活"`),
	}
	if err := taskqueue.New(fixture.pool, taskqueue.RealClock{}, time.Minute).EnqueueTx(fixture.ctx, dispatchTx, task); err != nil {
		t.Fatalf("enqueue published workflow dispatch: %v", err)
	}
	if err := dispatchTx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var queuedStatus string
	if err := fixture.pool.QueryRow(fixture.ctx, `
		SELECT status FROM weave_task_queue WHERE workspace_id=$1 AND id=$2
	`, fixture.workspaceID, task.ID).Scan(&queuedStatus); err != nil || queuedStatus != taskqueue.StatusQueued {
		t.Fatalf("dispatched task status = %q err = %v", queuedStatus, err)
	}
}

func TestPostTemplateEvaluationPGBudgetReauthorization(t *testing.T) {
	fixture := newEvaluationPGFixture(t)
	outcome := fixture.start(t, fixture.contract, uuid.NewString())
	run, err := fixture.builds.TransitionStatus(
		fixture.ctx, fixture.workspaceID, outcome.BuildRunID,
		teambuild.StatusAuthorized, teambuild.StatusRoundRunning,
		"worker", "candidate started",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.builds.RecordBudgetUsage(fixture.ctx, fixture.workspaceID, run.BuildRunID, teambuild.BudgetCharge{
		WorkspaceID: fixture.workspaceID, BuildRunID: run.BuildRunID, RoundNo: 1,
		SourceKind:  teambuild.UsageSourceKindCandidateRuntime,
		SourceRole:  teambuild.SourceRoleFixedWorkflowRoot,
		SourceRunID: "evaluation-candidate", CostUSD: 5.01,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.builds.TransitionStatus(
		fixture.ctx, fixture.workspaceID, run.BuildRunID,
		teambuild.StatusRoundRunning, teambuild.StatusBlocked,
		"worker", teambuild.BudgetExhaustedReason,
	); err != nil {
		t.Fatal(err)
	}
	restored, receipt, err := fixture.builds.ReauthorizeBudgetBlockedRun(
		fixture.ctx, fixture.workspaceID, run.BuildRunID, "admin-2",
		teambuild.Budget{MaxCostUSD: 6}, teambuild.Budget{MaxCostUSD: 6},
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != teambuild.StatusAuthorized || !restored.EvaluationOnly ||
		restored.EvaluationTeamID != fixture.team.ID || restored.BriefHash != run.BriefHash {
		t.Fatalf("restored evaluation run = %#v", restored)
	}
	if !receipt.Valid() || receipt.RoundBudget().MaxCostUSD != 6 || receipt.TotalBudget().MaxCostUSD != 6 {
		t.Fatalf("restored evaluation receipt = %#v", receipt)
	}
}

func TestPostTemplateEvaluationPGFullBudgetAndUsageWaiverReceipt(t *testing.T) {
	fixture := newEvaluationPGFixture(t)
	waiver := &teambuild.UnmeasuredUsageWaiver{
		Accepted: true, Reason: "CLI usage receipts are unavailable during Phase 1",
	}
	outcome, err := fixture.service.Evaluate(
		fixture.ctx, fixture.workspaceID, "admin-1", fixture.team.ID,
		Request{
			Contract: fixture.contract, IdempotencyKey: uuid.NewString(),
			Budget: Budget{
				MaxInputTokens: 1000, MaxOutputTokens: 500,
				MaxToolCalls: 20, MaxCostUSD: 5,
			},
			UnmeasuredUsageWaiver: waiver,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.builds.GetBuildRun(fixture.ctx, fixture.workspaceID, outcome.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	wantBudget := teambuild.Budget{
		MaxInputTokens: 1000, MaxOutputTokens: 500, MaxToolCalls: 20, MaxCostUSD: 5,
	}
	if run.RoundBudget != wantBudget || run.TotalBudget != wantBudget ||
		run.Brief.UnmeasuredUsageWaiver == nil || *run.Brief.UnmeasuredUsageWaiver != *waiver {
		t.Fatalf("evaluation budget/waiver = %#v", run)
	}
	receipt, err := fixture.builds.ReissueReceipt(fixture.ctx, fixture.workspaceID, outcome.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.UnmeasuredUsageWaiver() == nil || *receipt.UnmeasuredUsageWaiver() != *waiver {
		t.Fatalf("authorization receipt waiver = %#v", receipt.UnmeasuredUsageWaiver())
	}
}

type evaluationPGFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	workspaceID string
	team        org.Team
	workflowID  string
	contract    teambuild.EvaluationContract
	builds      *teambuild.Store
	org         *orgstore.Store
	workflows   *workflowcatalog.Store
	service     *Service
}

func newEvaluationPGFixture(t *testing.T) *evaluationPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	prefix := "eval" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	workspaceID := "workspace-" + prefix
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: prefix + "-lead", DisplayName: "负责人", Role: "avatar"}
	primary := registry.AgentRecord{Name: prefix + "-primary", DisplayName: "执行者", Role: "worker"}
	reviewer := registry.AgentRecord{Name: prefix + "-reviewer", DisplayName: "评审者", Role: "worker"}
	for _, agent := range []*registry.AgentRecord{&lead, &primary, &reviewer} {
		if err := agents.Put(ctx, workspaceID, agent); err != nil {
			t.Fatalf("create agent %s: %v", agent.Name, err)
		}
	}
	orgStore := orgstore.NewStore(pool)
	created, err := orgStore.CreateActiveTeam(ctx, workspaceID, org.CreateActiveTeamInput{
		Name: prefix + "-team", Objective: "真实业务交付",
		PrimaryScenario: "根据客户材料形成完整分析报告", SuccessCriteria: "事实正确、结构完整、可直接交付",
		LeadAvatarID: lead.ID, Evaluation: org.TeamEvaluationUnevaluated,
		Workers: []org.InitialTeamWorker{
			{WorkerAgentID: primary.ID, Duty: "执行", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "形成初稿"},
			{WorkerAgentID: reviewer.ID, Duty: "评审", AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "指出缺陷"},
		},
	})
	if err != nil {
		t.Fatalf("create unevaluated team: %v", err)
	}
	maxIterations := 1
	blueprint := teambuild.TeamBlueprintV1{
		SchemaVersion: teambuild.BlueprintSchemaVersionV1, Mode: teambuild.ModeCreate,
		NewTeamName: prefix + "-team", Purpose: created.Team.Objective,
		Members: []teambuild.BlueprintMemberV1{
			{StableRef: "lead", Name: lead.Name, DisplayName: lead.DisplayName, Role: teambuild.BlueprintMemberRoleAvatar, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"协调交付"}, Capabilities: []string{"delegation"}, ExecutionPolicy: standardPolicy()},
			{StableRef: "primary", Name: primary.Name, DisplayName: primary.DisplayName, Role: teambuild.BlueprintMemberRoleWorker, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"完成初稿"}, Capabilities: []string{"writing"}, ExecutionPolicy: standardPolicy()},
			{StableRef: "reviewer", Name: reviewer.Name, DisplayName: reviewer.DisplayName, Role: teambuild.BlueprintMemberRoleWorker, ManagementMode: teambuild.BlueprintManagementManaged, Responsibilities: []string{"业务评审"}, Capabilities: []string{"review"}, ExecutionPolicy: standardPolicy()},
		},
		LeadRef: "lead",
		Workflow: teambuild.BlueprintWorkflowV1{
			Mode: teambuild.BlueprintWorkflowTemplate, Template: teambuild.BlueprintTemplateDeliveryRework,
			TemplateParameters: &teambuild.BlueprintWorkflowTemplateParametersV1{
				LeadInstruction: "协调真实业务交付", PrimaryRef: "primary", ReviewerRef: "reviewer",
				MaxIterations:      &maxIterations,
				ResultRequirements: map[string]string{"primary": "形成完整初稿", "reviewer": "按标准评审"},
			},
		},
		RevisionPolicy: teambuild.BlueprintRevisionPolicyV1{MaxRevisions: 1, AllowedPatchPaths: []string{"/purpose"}},
	}
	// Template instantiation is allowed to preserve a template-defined workflow
	// identity. Evaluation must freeze that real draft instead of substituting
	// FirstOptimizeWorkflowID(teamID).
	workflowID := prefix + "-template-workflow"
	compiled, problems := teamforge.CompileWorkflowBlueprint(teamforge.WorkflowBlueprint{
		Template: teamforge.WorkflowBlueprintDeliveryRework, LeadInstruction: "协调真实业务交付",
		Primary:       &teamforge.WorkflowBlueprintWorker{AgentID: primary.ID, AgentVersion: 1, ResultRequirement: "形成完整初稿"},
		Reviewer:      &teamforge.WorkflowBlueprintWorker{AgentID: reviewer.ID, AgentVersion: 1, ResultRequirement: "按标准评审"},
		MaxIterations: int64Pointer(1),
	})
	if len(problems) != 0 {
		t.Fatalf("compile workflow: %#v", problems)
	}
	triggerJSON, _ := json.Marshal(compiled.Trigger)
	graphJSON := strictFixtureGraphJSON(t, compiled.Graph)
	workflowStore := workflowcatalog.New(pool, workflow.RealClock{}, workflow.NewArtifactStore(pool, workflow.RealClock{}))
	if _, err := workflowStore.Create(ctx, &workflow.TeamWorkflow{
		WorkspaceID: workspaceID, ID: workflowID, TeamID: created.Team.ID,
		Name: workflowID, Description: blueprint.Purpose,
	}, workflow.DraftInput{TriggerConfig: triggerJSON, GraphDefinition: graphJSON, CreatedBy: "template"}); err != nil {
		t.Fatalf("create template workflow draft: %v", err)
	}
	contract := realEvaluationContract()
	builds := teambuild.New(pool, teambuild.RealClock{})
	builds.SetBaselineSources(orgStore, agents, workflowStore, workflow.NewArtifactStore(pool, nil))
	seedTemplateLineage(t, ctx, builds, workspaceID, created.Team.ID, prefix, blueprint, contract)
	service := New(NewPGIdempotencyStore(pool), builds, noopEvaluationSubmitter{}, Options{
		OrgStore: orgStore, Registry: agents, Workflows: workflowStore, Artifacts: workflow.NewArtifactStore(pool, nil),
	})
	return &evaluationPGFixture{
		ctx: ctx, pool: pool, workspaceID: workspaceID, team: created.Team,
		workflowID: workflowID, contract: contract, builds: builds, org: orgStore,
		workflows: workflowStore, service: service,
	}
}

func seedTemplateLineage(
	t *testing.T,
	ctx context.Context,
	builds *teambuild.Store,
	workspaceID, teamID, prefix string,
	blueprint teambuild.TeamBlueprintV1,
	contract teambuild.EvaluationContract,
) {
	t.Helper()
	run, err := builds.CreateBuildRun(ctx, workspaceID, "template-"+prefix, teambuild.CreateRunParams{
		Brief: teambuild.BuildBrief{
			SchemaVersion: 1, Mode: teambuild.ModeCreate, BusinessDirection: "模板创建",
			Task: "创建待评测团队", NewTeamName: blueprint.NewTeamName,
			SuccessCriteria: []string{"资产完成"},
			AllowedAssets:   teambuild.AssetScope{AllowedKinds: []string{"agent", "team", "workflow"}, NamePrefix: prefix},
			RoundBudget:     teambuild.Budget{MaxCostUSD: 1}, TotalBudget: teambuild.Budget{MaxCostUSD: 1},
		},
		Contract: contract, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "admin-1",
		ExecutionStrategy: teambuild.ExecutionStrategyTemplateInstantiate,
	})
	if err != nil {
		t.Fatalf("create template lineage run: %v", err)
	}
	changeSet, err := teamforge.CompileTemplateInstantiateChangeSetV1(teamforge.EmptyCreateBaselineV1(), blueprint)
	if err != nil {
		t.Fatal(err)
	}
	blueprintJSON, _ := json.Marshal(blueprint)
	blueprintHash, _ := blueprint.BlueprintHash()
	changeSetJSON, _ := changeSet.TemplateInstantiateCanonicalBytes()
	changeSetHash, _ := changeSet.TemplateInstantiateCanonicalHash()
	revision, err := builds.PersistCompilerAuthorizationBundle(ctx, workspaceID, run.BuildRunID, teambuild.CompilerAuthorizationBundle{
		RevisionNo: 1, BlueprintJSON: blueprintJSON, BlueprintHash: blueprintHash,
		ChangeSetJSON: changeSetJSON, ChangeSetHash: changeSetHash,
		BaselineHash: teamforge.EmptyCreateBaselineHashV1, EvaluationContractHash: run.ContractHash,
	})
	if err != nil {
		t.Fatalf("persist template lineage: %v", err)
	}
	_, _, err = builds.AuthorizeTemplateBuildRun(ctx, workspaceID, run.BuildRunID, "admin-1", teambuild.BlueprintRevisionToken{
		RevisionNo: 1, BlueprintHash: revision.BlueprintHash, ChangeSetHash: revision.ChangeSetHash,
	}, teambuild.TemplateAuthorizationPolicy{AutoBudgetThresholdUSD: 10, DailyBudgetUSD: 100, MonthlyBudgetUSD: 1000, MaxConcurrent: 10})
	if err != nil {
		t.Fatalf("authorize template lineage: %v", err)
	}
	if _, err := builds.TransitionStatus(ctx, workspaceID, run.BuildRunID, teambuild.StatusAuthorized, teambuild.StatusRoundRunning, "worker", "materialize"); err != nil {
		t.Fatal(err)
	}
	if _, err := builds.MarkTemplateInstantiated(ctx, workspaceID, run.BuildRunID, "worker", teambuild.FinalRef{Ref: teamID, TeamID: teamID}); err != nil {
		t.Fatal(err)
	}
}

func (f *evaluationPGFixture) start(t *testing.T, contract teambuild.EvaluationContract, key string) Outcome {
	t.Helper()
	outcome, err := f.service.Evaluate(f.ctx, f.workspaceID, "admin-1", f.team.ID, Request{
		Contract: contract, IdempotencyKey: key, Budget: Budget{MaxCostUSD: 5},
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	return outcome
}

func (f *evaluationPGFixture) toPublishing(t *testing.T, buildRunID string) {
	t.Helper()
	if _, err := f.builds.TransitionStatus(f.ctx, f.workspaceID, buildRunID, teambuild.StatusAuthorized, teambuild.StatusRoundRunning, "worker", "candidate started"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.builds.TransitionStatus(f.ctx, f.workspaceID, buildRunID, teambuild.StatusRoundRunning, teambuild.StatusPublishing, "worker", "candidate passed"); err != nil {
		t.Fatal(err)
	}
}

func standardPolicy() teambuild.BlueprintExecutionPolicyV1 {
	return teambuild.BlueprintExecutionPolicyV1{EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop}
}

func int64Pointer(value int64) *int64 { return &value }

func strictFixtureGraphJSON(t *testing.T, graph machine.GraphDefinition) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var restoreRequiredPaths func(any)
	restoreRequiredPaths = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if source, _ := typed["source"].(string); source == string(machine.ValueRunInput) || source == string(machine.ValueNodeOutput) {
				if _, exists := typed["path"]; !exists {
					typed["path"] = ""
				}
			}
			for _, child := range typed {
				restoreRequiredPaths(child)
			}
		case []any:
			for _, child := range typed {
				restoreRequiredPaths(child)
			}
		}
	}
	restoreRequiredPaths(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func realEvaluationContract() teambuild.EvaluationContract {
	rule := "改变输入细节"
	return teambuild.EvaluationContract{
		SchemaVersion: 2, HardGates: teambuild.DefaultFloorHardGates(),
		Rubric:                 []teambuild.RubricDimension{{ID: "quality", Name: "质量", Description: "业务质量", MaxScore: 10, PassThreshold: 7}},
		PublicScenarios:        []teambuild.Scenario{{ID: "real-business-case", Input: "分析客户材料", Expected: "完整分析报告"}},
		PerturbationRules:      []string{rule},
		PerturbationScenarios:  []teambuild.PerturbationScenario{{ID: "real-business-case-variant", BaseScenarioID: "real-business-case", Rule: rule, Input: "分析变化后的客户材料", Expected: "完整分析报告"}},
		SevereDefectDefinition: "关键事实错误或缺少核心交付物", RunCount: 1, MaxIterations: 3,
		PassRules: []string{"全部门禁与 rubric 通过"}, BlockRules: []string{"质量失败即阻断"}, InfraFailureRules: []string{"基础设施失败单独报告"},
	}
}

type noopEvaluationSubmitter struct{}

func (noopEvaluationSubmitter) Submit(context.Context, string, string) error { return nil }
