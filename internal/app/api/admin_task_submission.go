package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// Console submissions use the ordinary published-team admission path.
type adminTaskSubmission struct {
	TeamID          string `json:"team_id"`
	Task            string `json:"task"`
	ClientRequestID string `json:"client_request_id"`
}

func (s *Server) handleSubmitAdminTask(c echo.Context) error {
	if s.GetPool() == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "task_submission_unavailable"})
	}
	var submission adminTaskSubmission
	if err := decodeWorkflowBody(c, &submission); err != nil {
		return workflowSchemaError(c)
	}
	submission.TeamID = strings.TrimSpace(submission.TeamID)
	if submission.TeamID == "" || strings.TrimSpace(submission.Task) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "team_and_task_required"})
	}
	if _, err := uuid.Parse(submission.ClientRequestID); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_request_id"})
	}
	c.SetParamNames("id")
	c.SetParamValues(submission.TeamID)
	if ok, err := s.ensureTeamAvailable(c, getTenant(c), submission.TeamID); !ok {
		return err
	}
	return s.dispatchAdmittedTeam(c, teamDispatchRequest{Task: submission.Task, ClientRequestID: submission.ClientRequestID})
}

func runTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "abandoned":
		return true
	}
	return false
}
