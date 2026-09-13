package api

import (
	"github.com/labstack/echo/v4"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapabilityAccessRequiresExplicitOperationScope(t *testing.T) {
	for _, test := range []struct {
		name, source, action string
		scopes, roles        []string
		status               int
	}{
		{"empty key", authSourceAPIKey, "invoke", nil, []string{"admin"}, 403},
		{"Workbench admin key", authSourceAPIKey, "invoke", []string{"admin"}, []string{"admin"}, 204},
		{"invoke only", authSourceAPIKey, "manage", []string{"capabilities:invoke"}, []string{"admin"}, 403},
		{"read only", authSourceAPIKey, "cancel", []string{"capabilities:read"}, nil, 403},
		{"allowed", authSourceAPIKey, "invoke", []string{"capabilities:invoke"}, nil, 204},
		{"developer", authSourceJWT, "manage", nil, []string{"developer"}, 204},
		{"unknown role", authSourceJWT, "invoke", nil, []string{"guest"}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			response := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest("POST", "/", nil), response)
			c.Set(authSourceContextKey, test.source)
			c.Set(scopesContextKey, test.scopes)
			c.Set("roles", test.roles)
			if err := requireCapabilityAccess(test.action)(func(c echo.Context) error { return c.NoContent(204) })(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
}

func TestCapabilityBodyRejectsDuplicateAndCallerSuppliedIdentity(t *testing.T) {
	for _, body := range []string{
		`{"request_id":"x","input":{},"invocation_id":"caller-selected"}`,
		`{"request_id":"x","request_id":"y","input":{}}`,
		`{"request_id":"x","input":{}}{}`,
	} {
		e := echo.New()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), httptest.NewRecorder())
		var req invokeCapabilityRequest
		if err := decodeCapabilityBody(c, &req); err == nil {
			t.Fatalf("body accepted: %s", body)
		}
	}
}
