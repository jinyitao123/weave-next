package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/teamevaluations"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func TestHandleEvaluateTeamStatusSemanticsWhenMetaTeamDisabled(t *testing.T) {
	tests := []struct {
		name       string
		outcome    teamevaluations.Outcome
		err        error
		wantStatus int
		wantBody   string
	}{
		{name: "created", outcome: teamevaluations.Outcome{BuildRunID: "br-1", Status: "authorized"}, wantStatus: http.StatusCreated, wantBody: `"build_run_id":"br-1"`},
		{name: "validation", err: &teamevaluations.ValidationError{Problems: []teamevaluations.Problem{{Path: "/contract", Code: "bad", Message: "bad contract"}}}, wantStatus: http.StatusUnprocessableEntity, wantBody: `"problems"`},
		{name: "active", err: teamevaluations.ErrConcurrentEvaluation, wantStatus: http.StatusConflict, wantBody: `"team_evaluation_active"`},
		{name: "idempotency", err: teamevaluations.ErrIdempotencyConflict, wantStatus: http.StatusConflict, wantBody: `"team_evaluation_idempotency_conflict"`},
		{name: "state", err: teamevaluations.ErrNotUnevaluated, wantStatus: http.StatusConflict, wantBody: `"team_evaluation_state_conflict"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeTeamEvaluationService{outcome: tc.outcome, err: tc.err}
			server := &Server{
				Config: &config.Config{MetaTeamEnabled: false}, TeamEvaluations: service,
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/teams/team-1/evaluations", strings.NewReader(`{"contract":{},"idempotency_key":"e7c03b33-bfac-4a34-a443-805319f7fa24","budget":{"max_cost_usd":5}}`))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			ctx := echo.New().NewContext(request, recorder)
			ctx.SetPath("/v1/teams/:id/evaluations")
			ctx.SetParamNames("id")
			ctx.SetParamValues("team-1")
			ctx.Set("tenant", "workspace-1")
			ctx.Set("user_id", "user-1")
			if err := server.handleEvaluateTeam(ctx); err != nil {
				t.Fatalf("handler error = %v", err)
			}
			if recorder.Code != tc.wantStatus || !strings.Contains(recorder.Body.String(), tc.wantBody) {
				t.Fatalf("status = %d body = %s, want %d containing %s", recorder.Code, recorder.Body.String(), tc.wantStatus, tc.wantBody)
			}
			if service.teamID != "team-1" || service.workspaceID != "workspace-1" || service.userID != "user-1" {
				t.Fatalf("identity = %#v", service)
			}
		})
	}
}

func TestHandleEvaluateTeamRejectsUnknownField(t *testing.T) {
	server := &Server{TeamEvaluations: &fakeTeamEvaluationService{}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"contract":{},"idempotency_key":"x","budget":{"max_cost_usd":5},"extra":true}`))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	ctx := echo.New().NewContext(request, recorder)
	if err := server.handleEvaluateTeam(ctx); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "evaluation_request_invalid") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestTeamEvaluationRouteIsRegistered(t *testing.T) {
	e := echo.New()
	server := &Server{Echo: e, Config: &config.Config{JWTSecret: "test-secret"}}
	server.registerRoutes()
	for _, route := range e.Routes() {
		if route.Method == http.MethodPost && route.Path == "/v1/teams/:id/evaluations" {
			return
		}
	}
	t.Fatal("team evaluation route is not registered")
}

type fakeTeamEvaluationService struct {
	outcome             teamevaluations.Outcome
	err                 error
	workspaceID, userID string
	teamID              string
	request             teamevaluations.Request
}

func (s *fakeTeamEvaluationService) Evaluate(_ context.Context, workspaceID, userID, teamID string, request teamevaluations.Request) (teamevaluations.Outcome, error) {
	s.workspaceID, s.userID, s.teamID, s.request = workspaceID, userID, teamID, request
	return s.outcome, s.err
}

var _ TeamEvaluationService = (*fakeTeamEvaluationService)(nil)
