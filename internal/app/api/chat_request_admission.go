package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jinyitao123/weave/internal/app/chatrequest"
	"github.com/labstack/echo/v4"
)

func chatRequestFingerprint(req ChatRequest) (string, error) {
	payload := struct {
		Agent                    string         `json:"agent"`
		ProjectID                string         `json:"project_id"`
		ConversationID           string         `json:"conversation_id,omitempty"`
		Intent                   string         `json:"intent,omitempty"`
		RuntimeID                string         `json:"runtime_id,omitempty"`
		SessionID                string         `json:"session_id"`
		Message                  string         `json:"message"`
		Channel                  string         `json:"channel"`
		Profile                  string         `json:"profile"`
		Effort                   string         `json:"effort"`
		Stream                   bool           `json:"stream"`
		Async                    bool           `json:"async"`
		Context                  map[string]any `json:"context"`
		AttachmentIDs            []string       `json:"attachment_ids"`
		BlueprintChangeRequested bool           `json:"blueprint_change_requested,omitempty"`
	}{
		Agent: req.Agent, ProjectID: req.ProjectID, ConversationID: req.ConversationID, Intent: req.Intent,
		RuntimeID: req.RuntimeID, SessionID: req.SessionID, Message: req.Message,
		Channel: req.Channel, Profile: req.Profile, Effort: req.Effort,
		Stream: req.Stream, Async: req.Async, Context: req.Context,
		AttachmentIDs:            append([]string(nil), req.AttachmentIDs...),
		BlueprintChangeRequested: req.BlueprintChangeRequested,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode chat request fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

func (s *Server) beginChatRequest(
	ctx context.Context, workspaceID, userID, agentID string, req ChatRequest,
) (chatrequest.Request, bool, error) {
	if strings.TrimSpace(req.ClientRequestID) == "" {
		return chatrequest.Request{}, true, nil
	}
	if s.ChatRequests == nil || s.Conversations == nil {
		return chatrequest.Request{}, false, errors.New("chat request idempotency is unavailable")
	}
	fingerprint, err := chatRequestFingerprint(req)
	if err != nil {
		return chatrequest.Request{}, false, err
	}
	return s.ChatRequests.Begin(ctx, chatrequest.BeginRequest{
		WorkspaceID: workspaceID, UserID: userID, ClientRequestID: req.ClientRequestID,
		RequestFingerprint: fingerprint, ProjectID: req.ProjectID, AgentID: agentID,
	})
}

func (s *Server) admitOrReplayChatRequest(
	c echo.Context,
	ctx context.Context,
	workspaceID string,
	userID string,
	agentID string,
	req ChatRequest,
) (bool, error) {
	requestRecord, requestCreated, err := s.beginChatRequest(ctx, workspaceID, userID, agentID, req)
	if err != nil {
		return false, respondChatRequestError(c, err)
	}
	if !requestCreated {
		return false, s.respondChatRequestReplay(c, req, requestRecord)
	}
	return true, nil
}

func respondChatRequestError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, chatrequest.ErrConflict):
		return c.JSON(http.StatusConflict, map[string]string{"error": "client_request_conflict"})
	case strings.Contains(err.Error(), "parse client request id"):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_request_id"})
	case strings.Contains(err.Error(), "unavailable"):
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "chat_request_idempotency_unavailable"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_admission_failed"})
	}
}

func (s *Server) respondChatRequestReplay(
	c echo.Context, req ChatRequest, record chatrequest.Request,
) error {
	var err error
	record, err = s.recoverPublishedChatAdmission(c.Request().Context(), record)
	if err != nil {
		return c.JSON(http.StatusAccepted, map[string]string{"code": "workflow_admission_pending", "status": "running"})
	}
	payload := map[string]any{
		"code": "client_request_replay", "status": record.Status,
		"project_id": record.ProjectID, "session_id": record.SessionID,
		"conversation_id": record.ConversationID, "user_message_id": record.UserMessageID,
		"task_id": record.TaskID, "run_id": record.RunID,
	}
	if len(record.Response) > 0 {
		_ = json.Unmarshal(record.Response, &payload)
		payload["replayed"] = true
	}
	switch record.Status {
	case "queued":
		return c.JSON(http.StatusAccepted, payload)
	case "yielded", "completed", "failed":
		if req.Stream {
			sse, err := NewSSEWriter(c)
			if err != nil {
				return err
			}
			return sse.SendEvent("done", payload)
		}
		return c.JSON(http.StatusOK, payload)
	default:
		payload["code"] = "client_request_in_progress"
		return c.JSON(http.StatusConflict, payload)
	}
}

func (s *Server) finishChatRequest(
	ctx context.Context,
	workspaceID, userID, clientRequestID, status, runID string,
	response any,
) {
	ctx = context.WithoutCancel(ctx)
	if strings.TrimSpace(clientRequestID) == "" || s.ChatRequests == nil {
		return
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		slog.Error("encode chat request outcome", "error", err)
		return
	}
	if _, err := s.ChatRequests.MarkFinished(
		ctx, workspaceID, userID, clientRequestID, status, runID, encoded,
	); err != nil {
		slog.Error("persist chat request outcome", "client_request_id", clientRequestID, "error", err)
	}
}
