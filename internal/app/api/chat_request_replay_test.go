package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/app/chatrequest"
	"github.com/labstack/echo/v4"
)

func TestRespondChatRequestReplayCompletedReturnsTerminalPayload(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/v1/chat", nil), recorder)
	record := chatrequest.Request{
		ProjectID: "project-1", SessionID: "session-1", ConversationID: "conversation-1",
		UserMessageID: "message-user-1", RunID: "run-1", Status: "completed",
		Response: json.RawMessage(`{
			"output":"done",
			"stop_reason":"stop",
			"session_id":"session-1",
			"run_id":"run-1",
			"project_id":"project-1",
			"conversation_id":"conversation-1",
			"user_message_id":"message-user-1"
		}`),
	}

	if err := (&Server{}).respondChatRequestReplay(ctx, ChatRequest{Stream: false}, record); err != nil {
		t.Fatalf("respond replay: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["replayed"] != true {
		t.Fatalf("replayed = %#v, want true", payload["replayed"])
	}
	for _, field := range []string{
		"output", "stop_reason", "session_id", "run_id", "project_id", "conversation_id", "user_message_id",
	} {
		if payload[field] == "" || payload[field] == nil {
			t.Fatalf("payload missing %q: %s", field, recorder.Body.String())
		}
	}
}

func TestRespondChatRequestReplayRunningReturnsInProgressConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/v1/chat", nil), recorder)
	record := chatrequest.Request{
		ProjectID: "project-1", SessionID: "session-1", ConversationID: "conversation-1",
		UserMessageID: "message-user-1", RunID: "run-1", Status: "running",
	}

	if err := (&Server{}).respondChatRequestReplay(ctx, ChatRequest{Stream: false}, record); err != nil {
		t.Fatalf("respond replay: %v", err)
	}
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["code"] != "client_request_in_progress" || payload["status"] != "running" {
		t.Fatalf("unexpected in-progress payload: %#v", payload)
	}
	for _, field := range []string{"project_id", "session_id", "conversation_id", "user_message_id", "run_id"} {
		if payload[field] == "" || payload[field] == nil {
			t.Fatalf("payload missing %q: %s", field, recorder.Body.String())
		}
	}
}
