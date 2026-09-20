package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestProductCapabilitiesUseServerRoleAndScopeDecisions(t *testing.T) {
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
		{name: "developer", authSource: authSourceJWT, roles: []string{"developer"}, wantTeam: "allow", wantRun: "allow", wantSimulate: "allow", wantSandbox: "deny", wantPublish: "allow"},
		{name: "scoped api key", authSource: authSourceAPIKey, roles: []string{"developer"}, scopes: []string{"org", "runs", "capabilities:manage"}, wantTeam: "allow", wantRun: "allow", wantSimulate: "allow", wantSandbox: "deny", wantPublish: "allow"},
		{name: "unscoped api key", authSource: authSourceAPIKey, roles: []string{"developer"}, wantTeam: "deny", wantRun: "deny", wantSimulate: "deny", wantSandbox: "deny", wantPublish: "deny"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			recorder := httptest.NewRecorder()
			context := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/authorization/capabilities", nil), recorder)
			context.Set("tenant", "org-1")
			context.Set("user_id", "user-1")
			context.Set("roles", test.roles)
			context.Set(scopesContextKey, test.scopes)
			context.Set(authSourceContextKey, test.authSource)

			if err := (&Server{}).handleProductCapabilities(context); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Status       string                      `json:"status"`
				Subject      map[string]string           `json:"subject"`
				Capabilities []productCapabilityDecision `json:"capabilities"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "ready" || response.Subject["id"] != "user-1" || response.Subject["organizationId"] != "org-1" {
				t.Fatalf("unexpected response identity: %+v", response)
			}
			if len(response.Capabilities) != 5 {
				t.Fatalf("capability count = %d", len(response.Capabilities))
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
