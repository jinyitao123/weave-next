package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/app/adminui"
	"github.com/labstack/echo/v4"
)

// The admin console is served from the same origin as the API. Its session
// credential (an operator API key or a product session issued by external
// identity exchange) lives only in an HttpOnly cookie; a Pre middleware turns
// that cookie into the ordinary Authorization header so every existing route
// keeps its own authentication and authorization unchanged.
const (
	adminSessionCookie = "weave_admin_session"
	// adminRequestHeader must accompany every state-changing cookie request. A
	// cross-site form cannot set it, and a cross-site script cannot send it
	// without a CORS preflight that this origin does not grant.
	adminRequestHeader = "X-Weave-Admin"
	adminSessionTTL    = externalSessionTTL
	// adminAPIKeyIdentitySource marks a console session obtained with an
	// operator API key.
	adminAPIKeyIdentitySource = "api_key"
)

func (s *Server) registerAdminConsole() {
	var connect []string
	if origin := s.adminForgeOrigin(); origin != "" {
		connect = append(connect, origin)
	}
	console := echo.WrapHandler(adminui.Handler(adminui.Options{ConnectSources: connect}))
	for _, path := range []string{adminui.Prefix, adminui.Prefix + "/*"} {
		s.Echo.GET(path, console)
		s.Echo.HEAD(path, console)
	}
	s.Echo.GET("/v1/admin/config", s.handleAdminConfig)
	s.Echo.POST("/v1/admin/session", s.handleCreateAdminSession)
	s.Echo.DELETE("/v1/admin/session", s.handleDeleteAdminSession)
}

// adminSessionCookieMiddleware runs before routing.
func adminSessionCookieMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := c.Request()
		if !strings.HasPrefix(request.URL.Path, "/v1/") || strings.HasPrefix(request.URL.Path, "/v1/runtime/") {
			return next(c)
		}
		unsafe := request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions
		cookie, err := request.Cookie(adminSessionCookie)
		hasCookie := err == nil && cookie.Value != ""
		isAdminSessionCall := request.URL.Path == "/v1/admin/session"
		if unsafe && (hasCookie || isAdminSessionCall) && !adminRequestTrusted(request) {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "admin_request_rejected"})
		}
		if hasCookie && request.Header.Get(echo.HeaderAuthorization) == "" {
			request.Header.Set(echo.HeaderAuthorization, "Bearer "+cookie.Value)
		}
		return next(c)
	}
}

func adminRequestTrusted(request *http.Request) bool {
	if request.Header.Get(adminRequestHeader) != "1" {
		return false
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}

func adminRequestSecure(request *http.Request) bool {
	return request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https")
}

// adminForgeOrigin returns the configured browser-facing Forge origin, or ""
// when it is absent or not a plain HTTP(S) origin.
func (s *Server) adminForgeOrigin() string {
	if s.Config == nil || s.Config.AdminForgeURL == "" {
		return ""
	}
	parsed, err := url.Parse(s.Config.AdminForgeURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (s *Server) handleAdminConfig(c echo.Context) error {
	forge := s.adminForgeOrigin()
	methods := []string{"api_key"}
	if forge != "" && s.ExternalIdentity != nil {
		methods = append([]string{"forge"}, methods...)
	}
	response := map[string]any{"sign_in_methods": methods, "secure": adminRequestSecure(c.Request())}
	if forge != "" {
		response["forge_origin"] = forge
	}
	return c.JSON(http.StatusOK, response)
}

type adminSessionRequest struct {
	APIKey     string `json:"api_key"`
	ForgeToken string `json:"forge_token"`
}

type adminSessionView struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Source    string    `json:"source"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleCreateAdminSession(c echo.Context) error {
	var request adminSessionRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request.APIKey, request.ForgeToken = strings.TrimSpace(request.APIKey), strings.TrimSpace(request.ForgeToken)
	if (request.APIKey == "") == (request.ForgeToken == "") {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "exactly one credential is required"})
	}
	expiresAt := time.Now().Add(adminSessionTTL)
	if request.APIKey != "" {
		if !strings.HasPrefix(request.APIKey, "wv_sk_") {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		}
		if s.KeyStore == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "api keys not configured"})
		}
		key, err := s.KeyStore.Validate(c.Request().Context(), request.APIKey)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		}
		if key.ExpiresAt != nil && key.ExpiresAt.Before(expiresAt) {
			expiresAt = *key.ExpiresAt
		}
		credential := request.APIKey
		// An administrator key that carries the admin scope can already issue
		// itself a key with every scope, so the console signs in as the key's
		// owner instead of being limited by the operator key's narrower scopes.
		// Any other key keeps its own scopes.
		if key.Role == "admin" && key.OwnerUserID != "" && slices.Contains(key.Scopes, "admin") {
			token, err := s.signJWTWithPermissionSets(key.TenantID, key.OwnerUserID, []string{"admin"}, adminAPIKeyIdentitySource, nil, time.Until(expiresAt))
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "could not issue console session"})
			}
			credential = token
		}
		s.setAdminSessionCookie(c, credential, expiresAt)
		name := key.Name
		if credential != request.APIKey && s.UserStore != nil {
			if owner, err := s.UserStore.GetByID(c.Request().Context(), key.TenantID, key.OwnerUserID); err == nil {
				name = owner.DisplayName
				if name == "" {
					name = owner.Username
				}
			}
		}
		return c.JSON(http.StatusOK, adminSessionView{Name: name, Role: key.Role, Source: "api_key", ExpiresAt: expiresAt})
	}
	exchanged, failure := s.exchangeExternalIdentity(c.Request().Context(), request.ForgeToken)
	if failure != nil {
		return c.JSON(failure.Status, map[string]string{"error": failure.Message})
	}
	s.setAdminSessionCookie(c, exchanged.Token, expiresAt)
	name := exchanged.User.DisplayName
	if name == "" {
		name = exchanged.Identity.Email
	}
	return c.JSON(http.StatusOK, adminSessionView{Name: name, Role: exchanged.AccessRole, Source: "forge", ExpiresAt: expiresAt})
}

func (s *Server) handleDeleteAdminSession(c echo.Context) error {
	c.SetCookie(&http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: adminRequestSecure(c.Request()), SameSite: http.SameSiteStrictMode,
	})
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) setAdminSessionCookie(c echo.Context, credential string, expiresAt time.Time) {
	c.SetCookie(&http.Cookie{
		Name: adminSessionCookie, Value: credential, Path: "/", Expires: expiresAt,
		MaxAge:   max(1, int(time.Until(expiresAt).Seconds())),
		HttpOnly: true, Secure: adminRequestSecure(c.Request()), SameSite: http.SameSiteStrictMode,
	})
}

// handleGetAdminSession reports who the current console session belongs to.
// It is registered behind the ordinary authentication middleware.
func (s *Server) handleGetAdminSession(c echo.Context) error {
	view := adminSessionView{Role: firstRole(c), Source: "forge"}
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		view.Source = "api_key"
		view.Name, _ = c.Get(apiKeyNameContextKey).(string)
	} else if source, _ := c.Get(identitySourceContextKey).(string); source == adminAPIKeyIdentitySource {
		view.Source = "api_key"
	} else if source != "forge" {
		view.Source = "session"
	}
	if view.Name == "" && s.UserStore != nil {
		if user, err := s.UserStore.GetByID(c.Request().Context(), getTenant(c), getUserID(c)); err == nil {
			view.Name = user.DisplayName
			if view.Name == "" {
				view.Name = user.Username
			}
		}
	}
	if cookie, err := c.Request().Cookie(adminSessionCookie); err == nil && cookie.Expires.After(time.Now()) {
		view.ExpiresAt = cookie.Expires
	}
	return c.JSON(http.StatusOK, view)
}
