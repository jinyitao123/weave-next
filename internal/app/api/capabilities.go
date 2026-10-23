package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/google/uuid"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/labstack/echo/v4"
)

type saveCapabilityDraftRequest struct {
	Definition capability.Definition `json:"definition"`
}

type invokeCapabilityRequest struct {
	RequestID    string                         `json:"request_id"`
	InvocationID optionalCapabilityInvocationID `json:"invocation_id,omitempty"`
	Lane         string                         `json:"lane,omitempty"`
	Priority     string                         `json:"priority,omitempty"`
	FairnessKey  string                         `json:"fairness_key,omitempty"`
	Input        json.RawMessage                `json:"input"`
}

type optionalCapabilityInvocationID struct {
	value    string
	supplied bool
}

func (id *optionalCapabilityInvocationID) UnmarshalJSON(raw []byte) error {
	var value string
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return errors.New("invocation_id must be a canonical UUID")
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return errors.New("invocation_id must be a canonical UUID")
	}
	id.value, id.supplied = value, true
	return nil
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
	models := []string{}
	if s.Models != nil {
		resolved, err := s.Models.ForWorkspace(c.Request().Context(), workspaceID)
		if err != nil {
			return capabilityHTTPError(c, err)
		}
		if router, ok := resolved.(*llmrouter.Router); ok {
			seen := map[string]bool{}
			for _, provider := range router.ListProviders() {
				for _, model := range provider.Models {
					if !seen[model] {
						seen[model] = true
						models = append(models, model)
					}
				}
			}
			sort.Strings(models)
		}
	}
	versions := []appcapabilities.VersionSummary{}
	if s.CapabilityAccess != nil {
		snapshot, err := s.CapabilityAccess.Snapshot(c.Request().Context(), workspaceID, getUserID(c))
		if err != nil {
			return capabilityHTTPError(c, err)
		}
		versions = snapshot.Versions
	}
	return c.JSON(200, map[string]any{"drafts": drafts, "models": models, "versions": versions})
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
	if draft.Workflow == nil {
		return capabilityHTTPError(c, appcapabilities.ErrWorkflowSourceRequired)
	}
	service := s.workflowCapabilityInvocations()
	if service == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workflow invocation service unavailable"})
	}
	if err := service.ValidateDefinition(c.Request().Context(), workspaceID, draft); err != nil {
		return capabilityHTTPError(c, err)
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
	if p, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal); ok {
		if err := s.CapabilityAccess.AuthorizeInvocation(c.Request().Context(), p, c.Param("capabilityID"), revision); err != nil {
			return capabilityHTTPError(c, err)
		}
	}
	published, err := appcapabilities.NewPGStore(s.Pool).GetRevision(c.Request().Context(), workspaceID, c.Param("capabilityID"), revision)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	if published.Definition.Workflow == nil {
		return capabilityHTTPError(c, appcapabilities.ErrWorkflowSourceRequired)
	}
	return s.invokeWorkflowCapability(c, request, revision)
}

func (s *Server) handleGetCapabilityInvocation(c echo.Context) error {
	if handled, err := s.getWorkflowCapabilityInvocation(c, false); handled {
		return err
	}
	return capabilityHTTPError(c, appcapabilities.ErrInvocationNotFound)
}

func (s *Server) handleCancelCapabilityInvocation(c echo.Context) error {
	if handled, err := s.cancelWorkflowCapabilityInvocation(c); handled {
		return err
	}
	return capabilityHTTPError(c, appcapabilities.ErrInvocationNotFound)
}

func capabilityHTTPError(c echo.Context, err error) error {
	status := http.StatusInternalServerError
	code := "capability_service_error"
	switch {
	case errors.Is(err, appcapabilities.ErrAccessDenied):
		status, code = http.StatusForbidden, "capability_access_denied"
	case errors.Is(err, capability.ErrInvalidDefinition), errors.Is(err, capability.ErrInvalidRevision):
		status, code = http.StatusBadRequest, "capability_request_invalid"
	case errors.Is(err, appcapabilities.ErrRevisionConflict):
		status, code = http.StatusConflict, "capability_revision_conflict"
	case errors.Is(err, appcapabilities.ErrNotFound), errors.Is(err, appcapabilities.ErrRevisionNotFound):
		status, code = http.StatusNotFound, "capability_not_found"
	case errors.Is(err, appcapabilities.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, appcapabilities.ErrInvocationLaneActive):
		status, code = http.StatusConflict, "invocation_lane_active"
	case errors.Is(err, appcapabilities.ErrInvocationIdentityConflict):
		status, code = http.StatusConflict, "invocation_identity_conflict"
	case errors.Is(err, appcapabilities.ErrWorkflowInvocationCancelled), errors.Is(err, publication.ErrAdmissionClosed):
		status, code = http.StatusConflict, "invocation_cancelled"
	case errors.Is(err, appcapabilities.ErrWorkflowSourceRequired):
		status, code = http.StatusBadRequest, "workflow_source_required"
	case errors.Is(err, appcapabilities.ErrInvocationNotFound):
		status, code = http.StatusNotFound, "invocation_not_found"
	case errors.Is(err, appcapabilities.ErrInvocationTerminal):
		status, code = http.StatusConflict, "invocation_terminal"
	case errors.Is(err, appcapabilities.ErrQuotaExceeded):
		status, code = http.StatusTooManyRequests, "capability_quota_exceeded"
	}
	message := code
	if status >= http.StatusInternalServerError {
		slog.Error("capability operation failed", "code", code, "error", err)
	}
	if status == http.StatusBadRequest {
		message = err.Error()
	}
	return c.JSON(status, map[string]string{"code": code, "error": message})
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
			if c.Get(authSourceContextKey) == authSourceCapabilityApp {
				p, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal)
				if ok && p.Allows(action) && action != "manage" {
					return next(c)
				}
				return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient application scope"})
			}
			return RequireAtLeast(roleDeveloper)(next)(c)
		}
	}
}

func (s *Server) handleListCapabilityInvocations(c echo.Context) error {
	combined := []any{}
	if principal, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal); ok {
		if service := s.workflowCapabilityInvocations(); service != nil {
			workflows, err := service.List(c.Request().Context(), principal.WorkspaceID, principal.AppID)
			if err != nil {
				return capabilityHTTPError(c, err)
			}
			for _, item := range workflows {
				combined = append(combined, item)
			}
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"invocations": combined})
}
func (s *Server) handleGetCapabilityInvocationEvents(c echo.Context) error {
	if handled, err := s.getWorkflowCapabilityInvocation(c, true); handled {
		return err
	}
	return capabilityHTTPError(c, appcapabilities.ErrInvocationNotFound)
}
func (s *Server) handleCapabilityQuota(c echo.Context) error {
	workspace, _ := c.Get("tenant").(string)
	store := appcapabilities.NewPGStore(s.Pool)
	if c.Request().Method == http.MethodGet {
		q, err := store.GetQuota(c.Request().Context(), workspace)
		if err != nil {
			return capabilityHTTPError(c, err)
		}
		return c.JSON(200, q)
	}
	var q appcapabilities.Quota
	if err := decodeCapabilityBody(c, &q); err != nil {
		return c.JSON(400, map[string]string{"code": "capability_request_invalid"})
	}
	q, err := store.SetQuota(c.Request().Context(), workspace, q)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(200, q)
}
