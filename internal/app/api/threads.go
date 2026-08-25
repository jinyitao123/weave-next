package api

import (
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleListThreads(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	workspaceID := getTenant(c)
	conversationID := c.Param("id")
	if _, err := s.Conversations.GetConversation(c.Request().Context(), workspaceID, conversationID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation not found"})
	}
	summaries, err := s.Conversations.ListThreads(c.Request().Context(), workspaceID, conversationID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"thread_summaries": summaries})
}

func (s *Server) handleCreateThread(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	var body struct {
		ParentMessageID string `json:"parent_message_id"`
	}
	if err := c.Bind(&body); err != nil || body.ParentMessageID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "parent_message_id is required"})
	}
	thread, created, err := s.Conversations.CreateThread(
		c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c), body.ParentMessageID,
	)
	if errors.Is(err, conversation.ErrConversationNotFound) || errors.Is(err, conversation.ErrMessageNotFound) {
		if parent, getErr := s.Conversations.GetConversation(c.Request().Context(), getTenant(c), c.Param("id")); getErr == nil && parent.UserID != getUserID(c) {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "conversation_read_only"})
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation or parent message not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.JSON(status, map[string]string{"thread_id": thread.ID})
}
