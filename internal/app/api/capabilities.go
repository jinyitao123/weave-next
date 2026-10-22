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
	"strings"

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
	if err := s.capabilityRunner().ValidateRuntime(c.Request().Context(), workspace, request.Definition.Runtime); err != nil {
		return c.JSON(422, map[string]string{"code": "capability_runtime_unavailable"})
	}
	invocation, replayed, err := s.Capabilities.Debug(c.Request().Context(), appcapabilities.DebugRequest{
		WorkspaceID: workspace, ApplicationID: capabilityApplicationID(c), RequestID: request.RequestID, Definition: request.Definition, Input: request.Input, ActorUserID: getUserID(c), MaxSteps: 100,
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
	if draft.Workflow != nil {
		service := s.workflowCapabilityInvocations()
		if service == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workflow invocation service unavailable"})
		}
		if err := service.ValidateDefinition(c.Request().Context(), workspaceID, draft); err != nil {
			return capabilityHTTPError(c, err)
		}
	} else if err := s.capabilityRunner().ValidateRuntime(c.Request().Context(), workspaceID, draft.Runtime); err != nil {
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
	applicationID := capabilityApplicationID(c)
	credentialID := ""
	if p, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal); ok {
		credentialID = p.CredentialID
		if err := s.CapabilityAccess.AuthorizeInvocation(c.Request().Context(), p, c.Param("capabilityID"), revision); err != nil {
			return capabilityHTTPError(c, err)
		}
	}
	published, err := appcapabilities.NewPGStore(s.Pool).GetRevision(c.Request().Context(), workspaceID, c.Param("capabilityID"), revision)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	if published.Definition.Workflow != nil {
		return s.invokeWorkflowCapability(c, request, revision)
	}
	if request.InvocationID.supplied || request.Lane != "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "capability_request_invalid", "error": "caller invocation IDs and lanes require a published workflow capability"})
	}
	quota, quotaErr := appcapabilities.NewPGStore(s.Pool).GetQuota(c.Request().Context(), workspaceID)
	if quotaErr != nil {
		return capabilityHTTPError(c, quotaErr)
	}
	invocation, replayed, err := s.Capabilities.Invoke(c.Request().Context(), appcapabilities.InvokeRequest{
		WorkspaceID: workspaceID, ApplicationID: applicationID, CredentialID: credentialID,
		RequestID: request.RequestID, CapabilityID: c.Param("capabilityID"), Revision: revision, Input: request.Input,
		ActorUserID: getUserID(c), MaxSteps: quota.MaxStepsPerInvocation,
	})
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"application_id": invocation.ApplicationID,
		"run_kind":       invocation.RunKind, "definition_hash": invocation.DefinitionHash,
		"invocation_id": invocation.InvocationID, "capability_id": invocation.CapabilityID,
		"revision": invocation.Revision, "task_id": invocation.TaskID, "status": invocation.Status,
		"result_state": invocation.ResultState, "replayed": replayed,
	})
}

func capabilityApplicationID(c echo.Context) string {
	if p, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal); ok {
		return p.AppID
	}
	applicationID, _ := c.Get(apiKeyIDContextKey).(string)
	if actor, _ := c.Get(workbenchActorContextKey).(string); applicationID != "" && actor != "" {
		return applicationID + ":workbench:" + actor
	}
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
	if handled, err := s.getWorkflowCapabilityInvocation(c, false); handled {
		return err
	}
	invocation, err := s.Capabilities.GetInvocation(c.Request().Context(), workspaceID, capabilityApplicationID(c), c.Param("invocationID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	response := map[string]any{
		"capability_name": invocation.CapabilityName, "step_names": invocation.StepNames, "result_steps": invocation.ResultSteps,
		"application_id": invocation.ApplicationID,
		"run_kind":       invocation.RunKind, "definition_hash": invocation.DefinitionHash,
		"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID,
		"capability_id": invocation.CapabilityID,
		"status":        invocation.Status, "result_state": invocation.ResultState,
		"result": invocation.Result, "error": publicCapabilityError(invocation), "actor_user_id": invocation.ActorUserID, "runtime_id": invocation.RuntimeID, "used_steps": invocation.UsedSteps, "max_steps": invocation.MaxSteps,
		"physical_usage": invocation.PhysicalUsage, "unreported_attempts": invocation.UnreportedAttempts,
		"failure_reason": publicCapabilityFailure(invocation),
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
	// Existing inline invocations keep their established cancellation owner.
	// Unknown IDs from an application are handled by workflow cancellation,
	// which records the application's pre-admission stop intent.
	if _, err := s.Capabilities.GetInvocation(c.Request().Context(), workspaceID, capabilityApplicationID(c), c.Param("invocationID")); errors.Is(err, appcapabilities.ErrInvocationNotFound) {
		if handled, cancelErr := s.cancelWorkflowCapabilityInvocation(c); handled {
			return cancelErr
		}
	} else if err != nil {
		return capabilityHTTPError(c, err)
	}
	invocation, err := s.Capabilities.CancelInvocation(c.Request().Context(), workspaceID, capabilityApplicationID(c), c.Param("invocationID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	if s.TaskWorker != nil && invocation.TaskID != "" {
		if err := s.TaskWorker.CancelTask(c.Request().Context(), workspaceID, invocation.TaskID); err != nil {
			return capabilityHTTPError(c, err)
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"invocation_id": invocation.InvocationID, "task_id": invocation.TaskID, "status": invocation.Status})
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

func publicCapabilityError(i appcapabilities.Invocation) string {
	if i.Status == "failed" {
		return "capability_execution_failed"
	}
	return ""
}

func publicCapabilityFailure(i appcapabilities.Invocation) string {
	if i.Status != "failed" {
		return ""
	}
	switch {
	case i.Error == "capability_model_unavailable":
		return "model_unavailable"
	case i.Error == "application_authorization_revoked" || i.Error == appcapabilities.ErrAccessDenied.Error():
		return "access_revoked"
	case i.Error == "execution_deadline_expired" || strings.Contains(i.Error, "context deadline exceeded"):
		return "deadline"
	case strings.Contains(i.Error, "output schema") || strings.Contains(i.Error, "invalid JSON output"):
		return "output_invalid"
	default:
		return "failed"
	}
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
			if c.Get(authSourceContextKey) == authSourceAPIKey {
				scopes, _ := c.Get(scopesContextKey).([]string)
				roles, _ := c.Get("roles").([]string)
				isAdmin := false
				for _, role := range roles {
					if role == "admin" {
						isAdmin = true
					}
				}
				for _, scope := range scopes {
					if scope == "capabilities:"+action || (scope == "admin" && isAdmin) {
						return next(c)
					}
				}
				return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient capability scope"})
			}
			return RequireAnyRole("admin", "owner", "developer")(next)(c)
		}
	}
}

type resumeCapabilityRequest struct {
	StepID   string          `json:"step_id"`
	Response json.RawMessage `json:"response"`
}

func (s *Server) handleListCapabilityInvocations(c echo.Context) error {
	store := appcapabilities.NewPGStore(s.Pool)
	workspace, _ := c.Get("tenant").(string)
	items, err := store.ListInvocations(c.Request().Context(), workspace, capabilityApplicationID(c), getUserID(c))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	combined := make([]any, 0, len(items))
	for _, item := range items {
		combined = append(combined, item)
	}
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
	store := appcapabilities.NewPGStore(s.Pool)
	workspace, _ := c.Get("tenant").(string)
	inv, err := s.Capabilities.GetInvocation(c.Request().Context(), workspace, capabilityApplicationID(c), c.Param("invocationID"))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	events, err := store.ListEvents(c.Request().Context(), workspace, inv.InvocationID)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	human, _ := store.GetHumanTask(c.Request().Context(), workspace, inv.InvocationID)
	return c.JSON(http.StatusOK, map[string]any{"invocation": inv, "events": events, "human_task": human})
}
func (s *Server) handleResumeCapabilityInvocation(c echo.Context) error {
	var request resumeCapabilityRequest
	if err := decodeCapabilityBody(c, &request); err != nil {
		return c.JSON(400, map[string]string{"code": "capability_request_invalid"})
	}
	workspace, _ := c.Get("tenant").(string)
	store := appcapabilities.NewPGStore(s.Pool)
	inv, err := store.ResumeHuman(c.Request().Context(), workspace, capabilityApplicationID(c), c.Param("invocationID"), request.StepID, request.Response)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	return c.JSON(http.StatusAccepted, inv)
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
