package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

type humanTaskCursorV1 struct {
	UpdatedAt string `json:"updated_at"`
	RunID     string `json:"run_id"`
}

type humanTaskResponse struct {
	RunID            string                     `json:"run_id"`
	ProjectID        string                     `json:"project_id,omitempty"`
	TeamID           string                     `json:"team_id"`
	WorkflowID       string                     `json:"workflow_id"`
	WorkflowVersion  int                        `json:"workflow_version"`
	Title            string                     `json:"title"`
	Instructions     string                     `json:"instructions"`
	AudienceRef      string                     `json:"audience_ref,omitempty"`
	ResumeSchema     json.RawMessage            `json:"resume_schema"`
	DeadlineAt       *time.Time                 `json:"deadline_at,omitempty"`
	CompletedOutputs map[string]json.RawMessage `json:"completed_outputs"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

type completeHumanTaskRequest struct {
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (s *Server) handleListHumanTasks(c echo.Context) error {
	if err := s.requireCurrentWorkspaceMember(c); err != nil {
		return err
	}
	if s.teamRunHumanTasks == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "human task inbox unavailable"})
	}
	limit := 20
	if raw := strings.TrimSpace(c.QueryParam("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 100"})
		}
		limit = parsed
	}
	before, beforeRunID, err := decodeHumanTaskCursor(c.QueryParam("cursor"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
	}
	items, hasMore, err := s.teamRunHumanTasks.List(c.Request().Context(), getTenant(c), before, beforeRunID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	responses := make([]humanTaskResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, humanTaskResponse{
			RunID: item.Run.RunID, ProjectID: item.Run.ProjectID, TeamID: item.Run.TeamID,
			WorkflowID: item.Run.WorkflowID, WorkflowVersion: item.Run.WorkflowVersion,
			Title: item.Detail.Task.Title, Instructions: item.Detail.Task.Instructions,
			AudienceRef: item.Detail.Task.AudienceRef, ResumeSchema: item.Detail.ResumeSchema,
			DeadlineAt: item.Detail.DeadlineAt, CompletedOutputs: item.CompletedOutputs,
			UpdatedAt: item.Run.UpdatedAt,
		})
	}
	nextCursor := ""
	if hasMore && len(items) != 0 {
		nextCursor, err = encodeHumanTaskCursor(items[len(items)-1].Run.UpdatedAt, items[len(items)-1].Run.RunID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "encode cursor"})
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"tasks": responses, "next_cursor": nextCursor})
}

func (s *Server) handleCompleteHumanTask(c echo.Context) error {
	if err := s.requireCurrentWorkspaceMember(c); err != nil {
		return err
	}
	if s.teamRunHumanTasks == nil || s.teamRunHumanResume == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "human task completion unavailable"})
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, teamrun.HumanResumePayloadMaxBytes+4096)
	var request completeHumanTaskRequest
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "request must contain one JSON object"})
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 ||
		len(request.Payload) == 0 || len(request.Payload) > teamrun.HumanResumePayloadMaxBytes {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "payload or idempotency_key is invalid"})
	}
	workspaceID := getTenant(c)
	runID := c.Param("run_id")
	task, err := s.teamRunHumanTasks.Get(c.Request().Context(), workspaceID, runID)
	canonical := json.RawMessage(nil)
	if err == nil {
		var problems []machine.RuntimeSchemaProblem
		canonical, problems, err = validateAndCanonicalizeHumanPayload(task.Detail.ResumeSchema, request.Payload)
		if len(problems) != 0 {
			return c.JSON(http.StatusUnprocessableEntity, map[string]any{
				"error": "payload violates resume_schema", "problems": problems,
			})
		}
		if err != nil {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "payload cannot be canonicalized"})
		}
	} else if !errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	} else {
		canonical, err = frozen.CanonicalizeJSON(request.Payload)
		if err != nil {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "payload cannot be canonicalized"})
		}
	}
	digest := sha256.Sum256(canonical)
	result, err := s.teamRunHumanResume.Complete(c.Request().Context(), teamrun.CompleteHumanWaitRequest{
		WorkspaceID: workspaceID, RunID: runID, Payload: canonical, PayloadDigest: digest[:],
		IdempotencyKey: request.IdempotencyKey, Actor: getUserID(c), OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, teamrun.ErrTeamRunStateConflict), errors.Is(err, teamrun.ErrTeamRunResumeStale), errors.Is(err, teamrun.ErrTeamRunResumeInvalid):
			return c.JSON(http.StatusConflict, map[string]string{"error": "human task already resolved or payload conflicts"})
		case errors.Is(err, teamrun.ErrTeamRunIdentityMismatch):
			return c.JSON(http.StatusNotFound, map[string]string{"error": "human task not found"})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"run_id": result.Run.RunID, "status": "queued", "task_id": result.TaskID,
		"idempotent": result.Idempotent,
	})
}

func (s *Server) requireCurrentWorkspaceMember(c echo.Context) error {
	if s.OrgStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workspace membership unavailable"})
	}
	members, err := s.OrgStore.ListMembers(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "check workspace membership"})
	}
	userID := getUserID(c)
	for _, member := range members {
		if member.UserID == userID && !member.Deleted {
			return nil
		}
	}
	return c.JSON(http.StatusForbidden, map[string]string{"error": "current workspace membership required"})
}

func encodeHumanTaskCursor(updatedAt time.Time, runID string) (string, error) {
	raw, err := json.Marshal(humanTaskCursorV1{UpdatedAt: updatedAt.UTC().Format(time.RFC3339Nano), RunID: runID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeHumanTaskCursor(raw string) (*time.Time, string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, "", err
	}
	var cursor humanTaskCursorV1
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.RunID == "" {
		return nil, "", errors.New("invalid cursor")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, "", errors.New("invalid cursor")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, cursor.UpdatedAt)
	if err != nil {
		return nil, "", err
	}
	updatedAt = updatedAt.UTC()
	return &updatedAt, cursor.RunID, nil
}

func validateAndCanonicalizeHumanPayload(
	schema json.RawMessage,
	payload json.RawMessage,
) (json.RawMessage, []machine.RuntimeSchemaProblem, error) {
	validated, problems := machine.ValidateRuntimeInput(schema, payload)
	if len(problems) != 0 {
		return nil, problems, nil
	}
	canonical, err := frozen.CanonicalizeJSON(validated)
	return canonical, nil, err
}
