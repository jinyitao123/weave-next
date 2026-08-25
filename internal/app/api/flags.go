package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleFlagMessage(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	created, err := s.Conversations.FlagMessage(
		c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"),
	)
	if errors.Is(err, conversation.ErrMessageNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "message not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.JSON(status, map[string]bool{"flagged": true})
}

func (s *Server) handleUnflagMessage(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	if err := s.Conversations.UnflagMessage(
		c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"),
	); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleListFlags(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	limit := flagLimit(c.QueryParam("limit"))
	items, hasMore, err := s.Conversations.ListFlaggedMessages(
		c.Request().Context(), getTenant(c), getUserID(c), limit,
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "has_more": hasMore})
}

func (s *Server) handleCountFlags(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	count, err := s.Conversations.CountFlaggedMessages(
		c.Request().Context(), getTenant(c), getUserID(c),
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]int{"count": count})
}

func flagLimit(raw string) int {
	if raw == "" {
		return 30
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit == 0 {
		return 30
	}
	if limit < 1 {
		return 1
	}
	if limit > 100 {
		return 100
	}
	return limit
}
