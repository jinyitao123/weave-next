package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/labstack/echo/v4"
)

type saveCapabilityDraftRequest struct {
	Definition capability.Definition `json:"definition"`
}

type invokeCapabilityRequest struct {
	RequestID    string          `json:"request_id"`
	InvocationID string          `json:"invocation_id,omitempty"`
	Input        json.RawMessage `json:"input"`
}

func (s *Server) handleSaveCapabilityDraft(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	var request saveCapabilityDraftRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	if err := s.Capabilities.SaveDraft(c.Request().Context(), appcapabilities.DraftRequest{WorkspaceID: workspaceID, Definition: request.Definition}); err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]any{"capability_id": request.Definition.CapabilityID, "status": "draft_saved"})
}

func (s *Server) handlePublishCapability(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "revision must be a positive integer"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	published, err := s.Capabilities.Publish(c.Request().Context(), workspaceID, c.Param("capabilityID"), revision)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusCreated, published)
}

func (s *Server) handleInvokeCapability(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "revision must be a positive integer"})
	}
	var request invokeCapabilityRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	applicationID, _ := c.Get(apiKeyIDContextKey).(string)
	if applicationID == "" {
		applicationID, _ = c.Get("user_id").(string)
	}
	invocation, replayed, err := s.Capabilities.Invoke(c.Request().Context(), appcapabilities.InvokeRequest{
		WorkspaceID: workspaceID, ApplicationID: applicationID, InvocationID: request.InvocationID,
		RequestID: request.RequestID, CapabilityID: c.Param("capabilityID"), Revision: revision, Input: request.Input,
	})
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"invocation_id": invocation.InvocationID, "capability_id": invocation.CapabilityID,
		"revision": invocation.Revision, "task_id": invocation.TaskID, "status": invocation.Status,
		"result_state": invocation.ResultState, "replayed": replayed,
	})
}

func capabilityApplicationID(c echo.Context) string {
	applicationID, _ := c.Get(apiKeyIDContextKey).(string)
	if applicationID == "" {
		applicationID, _ = c.Get("user_id").(string)
	}
	return applicationID
}

func (s *Server) handleGetCapabilityInvocation(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	invocation, err := s.Capabilities.GetInvocation(c.Request().Context(), workspaceID, capabilityApplicationID(c), c.Param("invocationID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID,
		"capability_id": invocation.CapabilityID, "revision": invocation.Revision,
		"status": invocation.Status, "result_state": invocation.ResultState,
	})
}

func (s *Server) handleCancelCapabilityInvocation(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	invocation, err := s.Capabilities.CancelInvocation(c.Request().Context(), workspaceID, capabilityApplicationID(c), c.Param("invocationID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID, "status": invocation.Status})
}

func capabilityHTTPError(c echo.Context, err error) error {
	status := http.StatusBadRequest
	code := "capability_request_invalid"
	switch {
	case errors.Is(err, appcapabilities.ErrNotFound), errors.Is(err, appcapabilities.ErrRevisionNotFound):
		status, code = http.StatusNotFound, "capability_not_found"
	case errors.Is(err, appcapabilities.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, appcapabilities.ErrInvocationNotFound):
		status, code = http.StatusNotFound, "invocation_not_found"
	case errors.Is(err, appcapabilities.ErrInvocationTerminal):
		status, code = http.StatusConflict, "invocation_terminal"
	}
	return c.JSON(status, map[string]string{"code": code, "error": err.Error()})
}
