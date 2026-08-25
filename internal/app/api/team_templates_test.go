package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func TestHandleCreateTeamFromTemplateStatusSemantics(t *testing.T) {
	tests := []struct {
		name       string
		outcome    teamtemplates.Outcome
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name: "ready", outcome: teamtemplates.Outcome{
				TeamID: "team-1", BuildRunID: "run-1", Status: "ready", Evaluation: "unevaluated",
			}, wantStatus: http.StatusCreated, wantBody: `"evaluation":"unevaluated"`,
		},
		{
			name: "timeout", outcome: teamtemplates.Outcome{
				BuildRunID: "run-2", Status: "building", ProgressURL: "/v1/internal/team-build-runs/run-2/progress",
			}, wantStatus: http.StatusAccepted, wantBody: `"progress_url"`,
		},
		{
			name: "validation", err: &teamtemplate.ValidationError{Problems: []teamtemplate.Problem{{
				Path: "/name", Code: "template_identifier_invalid", Message: "invalid name",
			}}}, wantStatus: http.StatusUnprocessableEntity, wantBody: `"problems"`,
		},
		{
			name: "idempotency conflict", err: teamtemplates.ErrIdempotencyConflict,
			wantStatus: http.StatusConflict, wantBody: `"template_idempotency_conflict"`,
		},
		{
			name: "terminal failure", outcome: teamtemplates.Outcome{
				BuildRunID: "run-3", Status: "blocked", ProgressURL: "/v1/internal/team-build-runs/run-3/progress",
			}, err: teamtemplates.ErrBuildFailed, wantStatus: http.StatusConflict, wantBody: `"build_run_id":"run-3"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeTeamTemplateService{outcome: tc.outcome, err: tc.err}
			server := &Server{TeamTemplates: service}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/teams:from-template", strings.NewReader(`{"yaml":"x","idempotency_key":"e7c03b33-bfac-4a34-a443-805319f7fa24"}`))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			ctx := echo.New().NewContext(request, recorder)
			ctx.Set("tenant", "workspace-1")
			ctx.Set("user_id", "user-1")
			if err := server.handleCreateTeamFromTemplate(ctx); err != nil {
				t.Fatalf("handler error = %v", err)
			}
			if recorder.Code != tc.wantStatus || !strings.Contains(recorder.Body.String(), tc.wantBody) {
				t.Fatalf("status = %d body = %s, want %d containing %s", recorder.Code, recorder.Body.String(), tc.wantStatus, tc.wantBody)
			}
			if service.workspaceID != "workspace-1" || service.userID != "user-1" {
				t.Fatalf("identity = %q/%q", service.workspaceID, service.userID)
			}
		})
	}
}

func TestHandleCreateTeamFromTemplateRejectsUnknownJSONField(t *testing.T) {
	server := &Server{TeamTemplates: &fakeTeamTemplateService{}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/teams:from-template", strings.NewReader(`{"yaml":"x","idempotency_key":"e7c03b33-bfac-4a34-a443-805319f7fa24","extra":true}`))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	ctx := echo.New().NewContext(request, recorder)
	if err := server.handleCreateTeamFromTemplate(ctx); err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "template_request_invalid") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleCreateTeamFromTemplateAcceptsDeclarativePlan(t *testing.T) {
	service := &fakeTeamTemplateService{outcome: teamtemplates.Outcome{BuildRunID: "run-1", Status: "building"}}
	server := &Server{TeamTemplates: service}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/teams:from-template", strings.NewReader(`{
		"yaml":"x",
		"idempotency_key":"e7c03b33-bfac-4a34-a443-805319f7fa24",
		"declarative_spec":{"schema_version":1,"entry_node_id":"entry","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[],"edges":[]}
	}`))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	ctx := echo.New().NewContext(request, recorder)
	ctx.Set("tenant", "workspace-1")
	ctx.Set("user_id", "user-1")
	if err := server.handleCreateTeamFromTemplate(ctx); err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if recorder.Code != http.StatusAccepted || service.request.DeclarativeSpec == nil || service.request.DeclarativeSpec.EntryNodeID != "entry" {
		t.Fatalf("status = %d request = %#v body = %s", recorder.Code, service.request, recorder.Body.String())
	}
}

func TestHandleListTeamTemplateSamples(t *testing.T) {
	service := &fakeTeamTemplateService{samples: []teamtemplates.Sample{{Name: "research", YAML: "schema: team-template/v1"}}}
	server := &Server{TeamTemplates: service}
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/team-templates/samples", nil), recorder)
	if err := server.handleListTeamTemplateSamples(ctx); err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"research"`) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestTeamTemplateRoutesAreRegistered(t *testing.T) {
	e := echo.New()
	server := &Server{Echo: e, Config: &config.Config{JWTSecret: "test-secret"}}
	server.registerRoutes()
	routes := map[string]bool{}
	for _, route := range e.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodPost + " /v1/teams:from-template",
		http.MethodGet + " /v1/team-templates/samples",
	} {
		if !routes[want] {
			t.Fatalf("route %q is not registered", want)
		}
	}
}

type fakeTeamTemplateService struct {
	outcome     teamtemplates.Outcome
	err         error
	samples     []teamtemplates.Sample
	workspaceID string
	userID      string
	request     teamtemplates.Request
}

func (s *fakeTeamTemplateService) Instantiate(_ context.Context, workspaceID, userID string, request teamtemplates.Request) (teamtemplates.Outcome, error) {
	s.workspaceID = workspaceID
	s.userID = userID
	s.request = request
	return s.outcome, s.err
}

func (s *fakeTeamTemplateService) Samples() []teamtemplates.Sample {
	return s.samples
}

var _ TeamTemplateService = (*fakeTeamTemplateService)(nil)
