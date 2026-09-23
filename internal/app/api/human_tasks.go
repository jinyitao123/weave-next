package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

type humanTaskCursorV1 struct {
	UpdatedAt string `json:"updated_at"`
	RunID     string `json:"run_id"`
}

type humanTaskResponse struct {
	InteractionID   string          `json:"interaction_id"`
	NodeID          string          `json:"node_id"`
	RunID           string          `json:"run_id"`
	ProjectID       string          `json:"project_id,omitempty"`
	TeamID          string          `json:"team_id"`
	WorkflowID      string          `json:"workflow_id"`
	WorkflowVersion int             `json:"workflow_version"`
	Title           string          `json:"title"`
	Instructions    string          `json:"instructions"`
	AudienceRef     string          `json:"audience_ref,omitempty"`
	ResumeSchema    json.RawMessage `json:"resume_schema"`
	DeadlineAt      *time.Time      `json:"deadline_at,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type humanTaskDetailResponse struct {
	humanTaskResponse
	PredecessorOutputs map[string]json.RawMessage `json:"predecessor_outputs"`
}

type completeHumanTaskRequest struct {
	InteractionID  string          `json:"interaction_id"`
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
	total, err := s.teamRunHumanTasks.Count(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	responses := make([]humanTaskResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, humanTaskResponse{
			InteractionID: teamrun.HumanInteractionID(item.Run), NodeID: item.Detail.NodeID,
			RunID: item.Run.RunID, ProjectID: item.Run.ProjectID, TeamID: item.Run.TeamID,
			WorkflowID: item.Run.WorkflowID, WorkflowVersion: item.Run.WorkflowVersion,
			Title: item.Detail.Task.Title, Instructions: item.Detail.Task.Instructions,
			AudienceRef: item.Detail.Task.AudienceRef, ResumeSchema: item.Detail.ResumeSchema,
			DeadlineAt: item.Detail.DeadlineAt, UpdatedAt: item.Run.UpdatedAt,
		})
	}
	nextCursor := ""
	if hasMore && len(items) != 0 {
		nextCursor, err = encodeHumanTaskCursor(items[len(items)-1].Run.UpdatedAt, items[len(items)-1].Run.RunID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "encode cursor"})
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"tasks": responses, "next_cursor": nextCursor, "total": total})
}

const (
	humanTaskGetMaxBytes  = 64 * 1024
	humanTaskPageMaxItems = 10_000
)

var (
	errInvalidJSONPointer = errors.New("invalid JSON pointer")
	errJSONPointerMissing = errors.New("JSON pointer value not found")
)

func (s *Server) handleGetHumanTask(c echo.Context) error {
	if err := s.requireCurrentWorkspaceMember(c); err != nil {
		return err
	}
	if s.teamRunHumanTasks == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "human task detail unavailable"})
	}
	item, err := s.teamRunHumanTasks.Get(c.Request().Context(), getTenant(c), c.Param("run_id"))
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "human task not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	detail := humanTaskDetailResponse{
		humanTaskResponse: humanTaskResponse{
			InteractionID: teamrun.HumanInteractionID(item.Run), NodeID: item.Detail.NodeID,
			RunID: item.Run.RunID, ProjectID: item.Run.ProjectID, TeamID: item.Run.TeamID,
			WorkflowID: item.Run.WorkflowID, WorkflowVersion: item.Run.WorkflowVersion,
			Title: item.Detail.Task.Title, Instructions: item.Detail.Task.Instructions,
			AudienceRef: item.Detail.Task.AudienceRef, ResumeSchema: item.Detail.ResumeSchema,
			DeadlineAt: item.Detail.DeadlineAt, UpdatedAt: item.Run.UpdatedAt,
		},
		PredecessorOutputs: item.CompletedOutputs,
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "encode human task detail"})
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "decode human task detail"})
	}
	return writeSelectedJSONResponse(c, value, "human_task")
}

func writeSelectedJSONResponse(c echo.Context, document any, resource string) error {
	path := c.QueryParam("path")
	value, err := resolveJSONPointer(document, path)
	if errors.Is(err, errInvalidJSONPointer) {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_json_pointer", "error": "path must be an RFC 6901 JSON pointer"})
	}
	if errors.Is(err, errJSONPointerMissing) {
		return c.JSON(http.StatusNotFound, map[string]string{"code": resource + "_value_not_found", "error": "selected value not found"})
	}
	offset, limit, paged, err := parseHumanTaskPage(c.QueryParam("offset"), c.QueryParam("limit"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_pagination", "error": err.Error()})
	}
	total := jsonPageLength(value)
	if paged {
		value, err = paginateJSONValue(value, offset, limit)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"code": "value_not_pageable", "error": "selected value does not support offset/limit pagination"})
		}
		c.Response().Header().Set("X-Weave-Page-Offset", strconv.Itoa(offset))
		c.Response().Header().Set("X-Weave-Page-Limit", strconv.Itoa(limit))
		c.Response().Header().Set("X-Weave-Page-Total", strconv.Itoa(total))
	}
	selected, err := json.Marshal(value)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "encode selected JSON value"})
	}
	if len(selected) > humanTaskGetMaxBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]any{
			"code": resource + "_value_too_large", "error": "selected JSON value exceeds the response limit",
			"max_bytes":             humanTaskGetMaxBytes,
			"pagination_parameters": map[string]int{"offset": 0, "limit": 1000},
		})
	}
	return c.JSONBlob(http.StatusOK, selected)
}

func resolveJSONPointer(document any, pointer string) (any, error) {
	if pointer == "" {
		return document, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errInvalidJSONPointer
	}
	current := document
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token, err := decodeJSONPointerToken(rawToken)
		if err != nil {
			return nil, err
		}
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[token]
			if !ok {
				return nil, errJSONPointerMissing
			}
		case []any:
			if token == "-" || (len(token) > 1 && token[0] == '0') {
				return nil, errJSONPointerMissing
			}
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, errJSONPointerMissing
			}
			current = typed[index]
		default:
			return nil, errJSONPointerMissing
		}
	}
	return current, nil
}

func decodeJSONPointerToken(token string) (string, error) {
	var decoded strings.Builder
	for index := 0; index < len(token); index++ {
		if token[index] != '~' {
			decoded.WriteByte(token[index])
			continue
		}
		if index+1 >= len(token) {
			return "", errInvalidJSONPointer
		}
		index++
		switch token[index] {
		case '0':
			decoded.WriteByte('~')
		case '1':
			decoded.WriteByte('/')
		default:
			return "", errInvalidJSONPointer
		}
	}
	return decoded.String(), nil
}

func parseHumanTaskPage(rawOffset, rawLimit string) (int, int, bool, error) {
	rawOffset, rawLimit = strings.TrimSpace(rawOffset), strings.TrimSpace(rawLimit)
	if rawOffset == "" && rawLimit == "" {
		return 0, 0, false, nil
	}
	if rawLimit == "" {
		return 0, 0, false, errors.New("limit is required when offset is set")
	}
	offset := 0
	var err error
	if rawOffset != "" {
		offset, err = strconv.Atoi(rawOffset)
		if err != nil || offset < 0 {
			return 0, 0, false, errors.New("offset must be a non-negative integer")
		}
	}
	limit, err := strconv.Atoi(rawLimit)
	if err != nil || limit < 1 || limit > humanTaskPageMaxItems {
		return 0, 0, false, errors.New("limit must be between 1 and 10000")
	}
	return offset, limit, true, nil
}

func jsonPageLength(value any) int {
	switch typed := value.(type) {
	case string:
		return len([]rune(typed))
	case []any:
		return len(typed)
	case map[string]any:
		return len(typed)
	default:
		return 0
	}
}

func paginateJSONValue(value any, offset, limit int) (any, error) {
	end := offset + limit
	switch typed := value.(type) {
	case string:
		runes := []rune(typed)
		if offset > len(runes) {
			offset = len(runes)
		}
		if end > len(runes) {
			end = len(runes)
		}
		return string(runes[offset:end]), nil
	case []any:
		if offset > len(typed) {
			offset = len(typed)
		}
		if end > len(typed) {
			end = len(typed)
		}
		return typed[offset:end], nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if offset > len(keys) {
			offset = len(keys)
		}
		if end > len(keys) {
			end = len(keys)
		}
		page := make(map[string]any, end-offset)
		for _, key := range keys[offset:end] {
			page[key] = typed[key]
		}
		return page, nil
	default:
		return nil, errors.New("value is not pageable")
	}
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
	request.InteractionID = strings.TrimSpace(request.InteractionID)
	if request.InteractionID == "" || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || len(request.InteractionID) > 256 ||
		len(request.Payload) == 0 || len(request.Payload) > teamrun.HumanResumePayloadMaxBytes {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "interaction_id, payload, or idempotency_key is invalid"})
	}
	workspaceID := getTenant(c)
	runID := c.Param("run_id")
	// Canonical bytes bind idempotent replay independently of the current
	// question. Schema validation happens only after the service locks that wait.
	canonical, err := frozen.CanonicalizeJSON(request.Payload)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "payload cannot be canonicalized"})
	}
	digest := sha256.Sum256(canonical)
	result, err := s.teamRunHumanResume.Complete(c.Request().Context(), teamrun.CompleteHumanWaitRequest{
		WorkspaceID: workspaceID, RunID: runID, Payload: canonical, PayloadDigest: digest[:],
		InteractionID: request.InteractionID,
		ValidatePayload: func(schema, payload json.RawMessage) error {
			_, problems, validationErr := validateAndCanonicalizeHumanPayload(schema, payload)
			if len(problems) > 0 {
				return &humanPayloadValidationError{problems: problems}
			}
			return validationErr
		},
		IdempotencyKey: request.IdempotencyKey, Actor: getUserID(c), OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		var invalid *humanPayloadValidationError
		if errors.As(err, &invalid) {
			return c.JSON(http.StatusUnprocessableEntity, map[string]any{"error": "payload violates resume_schema", "problems": invalid.problems})
		}
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

type humanPayloadValidationError struct {
	problems []machine.RuntimeSchemaProblem
}

func (*humanPayloadValidationError) Error() string { return "payload violates resume_schema" }

func (s *Server) requireCurrentWorkspaceMember(c echo.Context) error {
	if s.OrgStore == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, map[string]string{"error": "workspace membership unavailable"})
	}
	members, err := s.OrgStore.ListMembers(c.Request().Context(), getTenant(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, map[string]string{"error": "check workspace membership"})
	}
	userID := getUserID(c)
	for _, member := range members {
		if member.UserID == userID && !member.Deleted {
			return nil
		}
	}
	return echo.NewHTTPError(http.StatusForbidden, map[string]string{"error": "current workspace membership required"})
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
