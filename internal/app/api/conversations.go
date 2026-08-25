package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	conversationstore "github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleRenameConversation(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	conversation, err := s.Conversations.RenameConversation(
		c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c), body.Title,
	)
	switch {
	case errors.Is(err, conversationstore.ErrInvalidConversationTitle):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_conversation_title"})
	case errors.Is(err, conversationstore.ErrConversationThreadImmutable):
		return c.JSON(http.StatusConflict, map[string]string{"error": "conversation_thread_immutable"})
	case errors.Is(err, conversationstore.ErrConversationNotFound):
		if existing, getErr := s.Conversations.GetConversation(c.Request().Context(), getTenant(c), c.Param("id")); getErr == nil && existing.UserID != getUserID(c) {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "conversation_read_only"})
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
	case err != nil:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		return c.JSON(http.StatusOK, conversation)
	}
}

func (s *Server) handleListConversations(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	projectID := strings.TrimSpace(c.QueryParam("project_id"))
	var conversations []conversationstore.Conversation
	var err error
	if projectID == "" {
		conversations, err = s.Conversations.ListConversations(c.Request().Context(), getTenant(c), getUserID(c))
	} else {
		conversations, err = s.Conversations.ListProjectConversations(c.Request().Context(), getTenant(c), projectID)
		markReadOnlyConversations(conversations, getUserID(c))
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if s.TeamBuild != nil {
		conversationstore.AttachBuildRunSummaries(c.Request().Context(), conversations, s.TeamBuild)
	}
	return c.JSON(http.StatusOK, conversations)
}

func (s *Server) handleListConversationMessages(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	workspaceID := getTenant(c)
	conversationID := c.Param("id")
	if _, err := s.Conversations.GetConversation(c.Request().Context(), workspaceID, conversationID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation not found"})
	}

	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var messages []conversationstore.Message
	var err error
	before := c.QueryParam("before")
	if before != "" {
		messages, err = s.Conversations.ListMessagesBefore(
			c.Request().Context(), workspaceID, conversationID, before, limit,
		)
	} else {
		messages, err = s.Conversations.ListMessages(
			c.Request().Context(), workspaceID, conversationID, limit, offset,
		)
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	compactTimelineMessageMetadata(messages)
	return c.JSON(http.StatusOK, messages)
}

const (
	timelineExecutionFieldMaxRunes = 320
	timelineToolFieldMaxRunes      = 160
	timelineTextSegmentMaxRunes    = 240
	timelineMaxToolCalls           = 4
	timelineMaxExecutionSegments   = 4
)

func compactTimelineMessageMetadata(messages []conversationstore.Message) {
	for index := range messages {
		messages[index].Metadata = compactTimelineMetadata(messages[index].Metadata)
	}
}

func compactTimelineMetadata(metadata json.RawMessage) json.RawMessage {
	if len(metadata) == 0 {
		return metadata
	}
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return metadata
	}
	if execution, ok := fields["execution"].(map[string]any); ok {
		compactExecutionProjection(execution)
	}
	if segments, ok := fields["execution_segments"].([]any); ok {
		if len(segments) > timelineMaxExecutionSegments {
			fields["execution_segments"] = compactTimelineItems(segments, timelineMaxExecutionSegments, "execution segments")
			segments, _ = fields["execution_segments"].([]any)
		}
		for _, item := range segments {
			segment, ok := item.(map[string]any)
			if !ok {
				continue
			}
			compactExecutionSegmentProjection(segment)
			if tool, ok := segment["tool"].(map[string]any); ok {
				compactToolProjection(tool)
			}
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return metadata
	}
	return encoded
}

func compactExecutionProjection(execution map[string]any) {
	if input, ok := execution["input"].(string); ok {
		execution["input"] = compactTimelineString(input, timelineExecutionFieldMaxRunes)
	}
	if output, ok := execution["output"].(string); ok {
		execution["output"] = compactTimelineString(output, timelineExecutionFieldMaxRunes)
	}
	calls, ok := execution["tool_calls"].([]any)
	if !ok {
		return
	}
	if len(calls) > timelineMaxToolCalls {
		execution["tool_calls"] = compactTimelineItems(calls, timelineMaxToolCalls, "tool calls")
		calls, _ = execution["tool_calls"].([]any)
	}
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		compactToolProjection(call)
	}
}

func compactTimelineItems(items []any, limit int, label string) []any {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	head := limit / 2
	tail := limit - head - 1
	omitted := len(items) - head - tail
	next := make([]any, 0, limit)
	next = append(next, items[:head]...)
	next = append(next, map[string]any{
		"id":         "timeline-projection-omitted-" + label,
		"type":       "text",
		"agent":      "system",
		"name":       "timeline_projection_omitted",
		"status":     "success",
		"content":    strconv.Itoa(omitted) + " " + label + " omitted from timeline projection; open execution audit for full payload",
		"result":     strconv.Itoa(omitted) + " " + label + " omitted from timeline projection; open execution audit for full payload",
		"created_at": "",
	})
	next = append(next, items[len(items)-tail:]...)
	return next
}

func compactToolProjection(tool map[string]any) {
	if args, ok := tool["args"].(string); ok {
		tool["args"] = compactTimelineString(args, timelineToolFieldMaxRunes)
	}
	if result, ok := tool["result"].(string); ok {
		tool["result"] = compactTimelineString(result, timelineToolFieldMaxRunes)
	}
}

func compactExecutionSegmentProjection(segment map[string]any) {
	if content, ok := segment["content"].(string); ok {
		segment["content"] = compactTimelineString(content, timelineTextSegmentMaxRunes)
	}
	if label, ok := segment["label"].(string); ok {
		segment["label"] = compactTimelineString(label, timelineTextSegmentMaxRunes)
	}
}

func compactTimelineString(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	omitted := len(runes) - maxRunes
	return string(runes[:maxRunes]) + "\n… [" + strconv.Itoa(omitted) + " chars omitted from timeline projection; open execution audit for full payload]"
}

func (s *Server) handleUpdateAssistantExecutionSegments(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	workspaceID := getTenant(c)
	conversationID := c.Param("id")
	messageID := c.Param("message_id")
	conv, err := s.Conversations.GetConversation(c.Request().Context(), workspaceID, conversationID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
	}
	if conv.UserID != getUserID(c) {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "conversation_read_only"})
	}
	var body struct {
		Segments json.RawMessage `json:"segments"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
	}
	if len(body.Segments) == 0 || !json.Valid(body.Segments) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_execution_segments"})
	}
	var segments []json.RawMessage
	if err := json.Unmarshal(body.Segments, &segments); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_execution_segments"})
	}
	if len(segments) > 300 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "execution_segments_too_large"})
	}
	if len(body.Segments) > 512*1024 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "execution_segments_payload_too_large"})
	}
	message, err := s.Conversations.MergeAssistantMessageMetadataFields(
		c.Request().Context(), workspaceID, conversationID, messageID,
		map[string]any{"execution_segments": json.RawMessage(body.Segments)},
	)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "assistant_message_not_found"})
	}
	return c.JSON(http.StatusOK, message)
}

func (s *Server) handleMarkConversationRead(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	var body struct {
		LastMessageID string `json:"last_message_id"`
	}
	if err := c.Bind(&body); err != nil || body.LastMessageID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "last_message_id is required"})
	}
	if err := s.Conversations.MarkRead(
		c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c), body.LastMessageID,
	); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "conversation not found"})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "read"})
}

func (s *Server) handleListInboxUnread(c echo.Context) error {
	if s.Conversations == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "conversations are not configured"})
	}
	unread, err := s.Conversations.ListUnread(c.Request().Context(), getTenant(c), getUserID(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, unread)
}

func markReadOnlyConversations(conversations []conversationstore.Conversation, requesterID string) {
	for i := range conversations {
		conversations[i].ReadOnly = conversations[i].UserID != requesterID
	}
}
