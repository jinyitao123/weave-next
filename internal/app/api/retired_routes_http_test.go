package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func routeHTTPServer(cfg *config.Config) *Server {
	server := &Server{Echo: echo.New(), Config: cfg}
	server.registerRoutes()
	return server
}

func requestRegisteredRoute(server *Server, method, path, authorization, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if authorization != "" {
		request.Header.Set(echo.HeaderAuthorization, authorization)
	}
	recorder := httptest.NewRecorder()
	server.Echo.ServeHTTP(recorder, request)
	return recorder
}

func concreteRoutePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[index] = "fixture"
		}
	}
	return strings.Join(segments, "/")
}

func TestRetiredRoutesReturn404ThroughRealHTTPWithDisabledDefaults(t *testing.T) {
	// Use loaded defaults, rather than a zero-valued Config whose legacy flags
	// intentionally mean enabled for backwards-compatible explicit callers.
	t.Setenv("WEAVE_RETIRE_LEGACY_PLATFORM_APIS", "")
	t.Setenv("WEAVE_DISABLE_LOCAL_LOGIN", "")
	t.Setenv("DATABASE_URL", "postgres://fixture.invalid/unused")
	t.Setenv("JWT_SECRET", "retired-http-fixture")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RetireLegacyPlatformAPIs || !cfg.DisableLocalLogin {
		t.Fatal("deployment defaults did not disable retired APIs")
	}
	cfg.JWTSecret = "retired-http-fixture"
	server := routeHTTPServer(cfg)
	if len(retiredRoutes) != 21 {
		t.Fatalf("expected all 21 closed cutover routes, got %d", len(retiredRoutes))
	}
	for _, route := range retiredRoutes {
		t.Run(route, func(t *testing.T) {
			method, path, _ := strings.Cut(route, " ")
			for _, authorization := range []string{"", "Bearer invalid"} {
				response := requestRegisteredRoute(server, method, concreteRoutePath(path), authorization, `{}`)
				if response.Code != http.StatusNotFound {
					t.Fatalf("closed route %s returned %d: %s", route, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestSupportedPrivateRoutesStillRequireAuthenticationThroughRealHTTP(t *testing.T) {
	server := routeHTTPServer(&config.Config{JWTSecret: "private-http-fixture", RetireLegacyPlatformAPIs: true, DisableLocalLogin: true})
	for _, route := range supportedClientRoutes {
		method, path, _ := strings.Cut(route, " ")
		if path == "/v1/health" || path == "/v1/auth/external/exchange" {
			continue
		}
		t.Run(route, func(t *testing.T) {
			for _, authorization := range []string{"", "Bearer invalid"} {
				response := requestRegisteredRoute(server, method, concreteRoutePath(path), authorization, `{}`)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("private route %s lost authentication: %d %s", route, response.Code, response.Body.String())
				}
			}
		})
	}
	for _, route := range []string{"GET /v1/auth/me", "POST /v1/auth/refresh", "GET /v1/capability-apps", "POST /v1/capabilities/drafts", "POST /v1/runtime/hello", "POST /v1/runtime/claim", "POST /v1/runtime/tasks/fixture/llm/chat"} {
		method, path, _ := strings.Cut(route, " ")
		response := requestRegisteredRoute(server, method, path, "", `{}`)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("private authentication group %s returned %d: %s", route, response.Code, response.Body.String())
		}
	}
}

func TestEnabledRetiredPrivateRoutesStillRequireAuthenticationThroughRealHTTP(t *testing.T) {
	server := routeHTTPServer(&config.Config{JWTSecret: "legacy-http-fixture"})
	for _, route := range retiredRoutes {
		method, path, _ := strings.Cut(route, " ")
		if path == "/v1/auth/login" || path == "/v1/auth/register" {
			continue
		}
		t.Run(route, func(t *testing.T) {
			response := requestRegisteredRoute(server, method, concreteRoutePath(path), "", `{}`)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("enabled legacy route %s lost authentication: %d %s", route, response.Code, response.Body.String())
			}
		})
	}
	for _, path := range []string{"/v1/auth/login", "/v1/auth/register"} {
		response := requestRegisteredRoute(server, http.MethodPost, path, "", `{}`)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "username and password are required") {
			t.Errorf("enabled local auth handler %s no longer has its original public validation: %d %s", path, response.Code, response.Body.String())
		}
	}
}

func TestUnknownAndWrongMethodRequestsKeepRouterSemanticsThroughRealHTTP(t *testing.T) {
	server := routeHTTPServer(&config.Config{JWTSecret: "routing-http-fixture", RetireLegacyPlatformAPIs: true, DisableLocalLogin: true})
	for _, path := range []string{"/unknown", "/v1", "/v1/unknown", "/v1/runtime", "/v1/runtime/unknown", "/v1/teams/fixture/unknown"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			response := requestRegisteredRoute(server, method, path, "", `{}`)
			if response.Code != http.StatusNotFound {
				t.Errorf("missing route %s %s returned %d: %s", method, path, response.Code, response.Body.String())
			}
		}
	}
	// Echo's group fallback retains 404 for unsupported methods inside the
	// group; without a group fallback its method match supplies 405 and Allow.
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPatch, "/v1/teams", http.StatusNotFound},
		{http.MethodGet, "/v1/auth/external/exchange", http.StatusNotFound},
		{http.MethodGet, "/v1/runtime/hello", http.StatusNotFound},
		{http.MethodPost, "/install.sh", http.StatusMethodNotAllowed},
	} {
		response := requestRegisteredRoute(server, request.method, request.path, "", `{}`)
		if response.Code != request.status {
			t.Errorf("unsupported method %s %s returned %d, want %d: %s", request.method, request.path, response.Code, request.status, response.Body.String())
		}
		if request.status == http.StatusMethodNotAllowed && !strings.Contains(response.Header().Get(echo.HeaderAllow), http.MethodGet) {
			t.Errorf("405 lost Allow header: %v", response.Header())
		}
	}
}

func TestAuthenticatedRouteGroupDoesNotBypassActualWildcardHandler(t *testing.T) {
	server := routeHTTPServer(&config.Config{JWTSecret: "wildcard-http-fixture", RetireLegacyPlatformAPIs: true, DisableLocalLogin: true})
	// There is no actual wildcard HTTP method handler in the product registry
	// today. Register one at the exact same path as the generated 404 fallback
	// to verify that its method-specific middleware can never be bypassed.
	group := authenticatedRouteGroup(server.Echo, "/private-wildcard", AuthMiddleware(server.Config.JWTSecret, nil, nil))
	handlerCalls := 0
	group.GET("/*", func(c echo.Context) error { handlerCalls++; return c.NoContent(http.StatusNoContent) })
	for _, authorization := range []string{"", "Bearer invalid"} {
		response := requestRegisteredRoute(server, http.MethodGet, "/private-wildcard/nested/file", authorization, "")
		if response.Code != http.StatusUnauthorized || handlerCalls != 0 {
			t.Fatalf("actual wildcard bypassed authentication: code=%d handler calls=%d", response.Code, handlerCalls)
		}
	}
	response := requestRegisteredRoute(server, http.MethodPost, "/private-wildcard/nested/file", "", "")
	if response.Code != http.StatusNotFound || handlerCalls != 0 {
		t.Fatalf("wildcard fallback did not remain a missing method: code=%d calls=%d", response.Code, handlerCalls)
	}
}

func TestEnabledLocalAuthRetainsRegistrationAndLoginSemanticsRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	server := routeHTTPServer(&config.Config{JWTSecret: "local-auth-http-fixture"})
	server.UserStore = users.NewStore(pool)
	body := `{"username":"fixture-operator","password":"fixture-password","display_name":"Fixture Operator"}`
	registered := requestRegisteredRoute(server, http.MethodPost, "/v1/auth/register", "", body)
	if registered.Code != http.StatusCreated {
		t.Fatalf("enabled first-user registration failed: %d %s", registered.Code, registered.Body.String())
	}
	var user users.User
	if err := json.Unmarshal(registered.Body.Bytes(), &user); err != nil || user.Role != "admin" {
		t.Fatalf("first-user role changed: role=%q err=%v", user.Role, err)
	}
	login := requestRegisteredRoute(server, http.MethodPost, "/v1/auth/login", "", body)
	if login.Code != http.StatusOK {
		t.Fatalf("enabled login failed: %d %s", login.Code, login.Body.String())
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil || session.Token == "" {
		t.Fatal("enabled login did not issue its original session token")
	}
	me := requestRegisteredRoute(server, http.MethodGet, "/v1/auth/me", "Bearer "+session.Token, "")
	if me.Code != http.StatusOK {
		t.Fatalf("registered local account could not access private route: %d", me.Code)
	}
	invalid := requestRegisteredRoute(server, http.MethodPost, "/v1/auth/login", "", `{"username":"fixture-operator","password":"wrong-fixture-password"}`)
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid local credentials returned %d", invalid.Code)
	}
	second := requestRegisteredRoute(server, http.MethodPost, "/v1/auth/register", "", `{"username":"second-fixture-user","password":"fixture-password"}`)
	if second.Code != http.StatusForbidden {
		t.Fatalf("anonymous registration after bootstrap returned %d", second.Code)
	}
}
