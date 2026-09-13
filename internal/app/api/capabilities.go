package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/labstack/echo/v4"
)

type saveCapabilityDraftRequest struct {
	Definition capability.Definition `json:"definition"`
}

type invokeCapabilityRequest struct {
	RequestID string          `json:"request_id"`
	Input     json.RawMessage `json:"input"`
}

type debugCapabilityRequest struct {
	RequestID  string                `json:"request_id"`
	Definition capability.Definition `json:"definition"`
	Input      json.RawMessage       `json:"input"`
}

func (s *Server) handleDebugCapability(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(503, map[string]string{"error": "capability service unavailable"})
	}
	var request debugCapabilityRequest
	if err := decodeCapabilityBody(c, &request); err != nil {
		return c.JSON(400, map[string]string{"code": "capability_request_invalid"})
	}
	if request.Definition.CapabilityID != c.Param("capabilityID") {
		return c.JSON(400, map[string]string{"code": "capability_request_invalid"})
	}
	workspace, _ := c.Get("tenant").(string)
	if err := s.validateCapabilityRuntime(c.Request().Context(), workspace, request.Definition.Runtime); err != nil {
		return c.JSON(422, map[string]string{"code": "capability_runtime_unavailable"})
	}
	invocation, replayed, err := s.Capabilities.Debug(c.Request().Context(), appcapabilities.DebugRequest{
		WorkspaceID: workspace, ApplicationID: capabilityApplicationID(c), RequestID: request.RequestID, Definition: request.Definition, Input: request.Input,
	})
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(202, map[string]any{"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID, "capability_id": invocation.CapabilityID,
		"run_kind": invocation.RunKind, "definition_hash": invocation.DefinitionHash, "status": invocation.Status, "result_state": invocation.ResultState, "replayed": replayed})
}

func (s *Server) handleListCapabilityDrafts(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(503, map[string]string{"error": "capability service unavailable"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	drafts, err := s.Capabilities.ListDrafts(c.Request().Context(), workspaceID)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(200, map[string]any{"drafts": drafts})
}

func (s *Server) handleSaveCapabilityDraft(c echo.Context) error {
	if s.Capabilities == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "capability service unavailable"})
	}
	var request saveCapabilityDraftRequest
	if err := decodeCapabilityBody(c, &request); err != nil {
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
	draft, err := s.Capabilities.GetDraft(c.Request().Context(), workspaceID, c.Param("capabilityID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	if err := s.validateCapabilityRuntime(c.Request().Context(), workspaceID, draft.Runtime); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_runtime_unavailable", "error": "Select a configured model on the local Loom runtime; pools and additional runtime requirements are not connected."})
	}
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
	if err := decodeCapabilityBody(c, &request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	applicationID, _ := c.Get(apiKeyIDContextKey).(string)
	if applicationID == "" {
		applicationID, _ = c.Get("user_id").(string)
	}
	invocation, replayed, err := s.Capabilities.Invoke(c.Request().Context(), appcapabilities.InvokeRequest{
		WorkspaceID: workspaceID, ApplicationID: applicationID,
		RequestID: request.RequestID, CapabilityID: c.Param("capabilityID"), Revision: revision, Input: request.Input,
	})
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"run_kind": invocation.RunKind, "definition_hash": invocation.DefinitionHash,
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
	response := map[string]any{
		"run_kind": invocation.RunKind, "definition_hash": invocation.DefinitionHash,
		"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID,
		"capability_id": invocation.CapabilityID,
		"status":        invocation.Status, "result_state": invocation.ResultState,
		"result": invocation.Result, "error": publicCapabilityError(invocation),
	}
	if invocation.RunKind == "published" {
		response["revision"] = invocation.Revision
	}
	return c.JSON(http.StatusOK, response)
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
	status := http.StatusInternalServerError
	code := "capability_service_error"
	switch {
	case errors.Is(err, capability.ErrInvalidDefinition), errors.Is(err, capability.ErrInvalidRevision):
		status, code = http.StatusBadRequest, "capability_request_invalid"
	case errors.Is(err, appcapabilities.ErrRevisionConflict):
		status, code = http.StatusConflict, "capability_revision_conflict"
	case errors.Is(err, appcapabilities.ErrNotFound), errors.Is(err, appcapabilities.ErrRevisionNotFound):
		status, code = http.StatusNotFound, "capability_not_found"
	case errors.Is(err, appcapabilities.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, appcapabilities.ErrInvocationNotFound):
		status, code = http.StatusNotFound, "invocation_not_found"
	case errors.Is(err, appcapabilities.ErrInvocationTerminal):
		status, code = http.StatusConflict, "invocation_terminal"
	}
	message := code
	if status == http.StatusBadRequest {
		message = err.Error()
	}
	return c.JSON(status, map[string]string{"code": code, "error": message})
}

func publicCapabilityError(i appcapabilities.Invocation) string {
	if i.Status == "failed" {
		return "capability_execution_failed"
	}
	return ""
}

func decodeCapabilityBody(c echo.Context, target any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 1<<20))
	if err != nil {
		return err
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// Capability credentials use explicit scopes; legacy empty scope lists confer
// no capability access.
func requireCapabilityAccess(action string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Get(authSourceContextKey) == authSourceAPIKey {
				scopes, _ := c.Get(scopesContextKey).([]string)
				for _, scope := range scopes {
					if scope == "capabilities:"+action {
						return next(c)
					}
				}
				return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient capability scope"})
			}
			return RequireAnyRole("admin", "owner", "developer")(next)(c)
		}
	}
}
