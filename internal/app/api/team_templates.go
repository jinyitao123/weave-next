package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/teamtemplates"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"github.com/labstack/echo/v4"
)

type TeamTemplateService interface {
	Instantiate(context.Context, string, string, teamtemplates.Request) (teamtemplates.Outcome, error)
	Samples() []teamtemplates.Sample
}

func (s *Server) handleCreateTeamFromTemplate(c echo.Context) error {
	if s.TeamTemplates == nil {
		return workflowError(c, http.StatusServiceUnavailable, "team_template_unavailable", "team template service unavailable")
	}
	var request teamtemplates.Request
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, teamtemplate.MaxYAMLBytes*8))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return templateProblems(c, []teamtemplate.Problem{{Path: "/", Code: "template_request_empty", Message: "request body is required"}})
		}
		return templateProblems(c, []teamtemplate.Problem{{Path: "/", Code: "template_request_invalid", Message: err.Error()}})
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return templateProblems(c, []teamtemplate.Problem{{Path: "/", Code: "template_request_multiple_values", Message: "request must contain exactly one JSON object"}})
	}
	outcome, err := s.TeamTemplates.Instantiate(c.Request().Context(), getTenant(c), getUserID(c), request)
	if err != nil {
		var validation *teamtemplate.ValidationError
		switch {
		case errors.As(err, &validation):
			return templateProblems(c, validation.Problems)
		case errors.Is(err, teamtemplates.ErrIdempotencyConflict):
			return c.JSON(http.StatusConflict, map[string]any{"code": "template_idempotency_conflict", "error": err.Error()})
		case errors.Is(err, teamtemplates.ErrBuildFailed):
			return c.JSON(http.StatusConflict, map[string]any{
				"code": "template_build_failed", "error": err.Error(),
				"build_run_id": outcome.BuildRunID, "status": outcome.Status,
				"progress_url": outcome.ProgressURL,
			})
		case errors.Is(err, teamtemplates.ErrUnavailable):
			return workflowError(c, http.StatusServiceUnavailable, "team_template_unavailable", err.Error())
		default:
			return workflowError(c, http.StatusInternalServerError, "team_template_failed", err.Error())
		}
	}
	if outcome.Status == "ready" {
		return c.JSON(http.StatusCreated, outcome)
	}
	return c.JSON(http.StatusAccepted, outcome)
}

func (s *Server) handleListTeamTemplateSamples(c echo.Context) error {
	if s.TeamTemplates == nil {
		return workflowError(c, http.StatusServiceUnavailable, "team_template_unavailable", "team template service unavailable")
	}
	return c.JSON(http.StatusOK, map[string]any{"samples": s.TeamTemplates.Samples()})
}

func templateProblems(c echo.Context, problems []teamtemplate.Problem) error {
	return c.JSON(http.StatusUnprocessableEntity, map[string]any{"problems": problems})
}
