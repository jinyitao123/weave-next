package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleListFinalDeliverables(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	items, err := s.Deliverables.List(c.Request().Context(), getTenant(c), deliverable.ListFilter{
		ProjectID:      c.QueryParam("project_id"),
		ConversationID: c.QueryParam("conversation_id"),
		RunID:          c.QueryParam("run_id"),
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"deliverables": items})
}

func (s *Server) handleGetFinalDeliverable(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	item, err := s.Deliverables.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	if c.QueryParam("path") != "" || c.QueryParam("offset") != "" || c.QueryParam("limit") != "" {
		var content any
		decoder := json.NewDecoder(strings.NewReader(item.Content))
		decoder.UseNumber()
		if err := decoder.Decode(&content); err != nil {
			content = item.Content
		}
		return writeSelectedJSONResponse(c, content, "deliverable")
	}
	return c.JSON(http.StatusOK, item)
}

func (s *Server) handlePromoteMessageDeliverable(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	var body struct {
		Title string `json:"title"`
	}
	if c.Request().Body != nil {
		// The body is optional; an empty body promotes with the default title.
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		}
	}
	item, created, err := s.Deliverables.PromoteMessage(
		c.Request().Context(), getTenant(c), getUserID(c), c.Param("id"), body.Title,
	)
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "message_not_found"})
	}
	if errors.Is(err, deliverable.ErrNotPromotable) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "message_not_promotable"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "final_deliverable_promote_failed"})
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.JSON(status, item)
}

func (s *Server) handleGetConversationDeliverable(c echo.Context) error {
	if s.Deliverables == nil || s.Conversations == nil {
		return finalDeliverableUnavailable(c)
	}
	conversationID := c.Param("id")
	conversation, err := s.Conversations.GetConversation(
		c.Request().Context(), getTenant(c), conversationID,
	)
	if err != nil || conversation.UserID != getUserID(c) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
	}
	item, err := s.Deliverables.LatestForConversation(
		c.Request().Context(), getTenant(c), conversationID,
	)
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	return c.JSON(http.StatusOK, item)
}

func (s *Server) handleDownloadFinalDeliverable(c echo.Context) error {
	if s.Deliverables == nil {
		return finalDeliverableUnavailable(c)
	}
	item, err := s.Deliverables.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return finalDeliverableFailure(c, err)
	}
	extension := "md"
	switch {
	case strings.HasPrefix(item.ContentType, "image/svg+xml") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.Content)), "<svg"):
		extension = "svg"
	case deliverable.LooksLikeHTMLDocument(item.Content) || strings.HasPrefix(item.ContentType, "text/html"):
		extension = "html"
	case strings.HasPrefix(item.ContentType, "application/json"):
		extension = "json"
	case strings.HasPrefix(item.ContentType, "text/plain"):
		extension = "txt"
	}
	filename := "weave-deliverable-" + item.ID + "." + extension
	c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", filename))
	return c.Blob(http.StatusOK, item.ContentType+"; charset=utf-8", []byte(item.Content))
}

func finalDeliverableUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "final_deliverables_unavailable"})
}

func finalDeliverableFailure(c echo.Context, err error) error {
	if errors.Is(err, deliverable.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "final_deliverable_not_found"})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": "final_deliverable_read_failed"})
}
