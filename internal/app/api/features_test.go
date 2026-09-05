package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/teamcompiler"
	"github.com/labstack/echo/v4"
)

func TestChatRejectsMetaTeamNamesAndIntentsBeforeRegistryLookupWhenDisabled(t *testing.T) {
	cases := []struct {
		name, agent, intent string
	}{
		{name: "architect", agent: metateam.TeamArchitectName},
		{name: "config engineer", agent: metateam.ConfigEngineerName},
		{name: "graph designer", agent: metateam.GraphDesignerName},
		{name: "eval debugger", agent: metateam.EvalDebuggerName},
		{name: "semantic judge", agent: metateam.SemanticJudgeName},
		{name: "patch planner", agent: metateam.BlueprintPatchPlannerName},
		{name: "create intent", agent: "ordinary-agent", intent: "create_team"},
		{name: "optimize intent", agent: "ordinary-agent", intent: "optimize_team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			recorder := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/teams/test/dispatch", nil), recorder)
			ctx.Request().Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			server := &Server{Config: &config.Config{MetaTeamEnabled: false}}
			if err := server.handleChatRequest(ctx, ChatRequest{Agent: tc.agent, Message: "run", Intent: tc.intent, Stream: true}); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"metateam_disabled"`) {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestMetaTeamWorkerDispatchGateWhenDisabled(t *testing.T) {
	runner := &apiLockedWorkerRunner{
		server:   &Server{Config: &config.Config{MetaTeamEnabled: false}},
		snapshot: snapshot.TeamRunSnapshot{WorkspaceID: "workspace-1", RunID: "run-1"},
		workers: []teamcompiler.FrozenTeamWorker{{
			Name: metateam.ConfigEngineerName, WorkerAgentID: "agent-1", WorkerAgentVersion: 1,
		}},
	}
	_, err := runner.StepFor(context.Background(), teamcompiler.LockedWorkerRef{
		WorkspaceID: "workspace-1", RunSnapshotID: "run-1", WorkerAgentID: "agent-1", WorkerAgentVersion: 1,
	})
	if !errors.Is(err, errMetaTeamDisabled) {
		t.Fatalf("StepFor() error = %v", err)
	}
}

func TestMetaTeamRunGateDoesNotMatchReservedPrefix(t *testing.T) {
	server := &Server{Config: &config.Config{MetaTeamEnabled: false}}
	for _, name := range []string{"__graph_designer", "__another_platform_agent", metateam.TeamName} {
		if server.metaTeamRunDisabled(name, "") {
			t.Fatalf("metaTeamRunDisabled(%q) = true", name)
		}
	}
	if !server.metaTeamRunDisabled("ordinary-agent", "create_team") ||
		!server.metaTeamRunDisabled("ordinary-agent", "optimize_team") {
		t.Fatal("meta-team intents were not disabled")
	}
}

func TestTemplateCreationRemainsAvailableWhenMetaTeamDisabled(t *testing.T) {
	e := echo.New()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/teams:from-template", strings.NewReader(
		`{"yaml":"schema: team-template/v1","idempotency_key":"44b79220-51d6-4628-b896-052f2b5a2ea2"}`,
	))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	ctx := e.NewContext(request, recorder)
	server := &Server{
		Config: &config.Config{MetaTeamEnabled: false},
		TeamTemplates: &fakeTeamTemplateService{outcome: teamtemplates.Outcome{
			BuildRunID: "run-1", TeamID: "team-1", Status: "ready", Evaluation: "unevaluated",
		}},
	}
	if err := server.handleCreateTeamFromTemplate(ctx); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"evaluation":"unevaluated"`) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
