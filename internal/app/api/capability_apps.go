package api

import (
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
	"net/http"
	"strings"
)

const authSourceCapabilityApp = "capability_application"
const capabilityPrincipalKey = "capability_principal"

func (s *Server) capabilityAuthentication() echo.MiddlewareFunc {
	legacy := AuthMiddleware(s.Config.JWTSecret, func() *apikeys.Store { return s.KeyStore }, func() *users.Store { return s.UserStore })
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		fallback := legacy(next)
		return func(c echo.Context) error {
			c.Response().Header().Set("Cache-Control", "no-store")
			authorization := c.Request().Header.Get("Authorization")
			if !strings.HasPrefix(authorization, "Bearer ") {
				return fallback(c)
			}
			raw := strings.TrimPrefix(authorization, "Bearer ")
			if !strings.HasPrefix(raw, capabilities.CredentialPrefix) {
				return fallback(c)
			}
			if s.CapabilityAccess == nil {
				return c.JSON(401, map[string]string{"error": "invalid application credential"})
			}
			principal, err := s.CapabilityAccess.Authenticate(c.Request().Context(), raw)
			if err != nil {
				return c.JSON(401, map[string]string{"error": "invalid application credential"})
			}
			c.Set("tenant", principal.WorkspaceID)
			c.Set(authSourceContextKey, authSourceCapabilityApp)
			c.Set(capabilityPrincipalKey, principal)
			return next(c)
		}
	}
}

func (s *Server) handleCapabilityApps(c echo.Context) error {
	if s.CapabilityAccess == nil {
		return c.JSON(503, map[string]string{"error": "capability access service unavailable"})
	}
	snapshot, err := s.CapabilityAccess.Snapshot(c.Request().Context(), getTenant(c), getUserID(c))
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(200, snapshot)
}

type capabilityAppAction struct {
	Action       string   `json:"action"`
	AppID        string   `json:"app_id,omitempty"`
	Name         string   `json:"name,omitempty"`
	CredentialID string   `json:"credential_id,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	CapabilityID string   `json:"capability_id,omitempty"`
	Revision     int64    `json:"revision,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
}

func (s *Server) handleCapabilityAppAction(c echo.Context) error {
	if s.CapabilityAccess == nil {
		return c.JSON(503, map[string]string{"error": "capability access service unavailable"})
	}
	var request capabilityAppAction
	if err := decodeCapabilityBody(c, &request); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid application action"})
	}
	ctx, ws, actor := c.Request().Context(), getTenant(c), getUserID(c)
	c.Response().Header().Set("Cache-Control", "no-store")
	switch request.Action {
	case "create":
		if strings.TrimSpace(request.Name) == "" || len(request.Name) > 160 {
			return c.JSON(400, map[string]string{"error": "invalid application name"})
		}
		app, err := s.CapabilityAccess.CreateApplication(ctx, ws, actor, request.Name)
		if err != nil {
			return capabilityHTTPError(c, err)
		}
		return c.JSON(201, app)
	case "enable":
		if request.AppID == "" || request.Enabled == nil {
			return c.JSON(400, map[string]string{"error": "application and enabled state required"})
		}
		if err := s.CapabilityAccess.SetApplicationEnabled(ctx, ws, actor, request.AppID, *request.Enabled); err != nil {
			return capabilityHTTPError(c, err)
		}
	case "issue":
		if request.AppID == "" || strings.TrimSpace(request.Name) == "" || len(request.Name) > 160 {
			return c.JSON(400, map[string]string{"error": "application and credential name required"})
		}
		if len(request.Scopes) < 1 || len(request.Scopes) > 3 {
			return c.JSON(400, map[string]string{"error": "credential scopes required"})
		}
		seen := map[string]bool{}
		for _, scope := range request.Scopes {
			if (scope != "invoke" && scope != "read" && scope != "cancel") || seen[scope] {
				return c.JSON(400, map[string]string{"error": "invalid credential scopes"})
			}
			seen[scope] = true
		}
		credential, raw, err := s.CapabilityAccess.IssueCredential(ctx, ws, actor, request.AppID, request.Name, request.Scopes)
		if err != nil {
			return capabilityHTTPError(c, err)
		}
		return c.JSON(201, map[string]any{"credential": credential, "key": raw})
	case "revoke":
		if request.AppID == "" || request.CredentialID == "" {
			return c.JSON(400, map[string]string{"error": "application and credential required"})
		}
		if err := s.CapabilityAccess.RevokeCredential(ctx, ws, actor, request.AppID, request.CredentialID); err != nil {
			return capabilityHTTPError(c, err)
		}
	case "grant":
		if request.AppID == "" || request.CapabilityID == "" || request.Revision < 1 || request.Enabled == nil {
			return c.JSON(400, map[string]string{"error": "application, version and grant state required"})
		}
		if err := s.CapabilityAccess.SetGrant(ctx, ws, actor, capabilities.VersionGrant{AppID: request.AppID, CapabilityID: request.CapabilityID, Revision: request.Revision, Enabled: *request.Enabled}); err != nil {
			return capabilityHTTPError(c, err)
		}
	default:
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "unknown application action"})
	}
	return c.NoContent(204)
}
