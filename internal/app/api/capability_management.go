package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/labstack/echo/v4"
)

const capabilityManagementMaxBytes = 1 << 20

type capabilityManagementAction struct {
	Action          string                       `json:"action"`
	AppID           string                       `json:"app_id,omitempty"`
	CapabilityID    string                       `json:"capability_id,omitempty"`
	CredentialID    string                       `json:"credential_id,omitempty"`
	GrantID         string                       `json:"grant_id,omitempty"`
	InvocationID    string                       `json:"invocation_id,omitempty"`
	Name            string                       `json:"name,omitempty"`
	Key             string                       `json:"key,omitempty"`
	Description     string                       `json:"description,omitempty"`
	Scopes          []string                     `json:"scopes,omitempty"`
	ExpiresAt       *time.Time                   `json:"expires_at,omitempty"`
	Enabled         *bool                        `json:"enabled,omitempty"`
	MaxConcurrent   int                          `json:"max_concurrent,omitempty"`
	WorkflowID      string                       `json:"workflow_id,omitempty"`
	WorkflowVersion int                          `json:"workflow_version,omitempty"`
	ReleaseVersion  int                          `json:"release_version,omitempty"`
	ExecutionLimits capabilities.ExecutionLimits `json:"execution_limits,omitempty"`
	ResultPolicy    capabilities.ResultPolicy    `json:"result_policy,omitempty"`
	Reason          string                       `json:"reason,omitempty"`
}

func (s *Server) handleGetCapabilityManagement(c echo.Context) error {
	if s.Capabilities == nil || s.Capabilities.Store == nil {
		return capabilityManagementError(c, http.StatusServiceUnavailable, "capability_management_unavailable")
	}
	snapshot, err := s.Capabilities.Store.ManagementSnapshot(
		c.Request().Context(), getTenant(c), s.Capabilities,
	)
	if err != nil {
		return writeCapabilityManagementError(c, err)
	}
	return c.JSON(http.StatusOK, snapshot)
}

func (s *Server) handleCapabilityManagementAction(c echo.Context) error {
	if s.Capabilities == nil || s.Capabilities.Store == nil {
		return capabilityManagementError(c, http.StatusServiceUnavailable, "capability_management_unavailable")
	}
	var request capabilityManagementAction
	if err := decodeBoundedJSON(c, capabilityManagementMaxBytes, &request); err != nil {
		return capabilityManagementError(c, http.StatusBadRequest, "capability_management_invalid")
	}
	ctx := c.Request().Context()
	workspaceID, userID := getTenant(c), getUserID(c)
	store := s.Capabilities.Store
	switch request.Action {
	case "create_app":
		app, err := store.CreateApp(ctx, capabilities.CreateAppRequest{
			WorkspaceID: workspaceID, Name: request.Name, Description: request.Description,
			MaxConcurrentInvocations: request.MaxConcurrent, CreatedBy: userID,
		})
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusCreated, map[string]any{"app": app})
	case "set_app_enabled":
		if request.Enabled == nil {
			return capabilityManagementError(c, http.StatusBadRequest, "capability_management_invalid")
		}
		if err := store.SetAppEnabled(ctx, workspaceID, request.AppID, userID, *request.Enabled); err != nil {
			return writeCapabilityManagementError(c, err)
		}
	case "create_credential":
		credential, raw, err := store.CreateCredential(ctx, capabilities.CreateCredentialRequest{
			WorkspaceID: workspaceID, AppID: request.AppID, Name: request.Name,
			Scopes: request.Scopes, ExpiresAt: request.ExpiresAt, CreatedBy: userID,
		})
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusCreated, map[string]any{
			"credential": credential, "secret": raw,
		})
	case "revoke_credential":
		if err := store.RevokeCredential(ctx, workspaceID, request.CredentialID, userID); err != nil {
			return writeCapabilityManagementError(c, err)
		}
	case "create_capability":
		capability, err := store.CreateCapability(ctx, capabilities.CreateCapabilityRequest{
			WorkspaceID: workspaceID, Key: request.Key, Name: request.Name,
			Description: request.Description, CreatedBy: userID,
		})
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusCreated, map[string]any{"capability": capability})
	case "set_capability_enabled":
		if request.Enabled == nil {
			return capabilityManagementError(c, http.StatusBadRequest, "capability_management_invalid")
		}
		if err := store.SetCapabilityEnabled(
			ctx, workspaceID, request.CapabilityID, userID, *request.Enabled,
		); err != nil {
			return writeCapabilityManagementError(c, err)
		}
	case "publish_release":
		release, err := store.PublishRelease(ctx, capabilities.PublishReleaseRequest{
			WorkspaceID: workspaceID, CapabilityID: request.CapabilityID,
			Version: request.ReleaseVersion, WorkflowID: request.WorkflowID,
			WorkflowVersion: request.WorkflowVersion, ExecutionLimits: request.ExecutionLimits,
			ResultPolicy: request.ResultPolicy, CreatedBy: userID,
		})
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusCreated, map[string]any{"release": release})
	case "set_release_enabled":
		if request.Enabled == nil {
			return capabilityManagementError(c, http.StatusBadRequest, "capability_management_invalid")
		}
		if err := store.SetReleaseEnabled(
			ctx, workspaceID, request.CapabilityID, request.ReleaseVersion,
			userID, *request.Enabled,
		); err != nil {
			return writeCapabilityManagementError(c, err)
		}
	case "grant_release":
		grant, err := store.GrantRelease(ctx, capabilities.GrantReleaseRequest{
			WorkspaceID: workspaceID, AppID: request.AppID,
			CapabilityID: request.CapabilityID, ReleaseVersion: request.ReleaseVersion,
			MaxConcurrent: request.MaxConcurrent, GrantedBy: userID,
		})
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusCreated, map[string]any{"grant": grant})
	case "revoke_grant":
		if err := store.RevokeGrant(ctx, workspaceID, request.GrantID, userID); err != nil {
			return writeCapabilityManagementError(c, err)
		}
	case "cancel_invocation":
		receipt, err := s.Capabilities.AdminCancelInvocation(
			ctx, workspaceID, userID, request.InvocationID, request.Reason,
		)
		if err != nil {
			return writeCapabilityManagementError(c, err)
		}
		return c.JSON(http.StatusAccepted, map[string]any{"cancellation": receipt})
	default:
		return capabilityManagementError(c, http.StatusBadRequest, "capability_management_invalid")
	}
	return c.NoContent(http.StatusNoContent)
}

func writeCapabilityManagementError(c echo.Context, err error) error {
	status, code := http.StatusServiceUnavailable, "capability_management_failure"
	switch {
	case errors.Is(err, capabilities.ErrInvalid):
		status, code = http.StatusBadRequest, "capability_management_invalid"
	case errors.Is(err, capabilities.ErrNotFound), errors.Is(err, capabilities.ErrGrantUnavailable):
		status, code = http.StatusNotFound, "capability_management_missing"
	case errors.Is(err, capabilities.ErrScopeDenied):
		status, code = http.StatusForbidden, "capability_management_forbidden"
	case errors.Is(err, capabilities.ErrReleaseConflict), errors.Is(err, capabilities.ErrGrantConflict):
		status, code = http.StatusConflict, "capability_management_conflict"
	case errors.Is(err, capabilities.ErrDisabled), errors.Is(err, capabilities.ErrReleaseUnsafe):
		status, code = http.StatusUnprocessableEntity, "capability_release_unavailable"
	}
	return capabilityManagementError(c, status, code)
}

func capabilityManagementError(c echo.Context, status int, code string) error {
	return c.JSON(status, map[string]string{"code": code, "error": code})
}
