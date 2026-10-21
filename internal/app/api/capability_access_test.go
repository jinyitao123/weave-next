package api

import (
	"github.com/labstack/echo/v4"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// API keys are admin operator credentials; people need at least developer.
func TestCapabilityAccessRequiresDeveloperOrAdminKey(t *testing.T) {
	for _, test := range []struct {
		name, source, action string
		roles                []string
		status               int
	}{
		{"admin key", authSourceAPIKey, "manage", []string{"admin"}, 204},
		{"developer", authSourceJWT, "manage", []string{"developer"}, 204},
		{"member", authSourceJWT, "invoke", []string{"member"}, 403},
		{"unknown role", authSourceJWT, "invoke", []string{"guest"}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			response := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest("POST", "/", nil), response)
			c.Set(authSourceContextKey, test.source)
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
		`{"request_id":"x","input":{},"application_id":"caller-selected"}`,
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
