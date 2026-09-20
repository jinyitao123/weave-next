package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

type productAuthorizerFunc func(context.Context, ProductAuthorizationRequest) (ProductAuthorizationResult, error)

func (f productAuthorizerFunc) Check(ctx context.Context, request ProductAuthorizationRequest) (ProductAuthorizationResult, error) {
	return f(ctx, request)
}

func allowAllProductAuthorizer(_ context.Context, request ProductAuthorizationRequest) (ProductAuthorizationResult, error) {
	allowed := make(map[string]bool, len(request.Actions))
	for _, action := range request.Actions {
		allowed[action] = true
	}
	return ProductAuthorizationResult{Allowed: allowed, PolicyVersion: "test"}, nil
}

func rolePolicyAuthorizer(_ context.Context, request ProductAuthorizationRequest) (ProductAuthorizationResult, error) {
	allowed := make(map[string]bool, len(request.Actions))
	employee := hasString(request.Principal.Roles, "employee")
	developer := hasString(request.Principal.Roles, "developer")
	for _, action := range request.Actions {
		switch action {
		case "team.read", "run.read":
			allowed[action] = employee
		case "debug.simulate", "release.publish":
			allowed[action] = developer
		case "debug.sandbox_write":
			allowed[action] = developer && request.Resource.Attributes["environment"] == "sandbox"
		}
	}
	return ProductAuthorizationResult{Allowed: allowed, PolicyVersion: "default"}, nil
}

func TestProductCapabilitiesProjectCerbosDecisions(t *testing.T) {
	tests := []struct {
		name         string
		authSource   string
		roles        []string
		scopes       []string
		wantTeam     string
		wantRun      string
		wantSimulate string
		wantSandbox  string
		wantPublish  string
	}{
		{name: "employee", authSource: authSourceJWT, roles: []string{"member"}, wantTeam: "allow", wantRun: "allow", wantSimulate: "deny", wantSandbox: "deny", wantPublish: "deny"},
		{name: "developer", authSource: authSourceJWT, roles: []string{"developer"}, wantTeam: "allow", wantRun: "allow", wantSimulate: "allow", wantSandbox: "allow", wantPublish: "allow"},
		{name: "scoped api key", authSource: authSourceAPIKey, roles: []string{"developer"}, scopes: []string{"org", "runs", "capabilities:manage"}, wantTeam: "allow", wantRun: "allow", wantSimulate: "allow", wantSandbox: "allow", wantPublish: "allow"},
		{name: "unscoped api key", authSource: authSourceAPIKey, roles: []string{"developer"}, wantTeam: "deny", wantRun: "deny", wantSimulate: "deny", wantSandbox: "deny", wantPublish: "deny"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			recorder := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/authorization/capabilities", nil), recorder)
			ctx.Set("tenant", "org-1")
			ctx.Set("user_id", "user-1")
			ctx.Set("roles", test.roles)
			ctx.Set(scopesContextKey, test.scopes)
			ctx.Set(authSourceContextKey, test.authSource)

			server := &Server{ProductAuthorizer: productAuthorizerFunc(rolePolicyAuthorizer)}
			if err := server.handleProductCapabilities(ctx); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Status       string                      `json:"status"`
				Subject      map[string]string           `json:"subject"`
				Source       map[string]string           `json:"source"`
				Capabilities []productCapabilityDecision `json:"capabilities"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "ready" || response.Source["kind"] != "cerbos" || response.Subject["id"] != "user-1" || response.Subject["organizationId"] != "org-1" {
				t.Fatalf("unexpected response: %+v", response)
			}
			want := []string{test.wantTeam, test.wantRun, test.wantSimulate, test.wantSandbox, test.wantPublish}
			for index, decision := range response.Capabilities {
				if decision.Decision != want[index] || decision.Reason == "" {
					t.Fatalf("capability[%d] = %+v, want decision %s with a reason", index, decision, want[index])
				}
			}
		})
	}
}

func TestProductCapabilityMiddlewareFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		result     ProductAuthorizationResult
		err        error
		wantStatus int
	}{
		{name: "allow", result: ProductAuthorizationResult{Allowed: map[string]bool{"release.publish": true}}, wantStatus: http.StatusNoContent},
		{name: "deny", result: ProductAuthorizationResult{Allowed: map[string]bool{"release.publish": false}}, wantStatus: http.StatusForbidden},
		{name: "unavailable", err: errors.New("pdp down"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			recorder := httptest.NewRecorder()
			ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/publish", nil), recorder)
			ctx.Set("tenant", "org-1")
			ctx.Set("user_id", "user-1")
			ctx.Set("roles", []string{"developer"})
			ctx.Set(authSourceContextKey, authSourceJWT)
			server := &Server{ProductAuthorizer: productAuthorizerFunc(func(context.Context, ProductAuthorizationRequest) (ProductAuthorizationResult, error) {
				return test.result, test.err
			})}
			handler := server.requireProductCapability("release.publish", nil)(func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
			if err := handler(ctx); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
