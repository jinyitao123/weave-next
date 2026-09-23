package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/kernelbindings"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
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
	ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, UserID: userID})
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
	if err := agentcatalog.New(pool).Put(ctx, workspaceID, &lead); err != nil {
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
	artifacts := workflow.NewArtifactStore(pool, workflow.RealClock{})
	workflowStore := workflowcatalog.New(pool, workflow.RealClock{}, artifacts)
	createdWorkflow, err := workflowStore.Create(ctx, &workflow.TeamWorkflow{
		WorkspaceID: workspaceID, ID: workflowID, TeamID: teamID,
		Name: "sample-4-human-final-review", Description: sample.Description,
	}, workflow.DraftInput{
		TriggerConfig: frozenSpec.TriggerConfig, GraphDefinition: frozenSpec.GraphDefinition, CreatedBy: "sample-4",
	})
	if err != nil {
		t.Fatalf("create sample 4 workflow draft: %v", err)
	}
	descriptors := compiler.NewDescriptorRegistry()
	if err := descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
		t.Fatal(err)
	}
	builder := workflowcatalog.NewCandidateBuilder(workflowStore, agentcatalog.New(pool), delivery.New(pool, []byte(strings.Repeat("h", 32))), skills.New(pool), credentials.New(pool, []byte(strings.Repeat("h", 32))), schedule.New(pool, nil), descriptors)
	authority, publications := openAPIProductPublication(t, ctx, pool, builder)
	allowAPITestCandidateAssociation(publications)
	publicationTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, report, err := authority.BuildCandidateTx(ctx, publicationTx, workflow.CandidateInput{WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: 1})
	if err != nil || candidate == nil || report != nil && len(report.Issues) != 0 {
		_ = publicationTx.Rollback(ctx)
		t.Fatalf("build sample 4 publication: candidate=%v report=%+v err=%v", candidate != nil, report, err)
	}
	if err := publicationTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	command, err := teamconstruction.PublicationCommandForCandidate("sample-4-publication-"+prefix, candidate, teamconstruction.PublicationTarget{TeamID: teamID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publications.Publish(ctx, command); err != nil {
		t.Fatalf("publish sample 4 workflow artifact: %v", err)
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
	record, err := publications.AdmitCandidate(ctx, teamconstruction.CandidateTarget{BuildRunID: buildRunID, RoundNo: 1, SourceRole: "human-review"}, publication.CandidateRunRequest{
		Version: publication.ContractVersion, RequestID: "sample-4-admission-" + prefix,
		Candidate: command.Request.Candidate, Input: json.RawMessage(`"deliverable_ref:artifact-m3-final"`), InputVersion: "v1", SourceRef: buildRunID, Purpose: "human-final-review",
	})
	if err != nil {
		t.Fatalf("admit sample 4 candidate run: %v", err)
	}
	if record.Receipt == nil {
		t.Fatal("sample 4 admission receipt missing")
	}
	snapshots := snapshot.NewStore(pool)
	runID := record.Receipt.RunID

	tasks := taskqueue.New(pool, taskqueue.RealClock{}, time.Minute)

	runs, checkpoints := teamrun.NewPGStore(), teamrun.NewPGCheckpointStore()
	runtime := &teamrun.WorkflowSerialRuntime{
		Artifacts: artifacts, Loader: &workflow.RuntimeLoader{},
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
	server := &Server{OrgStore: kernelbindings.NewOrganization(pool), teamRunHumanTasks: reader, teamRunHumanResume: resume}
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
	if strings.Contains(listRecorder.Body.String(), "deliverable_ref:artifact-m3-final") ||
		strings.Contains(listRecorder.Body.String(), "predecessor_outputs") {
		t.Fatalf("human task list leaked predecessor output: %s", listRecorder.Body.String())
	}
	detail := getHumanTaskThroughAPI(t, server, workspaceID, userID, runID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"predecessor_outputs":{"draft":"deliverable_ref:artifact-m3-final"}`) {
		t.Fatalf("human task detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var question humanTaskDetailResponse
	if err := json.Unmarshal(detail.Body.Bytes(), &question); err != nil || question.InteractionID == "" || question.NodeID == "" ||
		question.InteractionID != inbox.Tasks[0].InteractionID || question.NodeID != inbox.Tasks[0].NodeID {
		t.Fatalf("human question identity missing or inconsistent: detail=%#v inbox=%#v error=%v", question, inbox.Tasks, err)
	}
	chapterPath := "/predecessor_outputs/chapters/第一~1章~0草稿"
	longChapter := strings.Repeat("长", humanTaskGetMaxBytes)
	checkpointTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := checkpoints.GetTx(ctx, checkpointTx, workspaceID, runID)
	if err != nil {
		_ = checkpointTx.Rollback(ctx)
		t.Fatal(err)
	}
	chapters, _ := json.Marshal(map[string]string{"第一/章~草稿": "短稿", "长章": longChapter})
	checkpoint.CompletedOutputs["chapters"] = chapters
	checkpoint.WrittenAt = time.Now().UTC()
	if _, err := checkpoints.PutTx(ctx, checkpointTx, checkpoint); err != nil {
		_ = checkpointTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := checkpointTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tooLarge := getHumanTaskThroughAPI(t, server, workspaceID, userID, runID, "")
	if tooLarge.Code != http.StatusRequestEntityTooLarge || !strings.Contains(tooLarge.Body.String(), "human_task_value_too_large") {
		t.Fatalf("oversize detail status=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}
	chapter := getHumanTaskThroughAPI(t, server, workspaceID, userID, runID, "?path="+url.QueryEscape(chapterPath))
	if chapter.Code != http.StatusOK || strings.TrimSpace(chapter.Body.String()) != `"短稿"` {
		t.Fatalf("chapter selection status=%d body=%s", chapter.Code, chapter.Body.String())
	}
	page := getHumanTaskThroughAPI(t, server, workspaceID, userID, runID,
		"?path="+url.QueryEscape("/predecessor_outputs/chapters/长章")+"&offset=10&limit=1000")
	if page.Code != http.StatusOK || page.Header().Get("X-Weave-Page-Total") != strconv.Itoa(humanTaskGetMaxBytes) {
		t.Fatalf("chapter page status=%d headers=%v body_bytes=%d", page.Code, page.Header(), page.Body.Len())
	}

	invalidBody, err := json.Marshal(completeHumanTaskRequest{
		InteractionID: question.InteractionID, Payload: json.RawMessage(`{"decision":"maybe","comments":"invalid"}`),
		IdempotencyKey: "m3-invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	invalid := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID, string(invalidBody))
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid resume payload status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	validJSON, err := json.Marshal(completeHumanTaskRequest{Payload: json.RawMessage(`{"decision":"approve","comments":"终审通过"}`),
		IdempotencyKey: "m3-complete", InteractionID: question.InteractionID})
	if err != nil {
		t.Fatal(err)
	}
	validBody := string(validJSON)
	var tasksBeforeMissing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE workspace_id=$1`, workspaceID).Scan(&tasksBeforeMissing); err != nil {
		t.Fatal(err)
	}
	missingInteraction := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID,
		`{"payload":{"decision":"approve","comments":"missing identity"},"idempotency_key":"m3-missing-interaction"}`)
	if missingInteraction.Code != http.StatusBadRequest {
		t.Fatalf("missing interaction identity status=%d body=%s", missingInteraction.Code, missingInteraction.Body.String())
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusParked)
	var tasksAfterMissing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE workspace_id=$1`, workspaceID).Scan(&tasksAfterMissing); err != nil || tasksAfterMissing != tasksBeforeMissing {
		t.Fatalf("missing interaction identity changed queue: before=%d after=%d error=%v", tasksBeforeMissing, tasksAfterMissing, err)
	}
	stale := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID,
		`{"payload":{"decision":"approve","comments":"old question"},"idempotency_key":"m3-stale","interaction_id":"human_old_question"}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale question accepted: status=%d body=%s", stale.Code, stale.Body.String())
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusParked)
	outsider := "outsider-" + prefix
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users (id,tenant_id,username,password,role)
		VALUES ($1,$2,$1,'x','user')`, outsider, workspaceID); err != nil {
		t.Fatal(err)
	}
	var tasksBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE workspace_id=$1`, workspaceID).Scan(&tasksBefore); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []struct {
		method, path string
		handler      echo.HandlerFunc
	}{
		{http.MethodGet, "/v1/human-tasks", server.handleListHumanTasks},
		{http.MethodGet, "/v1/human-tasks/" + runID + "?path=/title", server.handleGetHumanTask},
		{http.MethodPost, "/v1/human-tasks/" + runID + "/complete", server.handleCompleteHumanTask},
	} {
		recorder := httptest.NewRecorder()
		request := humanTaskAPIContext(denied.method, denied.path, validBody, recorder, workspaceID, outsider)
		request.SetParamNames("run_id")
		request.SetParamValues(runID)
		if err := denied.handler(request); err != nil {
			request.Echo().HTTPErrorHandler(err, request)
		}
		var response map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusForbidden || len(response) != 1 || response["error"] != "current workspace membership required" {
			t.Fatalf("outsider reached %s: status=%d body=%s error=%v", denied.path, recorder.Code, recorder.Body.String(), err)
		}
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusParked)
	var tasksAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE workspace_id=$1`, workspaceID).Scan(&tasksAfter); err != nil || tasksAfter != tasksBefore {
		t.Fatalf("outsider enqueued human continuation: before=%d after=%d error=%v", tasksBefore, tasksAfter, err)
	}
	completed := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID, validBody)
	if completed.Code != http.StatusAccepted {
		t.Fatalf("complete human task status=%d body=%s", completed.Code, completed.Body.String())
	}
	conflictJSON, err := json.Marshal(completeHumanTaskRequest{
		InteractionID:  question.InteractionID,
		Payload:        json.RawMessage(`{"decision":"reject","comments":"changed"}`),
		IdempotencyKey: "m3-complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID, string(conflictJSON))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same key different payload status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	processed, err = executor.ProcessNext(ctx, "m3-worker-resume")
	if err != nil || !processed {
		t.Fatalf("process queued human continuation: processed=%v err=%v", processed, err)
	}
	assertHumanRunStatus(t, ctx, pool, runs, workspaceID, runID, teamrun.StatusSucceeded)
	replay := completeHumanTaskThroughAPI(t, server, workspaceID, userID, runID, validBody)
	if replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), `"idempotent":true`) {
		t.Fatalf("lost successful response did not replay: status=%d body=%s", replay.Code, replay.Body.String())
	}
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

func getHumanTaskThroughAPI(t *testing.T, server *Server, workspaceID, userID, runID, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx := humanTaskAPIContext(http.MethodGet, "/v1/human-tasks/"+runID+query, "", recorder, workspaceID, userID)
	ctx.SetPath("/v1/human-tasks/:run_id")
	ctx.SetParamNames("run_id")
	ctx.SetParamValues(runID)
	if err := server.handleGetHumanTask(ctx); err != nil {
		t.Fatalf("get human task handler: %v", err)
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
