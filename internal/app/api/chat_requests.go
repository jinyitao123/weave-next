package api

import (
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/chatrequest"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleGetChatRequest(c echo.Context) error {
	if s.ChatRequests == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "chat request status is unavailable"})
	}
	record, err := s.ChatRequests.Get(
		c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"),
	)
	if errors.Is(err, chatrequest.ErrNotFound) {
		runID, _ := deterministicWorkflowDispatchIDs(getTenant(c), getUserID(c), c.Param("id"))
		record, err = s.ChatRequests.GetWorkflowDispatch(
			c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"), runID,
		)
	}
	if err == nil {
		record, err = s.ChatRequests.AttachWorkflowProgress(c.Request().Context(), record)
	}
	return respondChatRequestStatus(c, record, err)
}

func respondChatRequestStatus(c echo.Context, record chatrequest.Request, err error) error {
	switch {
	case errors.Is(err, chatrequest.ErrNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "chat_request_not_found"})
	case err != nil:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		return c.JSON(http.StatusOK, record)
	}
}
