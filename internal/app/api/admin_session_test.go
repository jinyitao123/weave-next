package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func adminConsoleServer(cfg *config.Config) *Server {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.JWTSecret = "admin-console-test"
	s := &Server{Echo: echo.New(), Config: cfg}
	s.registerRoutes()
	return s
}

func adminRequest(method, target, body string, mutate func(*http.Request)) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = "weave.example"
	if body != "" {
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if mutate != nil {
		mutate(request)
	}
	return request
}

func serveAdmin(s *Server, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	s.Echo.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminConsoleIsServedUnderItsOwnPrefix(t *testing.T) {
	s := adminConsoleServer(nil)
	recorder := serveAdmin(s, adminRequest(http.MethodGet, "/admin/", "", nil))
	// A source checkout without a console build answers 503; a built binary 200.
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /admin/ = %d", recorder.Code)
	}
	if recorder.Header().Get("Content-Security-Policy") == "" && recorder.Code == http.StatusOK {
		t.Fatal("console response has no content security policy")
	}
}

func TestAdminConfigListsOnlyUsableSignInMethods(t *testing.T) {
	read := func(s *Server) map[string]any {
		recorder := serveAdmin(s, adminRequest(http.MethodGet, "/v1/admin/config", "", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("config = %d", recorder.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	plain := read(adminConsoleServer(nil))
	if methods := plain["sign_in_methods"].([]any); len(methods) != 1 || methods[0] != "api_key" || plain["forge_origin"] != nil {
		t.Fatalf("config without Forge = %v", plain)
	}
	withForge := adminConsoleServer(&config.Config{AdminForgeURL: "https://forge.example"})
	withForge.ExternalIdentity = externalIdentityVerifierFunc(func(context.Context, string) (ExternalIdentity, error) {
		return ExternalIdentity{}, errors.New("unused")
	})
	body := read(withForge)
	if methods := body["sign_in_methods"].([]any); len(methods) != 2 || methods[0] != "forge" || body["forge_origin"] != "https://forge.example" {
		t.Fatalf("config with Forge = %v", body)
	}
	unsafeOrigin := read(adminConsoleServer(&config.Config{AdminForgeURL: "https://forge.example/path?x=1"}))
	if unsafeOrigin["forge_origin"] != nil {
		t.Fatalf("non-origin Forge URL accepted: %v", unsafeOrigin)
	}
}

func TestAdminCookieAuthenticatesExistingRoutes(t *testing.T) {
	s := adminConsoleServer(nil)
	token, err := s.signJWT("workspace-1", "user-1", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
	}
	read := serveAdmin(s, adminRequest(http.MethodGet, "/v1/admin/session", "", withCookie))
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"role":"admin"`) {
		t.Fatalf("session read = %d %s", read.Code, read.Body.String())
	}
	if anonymous := serveAdmin(s, adminRequest(http.MethodGet, "/v1/admin/session", "", nil)); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous session read = %d", anonymous.Code)
	}
}

func TestAdminCookieRejectsCrossSiteWrites(t *testing.T) {
	s := adminConsoleServer(nil)
	token, err := s.signJWT("workspace-1", "user-1", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: adminSessionCookie, Value: token}
	cases := map[string]func(*http.Request){
		"missing header": func(r *http.Request) { r.AddCookie(cookie) },
		"foreign origin": func(r *http.Request) {
			r.AddCookie(cookie)
			r.Header.Set(adminRequestHeader, "1")
			r.Header.Set("Origin", "https://attacker.example")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := serveAdmin(s, adminRequest(http.MethodPost, "/v1/teams", `{}`, mutate))
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "admin_request_rejected") {
				t.Fatalf("cross-site write = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	// An explicit Authorization header is never rewritten by the cookie path.
	bearer := serveAdmin(s, adminRequest(http.MethodGet, "/v1/admin/session", "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "not-a-token"})
		r.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}))
	if bearer.Code != http.StatusOK {
		t.Fatalf("explicit bearer = %d", bearer.Code)
	}
}

func TestAdminSignInExchangesForgeTokenIntoHttpOnlyCookie(t *testing.T) {
	s := adminConsoleServer(&config.Config{AdminForgeURL: "https://forge.example"})
	s.ExternalIdentity = externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
		if token != "forge-session" {
			return ExternalIdentity{}, errors.New("rejected")
		}
		return ExternalIdentity{Issuer: "forge:deployment", BaseURL: "https://forge.example", Subject: "forge-user", Email: "dev@example.com",
			Organization: "workspace-1", NativeOrganization: "org-1", AccessRole: "developer"}, nil
	})
	s.ExternalIdentityBinder = externalIdentityBinderFunc(func(context.Context, string, string, string, string, string) (*users.User, error) {
		return &users.User{ID: "user-1", TenantID: "workspace-1", DisplayName: "开发者小周"}, nil
	})
	trusted := func(r *http.Request) {
		r.Header.Set(adminRequestHeader, "1")
		r.Header.Set("Origin", "http://weave.example")
	}

	if refused := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"forge_token":"forge-session"}`, nil)); refused.Code != http.StatusForbidden {
		t.Fatalf("sign-in without console header = %d", refused.Code)
	}
	if invalid := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"forge_token":"other"}`, trusted)); invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid Forge token = %d", invalid.Code)
	}
	if both := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"forge_token":"a","api_key":"wv_sk_b"}`, trusted)); both.Code != http.StatusBadRequest {
		t.Fatalf("two credentials = %d", both.Code)
	}

	signedIn := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"forge_token":"forge-session"}`, trusted))
	if signedIn.Code != http.StatusOK || !strings.Contains(signedIn.Body.String(), `"name":"开发者小周"`) || !strings.Contains(signedIn.Body.String(), `"role":"developer"`) {
		t.Fatalf("sign-in = %d %s", signedIn.Code, signedIn.Body.String())
	}
	if strings.Contains(signedIn.Body.String(), "token") {
		t.Fatalf("session credential leaked into the response body: %s", signedIn.Body.String())
	}
	var session *http.Cookie
	for _, cookie := range signedIn.Result().Cookies() {
		if cookie.Name == adminSessionCookie {
			session = cookie
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || session.Path != "/" || session.MaxAge <= 0 {
		t.Fatalf("session cookie = %#v", session)
	}

	signedOut := serveAdmin(s, adminRequest(http.MethodDelete, "/v1/admin/session", "", func(r *http.Request) {
		trusted(r)
		r.AddCookie(session)
	}))
	if signedOut.Code != http.StatusNoContent {
		t.Fatalf("sign-out = %d", signedOut.Code)
	}
	cleared := signedOut.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != adminSessionCookie || cleared[0].MaxAge >= 0 {
		t.Fatalf("sign-out cookies = %#v", cleared)
	}
}

func TestAdminSignInRejectsMalformedAPIKeysWithoutAStore(t *testing.T) {
	s := adminConsoleServer(nil)
	trusted := func(r *http.Request) { r.Header.Set(adminRequestHeader, "1") }
	if malformed := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"api_key":"not-a-key"}`, trusted)); malformed.Code != http.StatusUnauthorized {
		t.Fatalf("malformed key = %d", malformed.Code)
	}
	if unconfigured := serveAdmin(s, adminRequest(http.MethodPost, "/v1/admin/session", `{"api_key":"wv_sk_example"}`, trusted)); unconfigured.Code != http.StatusServiceUnavailable {
		t.Fatalf("key without store = %d", unconfigured.Code)
	}
}
