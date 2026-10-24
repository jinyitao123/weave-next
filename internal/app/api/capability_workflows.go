package api

import (
	"errors"
	"net/http"

	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/labstack/echo/v4"
)

// This adapter selects the published-workflow invocation service. Admission,
// authorization, cancellation and projection remain in their existing owners.
func (s *Server) workflowCapabilityInvocations() *appcapabilities.WorkflowInvocationService {
	if s.GetPool() == nil || s.Capabilities == nil || s.Workflow == nil {
		return nil
	}
	kernel, ok := s.KernelPublication.(publication.PublishedService)
	if !ok {
		return nil
	}
	service := appcapabilities.NewWorkflowInvocationService(s.GetPool(), s.Capabilities, s.Workflow, kernel, s.Tasks, s.teamRunCancel)
	service.UseRuntimeAuthorizations(s.runtimeCredentialAuthorizations())
	return service
}

// runtimeCredentialAuthorizations is the process's one store of runtime
// credential checks; its full check includes the published team scope when
// the publication authority is configured.
func (s *Server) runtimeCredentialAuthorizations() *appcapabilities.RuntimeCredentialAuthorizations {
	s.runtimeCredentialsOnce.Do(func() {
		s.runtimeCredentials = &appcapabilities.RuntimeCredentialAuthorizations{}
		if s.PublicationAuthority != nil {
			s.runtimeCredentials.Team = s.PublicationAuthority
		}
	})
	return s.runtimeCredentials
}

func (s *Server) invokeWorkflowCapability(c echo.Context, request invokeCapabilityRequest, revision int64) error {
	principal, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal)
	if !ok {
		return capabilityHTTPError(c, appcapabilities.ErrAccessDenied)
	}
	service := s.workflowCapabilityInvocations()
	if service == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workflow invocation service unavailable"})
	}
	invocation, replayed, err := service.Invoke(c.Request().Context(), appcapabilities.WorkflowInvokeRequest{
		WorkspaceID: principal.WorkspaceID, ApplicationID: principal.AppID, CredentialID: principal.CredentialID,
		CapabilityID: c.Param("capabilityID"), Revision: revision, RequestID: request.RequestID,
		InvocationID: request.InvocationID.value, Lane: request.Lane, Input: request.Input,
		InvocationIDWasSupplied: request.InvocationID.supplied,
		Priority:                request.Priority, FairnessKey: request.FairnessKey,
	})
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	invocation.Replayed = replayed
	return c.JSON(http.StatusAccepted, invocation)
}

func (s *Server) getWorkflowCapabilityInvocation(c echo.Context, withEvents bool) (bool, error) {
	principal, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal)
	if !ok {
		return false, nil
	}
	service := s.workflowCapabilityInvocations()
	if service == nil {
		return false, nil
	}
	invocation, err := service.Get(c.Request().Context(), principal.WorkspaceID, principal.AppID, c.Param("invocationID"))
	if errors.Is(err, appcapabilities.ErrInvocationNotFound) {
		return false, nil
	}
	if err != nil {
		return true, capabilityHTTPError(c, err)
	}
	if !withEvents {
		return true, c.JSON(http.StatusOK, invocation)
	}
	events, err := service.Events(c.Request().Context(), principal.WorkspaceID, principal.AppID, c.Param("invocationID"))
	if err != nil {
		return true, capabilityHTTPError(c, err)
	}
	return true, c.JSON(http.StatusOK, map[string]any{"invocation": invocation, "events": events, "human_task": nil})
}

func (s *Server) cancelWorkflowCapabilityInvocation(c echo.Context) (bool, error) {
	principal, ok := c.Get(capabilityPrincipalKey).(appcapabilities.ApplicationPrincipal)
	if !ok {
		return false, nil
	}
	service := s.workflowCapabilityInvocations()
	if service == nil {
		return true, c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "workflow invocation service unavailable"})
	}
	invocation, err := service.Cancel(c.Request().Context(), principal.WorkspaceID, principal.AppID, principal.CredentialID, c.Param("invocationID"))
	if err != nil {
		return true, capabilityHTTPError(c, err)
	}
	return true, c.JSON(http.StatusOK, invocation)
}
