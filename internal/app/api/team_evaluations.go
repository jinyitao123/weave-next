package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/teamevaluations"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/labstack/echo/v4"
)

type TeamEvaluationService interface {
	Evaluate(context.Context, string, string, string, teamevaluations.Request) (teamevaluations.Outcome, error)
}

func (s *Server) handleEvaluateTeam(c echo.Context) error {
	if s.TeamEvaluations == nil {
		return workflowError(c, http.StatusServiceUnavailable, "team_evaluation_unavailable", "team evaluation service unavailable")
	}
	var request teamevaluations.Request
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return c.JSON(http.StatusUnprocessableEntity, map[string]any{"problems": []teamevaluations.Problem{{Path: "/", Code: "evaluation_request_empty", Message: "request body is required"}}})
		}
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{"problems": []teamevaluations.Problem{{Path: "/", Code: "evaluation_request_invalid", Message: err.Error()}}})
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{"problems": []teamevaluations.Problem{{Path: "/", Code: "evaluation_request_multiple_values", Message: "request must contain exactly one JSON object"}}})
	}
	outcome, err := s.TeamEvaluations.Evaluate(
		c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"), request,
	)
	if err != nil {
		var validation *teamevaluations.ValidationError
		switch {
		case errors.As(err, &validation):
			return c.JSON(http.StatusUnprocessableEntity, map[string]any{"problems": validation.Problems})
		case errors.Is(err, teamevaluations.ErrConcurrentEvaluation):
			return workflowError(c, http.StatusConflict, "team_evaluation_active", err.Error())
		case errors.Is(err, teamevaluations.ErrIdempotencyConflict):
			return workflowError(c, http.StatusConflict, "team_evaluation_idempotency_conflict", err.Error())
		case errors.Is(err, teamevaluations.ErrNotUnevaluated):
			return workflowError(c, http.StatusConflict, "team_evaluation_state_conflict", err.Error())
		case errors.Is(err, org.ErrTeamNotFound):
			return workflowError(c, http.StatusNotFound, "team_not_found", err.Error())
		case errors.Is(err, teamevaluations.ErrTemplateLineage):
			return workflowError(c, http.StatusUnprocessableEntity, "team_evaluation_lineage_invalid", err.Error())
		case errors.Is(err, teamevaluations.ErrUnavailable):
			return workflowError(c, http.StatusServiceUnavailable, "team_evaluation_unavailable", err.Error())
		default:
			return workflowError(c, http.StatusInternalServerError, "team_evaluation_failed", err.Error())
		}
	}
	return c.JSON(http.StatusCreated, outcome)
}
