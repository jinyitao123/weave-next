package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

func TestPublishWorkflowPGDoesNotGateUnevaluatedTeam(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	prefix := "publish-gate-" + uuid.NewString()[:8]
	workspaceID := "workspace-" + prefix
	agents := agentcatalog.New(pool)
	lead := registry.AgentRecord{Name: prefix + "-lead", DisplayName: "负责人", Role: "avatar"}
	worker := registry.AgentRecord{Name: prefix + "-worker", DisplayName: "执行者", Role: "worker"}
	for _, agent := range []*registry.AgentRecord{&lead, &worker} {
		if err := agents.Put(ctx, workspaceID, agent); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}
	orgStore := orgstore.NewStore(pool)
	created, err := orgStore.CreateActiveTeam(ctx, workspaceID, org.CreateActiveTeamInput{
		Name: prefix + "-team", Objective: "完成业务交付", PrimaryScenario: "分析客户材料",
		SuccessCriteria: "交付完整", LeadAvatarID: lead.ID, Evaluation: org.TeamEvaluationUnevaluated,
		Workers: []org.InitialTeamWorker{{
			WorkerAgentID: worker.ID, Duty: "执行", AllowedKinds: []string{"consult"},
			DefaultKind: "consult", ResultRequirement: "形成报告",
		}},
	})
	if err != nil {
		t.Fatalf("create unevaluated team: %v", err)
	}
	workflowStore := workflow.New(pool, workflow.RealClock{})
	record := &workflow.TeamWorkflow{
		WorkspaceID: workspaceID, ID: uuid.NewString(), TeamID: created.Team.ID,
		Name: prefix + "-workflow", Description: "待认证工作流",
	}
	if _, err := workflowStore.Create(ctx, record, workflow.DraftInput{
		TriggerConfig:   []byte(`{"schema_version":1,"type":"manual"}`),
		GraphDefinition: []byte(`{"schema_version":1,"nodes":[],"edges":[]}`), CreatedBy: "admin-1",
	}); err != nil {
		t.Fatalf("create workflow draft: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	echoContext := echo.New().NewContext(request, recorder)
	echoContext.SetPath("/v1/workflows/:id/versions/:version/publish")
	echoContext.SetParamNames("id", "version")
	echoContext.SetParamValues(record.ID, "1")
	echoContext.Set("tenant", workspaceID)
	server := &Server{Workflow: workflowStore, OrgStore: orgStore}
	if err := server.handlePublishWorkflowVersion(echoContext); err != nil {
		t.Fatalf("publish handler error: %v", err)
	}
	if recorder.Code != http.StatusServiceUnavailable ||
		strings.Contains(recorder.Body.String(), `"code":"team_evaluation_required"`) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
