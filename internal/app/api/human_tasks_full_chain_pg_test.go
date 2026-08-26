package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

func TestHumanFinalReviewSampleRealPGFullChain(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}

	prefix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	workspaceID, userID := "workspace-"+prefix, "user-"+prefix
	teamID, workflowID := "team-"+prefix, "workflow-"+prefix
	buildRunID := "build-" + prefix
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces (id,slug,name) VALUES ($1,$1,'M3 isolated workspace');
		INSERT INTO weave_users (id,tenant_id,username,password,role) VALUES ($2,$1,$2,'x','user');
		INSERT INTO weave_members (workspace_id,user_id,role) VALUES ($1,$2,'member')
	`, workspaceID, userID); err != nil {
		t.Fatalf("seed sample workspace and membership: %v", err)
	}
	lead := registry.AgentRecord{Name: "sample4-lead-" + prefix, DisplayName: "交付负责人", Role: "avatar"}
	if err := registry.New(pool).Put(ctx, workspaceID, &lead); err != nil {
		t.Fatalf("seed sample lead avatar: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_teams (id,workspace_id,name,lead_avatar_id,status,created_at)
		VALUES ($1,$2,'人工终审交付团队',$3,'building',now())
	`, teamID, workspaceID, lead.ID); err != nil {
		t.Fatalf("seed sample building team: %v", err)
	}

	sample := humanFinalReviewSample(t)
	templateCompilation, err := teamtemplate.CompileYAML([]byte(sample.YAML))
	if err != nil {
		t.Fatalf("compile sample 4 team template: %v", err)
	}
	bindings, err := teamforge.ResolveCreateDeclarativeWorkerBindingsV1(*sample.DeclarativeSpec, templateCompilation.Blueprint)
	if err != nil {
		t.Fatalf("resolve sample 4 declarative worker bindings: %v", err)
	}
	planned := make([]teameval.PlannedWorkerBinding, 0, len(bindings))
	for _, binding := range bindings {
		planned = append(planned, teameval.PlannedWorkerBinding{
			StableRef: binding.StableRef, AgentID: binding.AgentID, AgentVersion: binding.AgentVersion,
		})
	}
	briefHash, _, contractHash, err := teambuild.ValidateBuildRunDrafts(
		templateCompilation.Brief, templateCompilation.Contract,
	)
	if err != nil {
		t.Fatalf("validate sample 4 build drafts: %v", err)
	}
	frozenSpec, err := teamforge.FreezeDeclarativeWorkflowSpecV1(
		*sample.DeclarativeSpec,
		bindings,
		teamforge.DeclarativeBuildBindingV1{
			BuildRunID: buildRunID, BriefHash: briefHash, ContractHash: contractHash,
			AssetScope:   templateCompilation.Brief.AllowedAssets,
			BaselineHash: teamforge.EmptyCreateBaselineHashV1,
		},
		func(trigger machine.TriggerConfig, graph machine.GraphDefinition) (machine.Report, error) {
			return teameval.ValidateWorkflowForBlueprint(
				workspaceID, templateCompilation.Blueprint, planned, trigger, graph,
			)
		},
	)
	if err != nil {
		t.Fatalf("freeze sample 4 declarative workflow: %v", err)
	}
	artifact := humanReviewArtifact(
		t, workspaceID, teamID, workflowID, lead.ID, frozenSpec.TriggerConfig, frozenSpec.GraphDefinition,
	)
	workflowStore := workflow.New(pool, workflow.RealClock{})
	createdWorkflow, err := workflowStore.Create(ctx, &workflow.TeamWorkflow{
		WorkspaceID: workspaceID, ID: workflowID, TeamID: teamID,
		Name: "sample-4-human-final-review", Description: sample.Description,
	}, workflow.DraftInput{
		TriggerConfig: frozenSpec.TriggerConfig, GraphDefinition: frozenSpec.GraphDefinition, CreatedBy: "sample-4",
	})
	if err != nil {
		t.Fatalf("create sample 4 workflow draft: %v", err)
	}
	version, err := workflowStore.GetVersion(ctx, workspaceID, workflowID, 1)
	if err != nil {
		t.Fatalf("read sample 4 workflow version: %v", err)
	}
	publicationTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflowStore.InsertPublicationTx(ctx, publicationTx, workflow.Publication{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1,
		ExpectedUpdatedAt: version.UpdatedAt, Artifact: *artifact,
	}); err != nil {
		_ = publicationTx.Rollback(ctx)
		t.Fatalf("publish sample 4 workflow artifact: %v", err)
	}
	if err := publicationTx.Commit(ctx); err != nil {
		t.Fatalf("commit sample 4 publication: %v", err)
	}
	if createdWorkflow.WorkflowID != workflowID || createdWorkflow.Version != 1 {
		t.Fatalf("created workflow version = %#v", createdWorkflow)
	}

	var candidatePayload frozen.ArtifactPayloadV1
	if err := json.Unmarshal(artifact.Payload, &candidatePayload); err != nil {
		t.Fatalf("decode sample 4 candidate payload: %v", err)
	}
	if _, graphReport := machine.DecodeGraphDefinitionV1(candidatePayload.GraphDefinition); graphReport != nil && len(graphReport.Issues) != 0 {
		t.Fatalf("decode sample 4 frozen graph: issues=%#v graph=%s", graphReport.Issues, candidatePayload.GraphDefinition)
	}
	admissionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admissionTx.Rollback(ctx) }()
	if err := workflowStore.InsertCandidateTx(ctx, admissionTx, &workflow.PublicationCandidate{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1,
		ExpectedUpdatedAt:         version.UpdatedAt,
		ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion:   artifact.CanonicalizationVersion,
		HashAlgorithm:             artifact.HashAlgorithm, ContentHash: artifact.ContentHash,
		Payload: candidatePayload, Dependencies: []workflow.TeamWorkflowDependency{},
	}, "sample-4"); err != nil {
		t.Fatalf("persist sample 4 candidate: %v", err)
	}
	runSnapshot, err := workflowStore.AdmitWorkflowCandidateRunTx(ctx, admissionTx, workflow.WorkflowCandidateRunAdmissionRequest{
		WorkspaceID: workspaceID, WorkflowID: workflowID, BuildRunID: buildRunID,
		ContentHash: artifact.ContentHash, SourceRef: buildRunID, TriggerType: "api",
	})
	if err != nil {
		t.Fatalf("admit sample 4 candidate run: %v", err)
	}
	snapshots := snapshot.NewStore(pool)
	if _, err := snapshots.CreateTx(ctx, admissionTx, runSnapshot); err != nil {
		t.Fatalf("persist sample 4 dispatch snapshot: %v", err)
	}
	if err := admissionTx.Commit(ctx); err != nil {
		t.Fatalf("commit sample 4 candidate admission: %v", err)
	}
	runID := runSnapshot.RunID

	tasks := taskqueue.New(pool, taskqueue.RealClock{}, time.Minute)
	sourceTask := &taskqueue.Task{
		ID: "task-" + prefix, WorkspaceID: workspaceID,
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: workflowID, WorkflowVersion: 1, RunSnapshotID: runID,
		Source: "api", Kind: "team_workflow", Payload: json.RawMessage(`"deliverable_ref:artifact-m3-final"`),
	}
	if err := tasks.Enqueue(ctx, sourceTask); err != nil {
		t.Fatalf("dispatch sample 4 task: %v", err)
	}

	runs, checkpoints := teamrun.NewPGStore(), teamrun.NewPGCheckpointStore()
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts: workflowStore, Loader: &workflow.RuntimeLoader{},
		HostFactory:         rejectingRuntimeHostFactory{},
		CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return nil, nil },
		Transactions:        pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks, Snapshots: snapshots,
	}
	executor := &teamrun.Executor{
		Tasks:        tasks,
		Consumer:     &teamrun.Consumer{Transactions: pool, Snapshots: snapshots, Runs: runs, Tasks: tasks},
		Transactions: pool, Runs: runs, Checkpoints: checkpoints, Runtime: runtime,
		ResumeTokenHash:   func() ([]byte, error) { return []byte("m3-human-resume-token-hash-32b"), nil },
		HeartbeatInterval: time.Second,
	}
	processed, err := executor.ProcessNext(ctx, "m3-worker-dispatch")
	if err != nil || !processed {
		t.Fatalf("execute sample 4 to human wait: processed=%v err=%v", processed, err)
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusParked)

	reader := &teamrun.HumanTaskReader{Pool: pool}
	resume := &teamrun.HumanResumeService{Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: tasks}
	server := &Server{OrgStore: org.NewStore(pool), teamRunHumanTasks: reader, teamRunHumanResume: resume}
	listRecorder := httptest.NewRecorder()
	listContext := humanTaskAPIContext(http.MethodGet, "/v1/human-tasks", "", listRecorder, workspaceID, userID)
	if err := server.handleListHumanTasks(listContext); err != nil || listRecorder.Code != http.StatusOK {
		t.Fatalf("list parked human task: status=%d err=%v body=%s", listRecorder.Code, err, listRecorder.Body.String())
	}
	var inbox struct {
		Tasks []humanTaskResponse `json:"tasks"`
		Total int                 `json:"total"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &inbox); err != nil || inbox.Total != 1 || len(inbox.Tasks) != 1 {
		t.Fatalf("decode human inbox: inbox=%#v err=%v body=%s", inbox, err, listRecorder.Body.String())
	}
	if got := string(inbox.Tasks[0].CompletedOutputs["draft"]); got != `"deliverable_ref:artifact-m3-final"` {
		t.Fatalf("pending deliverable preview = %s", got)
	}

	invalid := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID,
		`{"payload":{"decision":"maybe","comments":"invalid"},"idempotency_key":"m3-invalid"}`)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid resume payload status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	validBody := `{"payload":{"decision":"approve","comments":"终审通过"},"idempotency_key":"m3-complete"}`
	completed := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID, validBody)
	if completed.Code != http.StatusAccepted {
		t.Fatalf("complete human task status=%d body=%s", completed.Code, completed.Body.String())
	}
	conflict := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID,
		`{"payload":{"decision":"reject","comments":"changed"},"idempotency_key":"m3-complete"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same key different payload status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	processed, err = executor.ProcessNext(ctx, "m3-worker-resume")
	if err != nil || !processed {
		t.Fatalf("process queued human continuation: processed=%v err=%v", processed, err)
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusSucceeded)
	remaining, _, err := reader.List(ctx, workspaceID, nil, "", 20)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("human inbox after delivery: tasks=%d err=%v", len(remaining), err)
	}
}

func humanFinalReviewSample(t *testing.T) teamtemplates.Sample {
	t.Helper()
	for _, sample := range teamtemplates.NewStaticCatalog().List() {
		if sample.Name == "human-final-review" {
			if sample.DeclarativeSpec == nil {
				t.Fatal("sample 4 has no declarative workflow")
			}
			return sample
		}
	}
	t.Fatal("sample 4 is absent from catalog")
	return teamtemplates.Sample{}
}

func humanReviewArtifact(
	t *testing.T,
	workspaceID, teamID, workflowID, leadAgentID string,
	triggerJSON, graphJSON json.RawMessage,
) *workflow.PublishedArtifactContent {
	t.Helper()
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion: frozen.ArtifactSchemaVersion, TriggerConfig: triggerJSON, GraphDefinition: graphJSON,
		Team:    frozen.ArtifactTeamV1{WorkspaceID: workspaceID, TeamID: teamID, LeadAgentID: leadAgentID},
		Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payload,
	})
	if err != nil {
		t.Fatalf("hash sample 4 artifact: %v", err)
	}
	payloadJSON, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatalf("canonicalize sample 4 artifact: %v", err)
	}
	return &workflow.PublishedArtifactContent{
		WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, ContentHash: hash, Payload: payloadJSON,
	}
}

type rejectingRuntimeHostFactory struct{}

func (rejectingRuntimeHostFactory) Build(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	return compiler.FrozenBuildOpts{}, nil, errors.New("sample 4 contains no agent execution bundle")
}

func humanTaskAPIContext(method, path, body string, recorder *httptest.ResponseRecorder, workspaceID, userID string) echo.Context {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	ctx := echo.New().NewContext(request, recorder)
	ctx.Set("tenant", workspaceID)
	ctx.Set("user_id", userID)
	return ctx
}

func completeHumanTaskThroughAPI(t *testing.T, server *Server, workspaceID, userID, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx := humanTaskAPIContext(http.MethodPost, "/v1/human-tasks/"+runID+"/complete", body, recorder, workspaceID, userID)
	ctx.SetPath("/v1/human-tasks/:run_id/complete")
	ctx.SetParamNames("run_id")
	ctx.SetParamValues(runID)
	if err := server.handleCompleteHumanTask(ctx); err != nil {
		t.Fatalf("complete human task handler: %v", err)
	}
	return recorder
}

func assertHumanRunStatus(t *testing.T, ctx context.Context, transactions teamrun.TransactionBeginner, runs *teamrun.PGStore, workspaceID, runID string, want teamrun.Status) {
	t.Helper()
	tx, err := transactions.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := runs.GetTx(ctx, tx, workspaceID, runID)
	if err != nil || run.Status != want {
		var errorCode, causeSummary string
		if run.ErrorCode != nil {
			errorCode = string(*run.ErrorCode)
		}
		if run.CauseSummary != nil {
			causeSummary = *run.CauseSummary
		}
		t.Fatalf("run status=%q want=%q err=%v error_code=%q cause=%q run=%#v", run.Status, want, err, errorCode, causeSummary, run)
	}
}
