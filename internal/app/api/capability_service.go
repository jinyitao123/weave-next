package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

const (
	servicePrincipalContextKey = "service_app_principal"
	capabilityRequestMaxBytes  = (1 << 20) + 4096
	capabilityCancelMaxBytes   = 4096
	capabilitySweepInterval    = 2 * time.Second
)

func ServiceAppAuthMiddleware(
	getter func() *capabilities.InvocationService,
) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authorization := c.Request().Header.Get(echo.HeaderAuthorization)
			raw := strings.TrimPrefix(authorization, "Bearer ")
			if authorization == "" || raw == authorization || !strings.HasPrefix(raw, "wv_app_") {
				return capabilityAPIError(c, http.StatusUnauthorized, "service_credential_invalid")
			}
			service := getter()
			if service == nil || service.Store == nil {
				return capabilityAPIError(c, http.StatusServiceUnavailable, "capability_service_unavailable")
			}
			principal, err := service.Store.ValidateCredential(c.Request().Context(), raw)
			if err != nil {
				return capabilityAPIError(c, http.StatusUnauthorized, "service_credential_invalid")
			}
			c.Set(servicePrincipalContextKey, principal)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = service.Store.TouchCredential(ctx, principal.WorkspaceID, principal.CredentialID)
			}()
			return next(c)
		}
	}
}

func RequireServiceScope(scope string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			principal, ok := capabilityPrincipal(c)
			if !ok || !principal.HasScope(scope) {
				return capabilityAPIError(c, http.StatusForbidden, "service_scope_denied")
			}
			return next(c)
		}
	}
}

func capabilityPrincipal(c echo.Context) (capabilities.Principal, bool) {
	principal, ok := c.Get(servicePrincipalContextKey).(capabilities.Principal)
	return principal, ok
}

func (s *Server) handleSubmitCapabilityInvocation(c echo.Context) error {
	principal, ok := capabilityPrincipal(c)
	if !ok || s.Capabilities == nil {
		return capabilityAPIError(c, http.StatusServiceUnavailable, "capability_service_unavailable")
	}
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil || version < 1 {
		return capabilityAPIError(c, http.StatusBadRequest, "capability_request_invalid")
	}
	var request struct {
		RequestID      string          `json:"request_id"`
		Input          json.RawMessage `json:"input"`
		ConcurrencyKey *string         `json:"concurrency_key,omitempty"`
	}
	if err := decodeBoundedJSON(c, capabilityRequestMaxBytes, &request); err != nil {
		return capabilityAPIError(c, http.StatusBadRequest, "capability_request_invalid")
	}
	receipt, err := s.Capabilities.Submit(c.Request().Context(), capabilities.SubmitInvocationRequest{
		Principal: principal, CapabilityID: c.Param("id"), ReleaseVersion: version,
		RequestID: request.RequestID, Input: request.Input, ConcurrencyKey: request.ConcurrencyKey,
	})
	if err != nil {
		return respondCapabilityServiceError(c, err)
	}
	status := http.StatusAccepted
	if receipt.Replay {
		status = http.StatusOK
	}
	c.Response().Header().Set("Location", receipt.QueryPath)
	return c.JSON(status, receipt)
}

func (s *Server) handleGetCapabilityInvocation(c echo.Context) error {
	principal, ok := capabilityPrincipal(c)
	if !ok || s.Capabilities == nil {
		return capabilityAPIError(c, http.StatusServiceUnavailable, "capability_service_unavailable")
	}
	status, err := s.Capabilities.Get(c.Request().Context(), principal, c.Param("id"))
	if err != nil {
		return respondCapabilityServiceError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}

func (s *Server) handleCancelCapabilityInvocation(c echo.Context) error {
	principal, ok := capabilityPrincipal(c)
	if !ok || s.Capabilities == nil {
		return capabilityAPIError(c, http.StatusServiceUnavailable, "capability_service_unavailable")
	}
	var request struct {
		RequestID    string `json:"request_id,omitempty"`
		InvocationID string `json:"invocation_id,omitempty"`
		Reason       string `json:"reason"`
	}
	if err := decodeBoundedJSON(c, capabilityCancelMaxBytes, &request); err != nil {
		return capabilityAPIError(c, http.StatusBadRequest, "capability_request_invalid")
	}
	receipt, err := s.Capabilities.CancelInvocation(c.Request().Context(), capabilities.CancelInvocationRequest{
		Principal: principal, RequestID: request.RequestID,
		InvocationID: request.InvocationID, Reason: request.Reason,
	})
	if err != nil {
		return respondCapabilityServiceError(c, err)
	}
	return c.JSON(http.StatusAccepted, receipt)
}

func decodeBoundedJSON(c echo.Context, limit int64, destination any) error {
	reader := http.MaxBytesReader(c.Response().Writer, c.Request().Body, limit)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func respondCapabilityServiceError(c echo.Context, err error) error {
	var schema *capabilities.SchemaViolationError
	var resource *capabilities.ResourceLimitError
	switch {
	case errors.As(err, &schema):
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error": "input_contract_violation", "problems": schema.Problems,
		})
	case errors.As(err, &resource):
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error": "resource_limit_exceeded", "limit": resource.Limit,
		})
	case errors.Is(err, capabilities.ErrInvalid):
		return capabilityAPIError(c, http.StatusBadRequest, "capability_request_invalid")
	case errors.Is(err, capabilities.ErrCredentialInvalid):
		return capabilityAPIError(c, http.StatusUnauthorized, "service_credential_invalid")
	case errors.Is(err, capabilities.ErrScopeDenied):
		return capabilityAPIError(c, http.StatusForbidden, "service_scope_denied")
	case errors.Is(err, capabilities.ErrNotFound):
		return capabilityAPIError(c, http.StatusNotFound, "invocation_not_found")
	case errors.Is(err, capabilities.ErrRequestConflict):
		return capabilityAPIError(c, http.StatusConflict, "request_id_conflict")
	case errors.Is(err, capabilities.ErrCancelledBeforeSubmit):
		return capabilityAPIError(c, http.StatusConflict, "request_cancelled_before_submit")
	case errors.Is(err, capabilities.ErrCapacityExceeded):
		c.Response().Header().Set("Retry-After", "1")
		return capabilityAPIError(c, http.StatusTooManyRequests, "invocation_capacity_exceeded")
	case errors.Is(err, capabilities.ErrConcurrencyBusy):
		c.Response().Header().Set("Retry-After", "1")
		return capabilityAPIError(c, http.StatusTooManyRequests, "concurrency_key_busy")
	case errors.Is(err, capabilities.ErrGrantUnavailable),
		errors.Is(err, capabilities.ErrDisabled), errors.Is(err, capabilities.ErrReleaseUnsafe),
		errors.Is(err, workflow.ErrNotPublished),
		errors.Is(err, workflow.ErrWorkflowScheduleAdmissionDenied):
		return capabilityAPIError(c, http.StatusForbidden, "capability_not_available")
	default:
		slog.Error("capability service request failed", "error", err)
		return capabilityAPIError(c, http.StatusServiceUnavailable, "capability_service_failure")
	}
}

func capabilityAPIError(c echo.Context, status int, code string) error {
	return c.JSON(status, map[string]string{"error": code})
}

func (s *Server) runCapabilityControlLoop(ctx context.Context) {
	ticker := time.NewTicker(capabilitySweepInterval)
	defer ticker.Stop()
	for {
		if _, err := s.Capabilities.SweepControls(ctx, temporaryTeamRunWorkerBatchSize); err != nil && ctx.Err() == nil {
			slog.Error("capability invocation control sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
