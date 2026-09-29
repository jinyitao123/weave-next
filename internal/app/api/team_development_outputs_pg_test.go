package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func TestDevelopmentTrialSerialOutputActivityRealPG(t *testing.T) {
	fixture := newDevelopmentTrialOutputFixture(t, 1)
	worker := fixture.workers[0]
	fixture.recordMemberEvents(t, worker, "serial-step")
	recorder := &developmentTrialWorkflowOutputRecorder{pool: fixture.pool, fallback: fixture.server.Deliverables}
	if err := recorder.RecordWorkflowOutput(fixture.ctx, deliverable.WorkflowOutput{
		WorkspaceID: fixture.workspaceID, RunID: fixture.runID, RunSnapshotID: fixture.runID,
		NodeID: "serial-step", NodeLabel: "串行核对", NodeType: "worker", AgentID: worker.ID,
		Output: "串行步骤的真实输出", CreatedAt: fixture.now,
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.activity(t, fixture.actorID)
	if got := response.Completeness["member_outputs"]; got != "complete" {
		t.Fatalf("member_outputs completeness = %q, want complete", got)
	}
	if got := response.Completeness["stages"]; got != "complete" {
		t.Fatalf("stages completeness = %q, want complete", got)
	}
	stage := developmentTrialMemberStage(t, response.Members, worker.ID, "serial-step")
	if stage.Status != "completed" || len(stage.Outputs) != 1 || stage.Outputs[0].Content != "串行步骤的真实输出" || stage.Outputs[0].Truncated {
		t.Fatalf("serial stage output = %#v", stage)
	}
	if len(response.Stages) != 1 || response.Stages[0].NodeID != "serial-step" || response.Stages[0].Outputs[0].Content != "串行步骤的真实输出" {
		t.Fatalf("top-level serial stages = %#v", response.Stages)
	}
	fixture.assertNoFormalDeliverables(t)
	fixture.assertActivityDeniedToOtherDeveloper(t)
}

func TestDevelopmentTrialParallelJoinOutputsActivityRealPG(t *testing.T) {
	fixture := newDevelopmentTrialOutputFixture(t, 2)
	left, right := fixture.workers[0], fixture.workers[1]
	fixture.recordMemberEvents(t, left, "left-branch")
	fixture.recordMemberEvents(t, right, "right-branch")
	recorder := &developmentTrialWorkflowOutputRecorder{pool: fixture.pool, fallback: fixture.server.Deliverables}
	outputs := []deliverable.WorkflowOutput{
		{WorkspaceID: fixture.workspaceID, RunID: fixture.runID, RunSnapshotID: fixture.runID,
			NodeID: "left-branch", NodeLabel: "左侧检查", NodeType: "worker", AgentID: left.ID,
			Output: "左侧分支输出", CreatedAt: fixture.now},
		{WorkspaceID: fixture.workspaceID, RunID: fixture.runID, RunSnapshotID: fixture.runID,
			NodeID: "right-branch", NodeLabel: "右侧检查", NodeType: "worker", AgentID: right.ID,
			Output: "右侧分支输出", CreatedAt: fixture.now.Add(time.Millisecond)},
		{WorkspaceID: fixture.workspaceID, RunID: fixture.runID, RunSnapshotID: fixture.runID,
			NodeID: "join", NodeLabel: "并行汇合", NodeType: "join",
			Output:    map[string]any{"decision": "succeeded", "branches": []string{"left-branch", "right-branch"}},
			CreatedAt: fixture.now.Add(2 * time.Millisecond)},
	}
	for _, output := range outputs {
		if err := recorder.RecordWorkflowOutput(fixture.ctx, output); err != nil {
			t.Fatal(err)
		}
	}

	response := fixture.activity(t, fixture.actorID)
	if got := response.Completeness["member_outputs"]; got != "complete" {
		t.Fatalf("member_outputs completeness = %q, want complete", got)
	}
	if got := response.Completeness["stages"]; got != "complete" {
		t.Fatalf("stages completeness = %q, want complete", got)
	}
	if stage := developmentTrialMemberStage(t, response.Members, left.ID, "left-branch"); len(stage.Outputs) != 1 || stage.Outputs[0].Content != "左侧分支输出" {
		t.Fatalf("left branch output = %#v", stage)
	}
	if stage := developmentTrialMemberStage(t, response.Members, right.ID, "right-branch"); len(stage.Outputs) != 1 || stage.Outputs[0].Content != "右侧分支输出" {
		t.Fatalf("right branch output = %#v", stage)
	}
	join := developmentTrialTopLevelStage(t, response.Stages, "join")
	if len(join.Outputs) != 1 || !strings.Contains(join.Outputs[0].Content, `"decision": "succeeded"`) {
		t.Fatalf("parallel join output = %#v", join)
	}
	if completed, total := runActivityStageProgress(response.Stages); completed != 3 || total != 3 {
		t.Fatalf("parallel stage progress = %d/%d, want 3/3", completed, total)
	}
	fixture.assertNoFormalDeliverables(t)
	fixture.assertActivityDeniedToOtherDeveloper(t)
}

func TestDevelopmentTrialTruncatedOutputRemainsPartial(t *testing.T) {
	fixture := newDevelopmentTrialOutputFixture(t, 1)
	worker := fixture.workers[0]
	fixture.recordMemberEvents(t, worker, "large-step")
	content := strings.Repeat("x", developmentTrialStageOutputMaxBytes+1)
	recorder := &developmentTrialWorkflowOutputRecorder{pool: fixture.pool, fallback: fixture.server.Deliverables}
	if err := recorder.RecordWorkflowOutput(fixture.ctx, deliverable.WorkflowOutput{
		WorkspaceID: fixture.workspaceID, RunID: fixture.runID, RunSnapshotID: fixture.runID,
		NodeID: "large-step", NodeLabel: "较大输出", NodeType: "worker", AgentID: worker.ID,
		Output: content, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatal(err)
	}
	response := fixture.activity(t, fixture.actorID)
	if response.Completeness["member_outputs"] != "partial" || response.Completeness["stages"] != "partial" {
		t.Fatalf("truncated output was reported complete: %#v", response.Completeness)
	}
	stage := developmentTrialMemberStage(t, response.Members, worker.ID, "large-step")
	if len(stage.Outputs) != 1 || !stage.Outputs[0].Truncated || stage.Outputs[0].ContentBytes != int64(len(content)) {
		t.Fatalf("truncated output metadata = %#v", stage.Outputs)
	}
	fixture.assertNoFormalDeliverables(t)
}

type developmentTrialOutputFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	server      *Server
	workspaceID string
	actorID     string
	teamID      string
	requestID   string
	runID       string
	workers     []registry.AgentRecord
	now         time.Time
}

func newDevelopmentTrialOutputFixture(t *testing.T, workerCount int) *developmentTrialOutputFixture {
	t.Helper()
	const workspaceID, actorID = "trial-output-workspace", "developer"
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: workspaceID, UserID: actorID})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Trial outputs');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES($2,$1,$2,'unused','admin')`, workspaceID, actorID); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: "trial-lead", DisplayName: "负责人", Role: "avatar", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Coordinate"}}
	if err := agents.Put(ctx, workspaceID, &lead); err != nil {
		t.Fatal(err)
	}
	workers := make([]registry.AgentRecord, 0, workerCount)
	initialWorkers := make([]org.InitialTeamWorker, 0, workerCount)
	for index := 0; index < workerCount; index++ {
		worker := registry.AgentRecord{Name: fmt.Sprintf("trial-worker-%d", index+1), DisplayName: fmt.Sprintf("成员%d", index+1), Role: "worker", Engine: "loom", Model: "fixture-model", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Return a concrete step result"}}
		if err := agents.Put(ctx, workspaceID, &worker); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, worker)
		initialWorkers = append(initialWorkers, org.InitialTeamWorker{WorkerAgentID: worker.ID, Duty: worker.DisplayName, AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "Return step output"})
	}
	created, err := orgstore.NewStore(pool).CreateActiveTeam(ctx, workspaceID, org.CreateActiveTeamInput{
		Name: "Trial output team", Objective: "Capture candidate step outputs", LeadAvatarID: lead.ID, Workers: initialWorkers,
	})
	if err != nil {
		t.Fatal(err)
	}

	requestID, runID, taskID := uuid.NewString(), "run-"+uuid.NewString(), "task-"+uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	frozenWorkers := make([]snapshot.FrozenTeamWorker, 0, len(workers))
	for _, worker := range workers {
		frozenWorkers = append(frozenWorkers, snapshot.FrozenTeamWorker{
			SchemaVersion: snapshot.TeamWorkerSnapshotSchemaVersion, WorkerAgentID: worker.ID,
			WorkerAgentVersion: worker.Version, Name: worker.DisplayName, Duty: worker.DisplayName,
			AllowedKinds: []string{"consult"}, DefaultKind: "consult", ResultRequirement: "Return step output",
			EnabledAtSnapshot: true, RoleProof: snapshot.FrozenWorkerRoleProof{
				Role: "worker", AgentContentHash: strings.Repeat("a", 64), CapabilitySchema: 2,
				CapabilityContentHash: strings.Repeat("b", 64),
			},
		})
	}
	workerSnapshot, workerVersions, err := snapshot.EncodeTeamWorkerSnapshot(frozenWorkers)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) == 0 {
		workerSnapshot, workerVersions = json.RawMessage(`[]`), json.RawMessage(`{}`)
	}
	triggerSource := "team-development:" + created.Team.ID
	snap := snapshot.TeamRunSnapshot{
		Subject: execution.Subject{WorkspaceID: workspaceID, UserID: actorID}, RunID: runID, WorkspaceID: workspaceID,
		TeamID: created.Team.ID, SnapshotSchemaVersion: 2, Mode: "free_collab",
		LeadAvatarID: lead.ID, LeadAvatarVersion: lead.Version,
		WorkerVersions: workerVersions, TeamWorkerSnapshot: workerSnapshot, InlineDependencies: json.RawMessage(`{}`),
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-29T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
		TriggerSourceV2:   json.RawMessage(fmt.Sprintf(`{"schema_version":1,"type":"api","source_ref":%q}`, triggerSource)), SourceRef: triggerSource,
		CreatedAt: now,
	}
	if _, err := snapshot.NewStore(pool).Create(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,source,status,kind,context_key,payload,run_id,source_ref,started_at,completed_at)
		VALUES($1,$2,'trial-lead','api','completed','team_workflow',$3,'{}'::jsonb,$4,$5,$6,$6)`,
		taskID, workspaceID, "development:"+requestID, runID, triggerSource, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_run_generation,execution_lease_epoch,resume_generation,
		team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at,terminal_at)
		VALUES($1,$2,'succeeded',0,0,0,$3,'trial-workflow',1,$2,'api',$4,$5,$6,$6,$6)`,
		workspaceID, runID, created.Team.ID, taskID, "trial-output:"+requestID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_development_trials(workspace_id,team_id,request_id,revision,workflow_id,actor_id,request_digest,request,receipt)
		VALUES($1,$2,$3::uuid,7,'trial-workflow',$4,$5,'{}'::jsonb,jsonb_build_object('run_id',$6))`,
		workspaceID, created.Team.ID, requestID, actorID, strings.Repeat("c", 64), runID); err != nil {
		t.Fatal(err)
	}

	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	deliverables := deliverable.New(pool)
	server := &Server{
		Store: teamDeliveryPoolStore{teamDispatchPoolStore{pool: pool}}, Pool: pool,
		Snapshots: snapshot.NewStore(pool), Deliverables: deliverables,
		teamRunCancel:     &teamrun.CancelService{Transactions: pool, Runs: runs},
		teamRunActivities: &teamrun.PGActivityStore{Transactions: pool},
	}
	return &developmentTrialOutputFixture{
		ctx: ctx, pool: pool, server: server,
		workspaceID: workspaceID, actorID: actorID, teamID: created.Team.ID,
		requestID: requestID, runID: runID, workers: workers, now: now,
	}
}

func (fixture *developmentTrialOutputFixture) recordMemberEvents(t *testing.T, worker registry.AgentRecord, nodeID string) {
	t.Helper()
	started := fixture.now.Add(time.Second)
	completed := started.Add(time.Second)
	store := &teamrun.PGActivityStore{Transactions: fixture.pool}
	for _, event := range []teamrun.ActivityEvent{
		{WorkspaceID: fixture.workspaceID, RunID: fixture.runID, Kind: "member_started", NodeID: nodeID,
			MemberID: worker.ID, MemberVersion: int64(worker.Version), Detail: json.RawMessage(`{"input_summary":{"task":"fixed trial input"}}`), OccurredAt: started},
		{WorkspaceID: fixture.workspaceID, RunID: fixture.runID, Kind: "member_completed", NodeID: nodeID,
			MemberID: worker.ID, MemberVersion: int64(worker.Version), Detail: json.RawMessage(`{"duration_ms":1000,"tool_calls":0}`), OccurredAt: completed},
	} {
		if err := store.Record(fixture.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *developmentTrialOutputFixture) activity(t *testing.T, actorID string) struct {
	Completeness map[string]string   `json:"completeness"`
	Members      []runActivityMember `json:"members"`
	Stages       []runActivityStage  `json:"stages"`
} {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+fixture.runID+"/activity", nil).WithContext(fixture.ctx)
	ctx := echo.New().NewContext(request, response)
	ctx.Set("tenant", fixture.workspaceID)
	ctx.Set("user_id", actorID)
	ctx.SetParamNames("id")
	ctx.SetParamValues(fixture.runID)
	err := fixture.server.handleGetRunActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Completeness map[string]string   `json:"completeness"`
		Members      []runActivityMember `json:"members"`
		Stages       []runActivityStage  `json:"stages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func (fixture *developmentTrialOutputFixture) assertNoFormalDeliverables(t *testing.T) {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM weave_final_deliverables WHERE workspace_id=$1 AND run_id=$2`, fixture.workspaceID, fixture.runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("development trial created %d formal deliverables", count)
	}
}

func (fixture *developmentTrialOutputFixture) assertActivityDeniedToOtherDeveloper(t *testing.T) {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+fixture.runID+"/activity", nil).WithContext(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: fixture.workspaceID, UserID: "other-developer"}))
	ctx := echo.New().NewContext(request, response)
	ctx.Set("tenant", fixture.workspaceID)
	ctx.Set("user_id", "other-developer")
	ctx.SetParamNames("id")
	ctx.SetParamValues(fixture.runID)
	if err := fixture.server.handleGetRunActivity(ctx); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("other developer activity status=%d, want 404", response.Code)
	}
}

func developmentTrialMemberStage(t *testing.T, members []runActivityMember, memberID, nodeID string) runActivityMemberStage {
	t.Helper()
	for _, member := range members {
		if member.AgentID != memberID {
			continue
		}
		for _, stage := range member.Stages {
			if stage.NodeID == nodeID {
				return stage
			}
		}
	}
	t.Fatalf("member stage %s/%s not found: %#v", memberID, nodeID, members)
	return runActivityMemberStage{}
}

func developmentTrialTopLevelStage(t *testing.T, stages []runActivityStage, nodeID string) runActivityStage {
	t.Helper()
	for _, stage := range stages {
		if stage.NodeID == nodeID {
			return stage
		}
	}
	t.Fatalf("top-level stage %s not found: %#v", nodeID, stages)
	return runActivityStage{}
}
