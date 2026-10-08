package api

import (
	"sort"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func registeredRoutes(t *testing.T, cfg *config.Config) map[string]bool {
	t.Helper()
	server := &Server{Echo: echo.New(), Config: cfg}
	server.registerRoutes()
	routes := map[string]bool{}
	for _, route := range server.Echo.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	return routes
}

// supportedClientRoutes are the Weave endpoints the GooeyPi desktop with Forge
// calls: identity exchange, team authoring and trials, workflow publication,
// handoff registration and dispatch, and run and result reads. Retiring legacy
// platform APIs must never remove any of them.
var supportedClientRoutes = []string{
	"POST /v1/auth/external/exchange",
	"GET /v1/health",
	"GET /v1/agents", "POST /v1/agents", "GET /v1/agents/:name", "PUT /v1/agents/:name",
	"GET /v1/teams", "POST /v1/teams", "GET /v1/teams/:id", "PUT /v1/teams/:id/profile",
	"POST /v1/teams/:id/workers", "DELETE /v1/teams/:id/workers/:worker",
	"GET /v1/teams/:id/members/:agent/config-draft",
	"PUT /v1/teams/:id/members/:agent/config-draft",
	"POST /v1/teams/:id/members/:agent/config-draft/apply",
	"GET /v1/teams/:id/development", "PUT /v1/teams/:id/development",
	"POST /v1/teams/:id/development/trials", "POST /v1/teams/:id/development/publish",
	"GET /v1/teams/:id/development/trials/:request/input", "DELETE /v1/teams/:id/development/team",
	"GET /v1/development/model-catalog",
	"POST /v1/teams/:id/workflows", "GET /v1/teams/:id/workflows",
	"POST /v1/workflows/:id/drafts", "PUT /v1/workflows/:id/versions/:version",
	"POST /v1/workflows/:id/versions/:version/validate", "POST /v1/workflows/:id/versions/:version/publish",
	"POST /v1/workbench/dispatch-inputs", "POST /v1/teams/:id/dispatch",
	"GET /v1/runs", "GET /v1/runs/:id", "GET /v1/runs/:id/activity", "GET /v1/runs/:id/workbench-context",
	"GET /v1/deliverables/:id",
	"GET /v1/human-tasks", "POST /v1/human-tasks/:run_id/complete",
	"GET /v1/runtimes",
}

var retiredRoutes = []string{
	"POST /v1/auth/login", "POST /v1/auth/register",
	"POST /v1/teams:from-template", "POST /v1/teams/:id/evaluations", "GET /v1/team-templates/samples",
	"POST /v1/internal/team-build-runs", "GET /v1/internal/team-build-runs",
	"PUT /v1/team-build-runs/:id/blueprint", "PUT /v1/internal/team-build-runs/:id/drafts",
	"POST /v1/internal/team-build-runs/:id/authorize", "POST /v1/internal/team-build-runs/:id/submit",
	"POST /v1/internal/team-build-runs/:id/execute", "POST /v1/internal/team-build-runs/:id/cancel",
	"POST /v1/internal/team-build-runs/:id/rollback", "GET /v1/internal/team-build-runs/:id/progress",
	"GET /v1/internal/team-build-runs/:id/rounds", "GET /v1/internal/team-build-runs/:id/rounds/:n/report",
	"GET /v1/internal/team-build-runs/:id/usage", "GET /v1/internal/team-build-runs/:id",
	"POST /v1/internal/team-build-runs/candidate-runs", "POST /v1/internal/team-build-runs/publish",
}

// W10 removed Weave's local accounts (identity comes from the external
// exchange and admin API keys) and the unused bounded-decision interface.
var w10DeletedRoutes = []string{
	"POST /v1/auth/token", "POST /v1/auth/refresh", "GET /v1/auth/me", "PUT /v1/auth/me", "PUT /v1/auth/me/password",
	"GET /v1/users", "GET /v1/users/:id", "PUT /v1/users/:id", "DELETE /v1/users/:id",
	"GET /v1/workspace", "GET /v1/workspace/members", "POST /v1/workspace/members", "DELETE /v1/workspace/members/:userID",
	"PUT /v1/decision-bindings/:key_id", "POST /v1/decisions", "GET /v1/decisions/:decision_id", "POST /v1/decisions:cancel",
}

// Delivery targets are closed first and deleted later; see W10 in
// weave-workbench docs/plans/Weave与Loom整改方案.md for the deletion trigger.
var closedDeliveryTargetRoutes = []string{
	"GET /v1/delivery-targets", "POST /v1/delivery-targets", "GET /v1/delivery-targets/:id", "PUT /v1/delivery-targets/:id",
	"GET /v1/delivery-targets/:id/revisions/:revision", "POST /v1/delivery-targets/:id/rotate-headers",
	"POST /v1/delivery-targets/:id/disable", "POST /v1/delivery-targets/:id/revoke", "DELETE /v1/delivery-targets/:id",
}

func TestRetiredRoutesAreNeverRegistered(t *testing.T) {
	routes := registeredRoutes(t, &config.Config{JWTSecret: "test"})
	var stillRegistered []string
	for _, group := range [][]string{retiredRoutes, w10DeletedRoutes, closedDeliveryTargetRoutes} {
		for _, route := range group {
			if routes[route] {
				stillRegistered = append(stillRegistered, route)
			}
		}
	}
	sort.Strings(stillRegistered)
	if len(stillRegistered) != 0 {
		t.Fatalf("retired routes still registered: %v", stillRegistered)
	}
}

func TestSupportedClientRoutesSurviveRetirement(t *testing.T) {
	routes := registeredRoutes(t, &config.Config{JWTSecret: "test"})
	var missing []string
	for _, route := range supportedClientRoutes {
		if !routes[route] {
			missing = append(missing, route)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("supported client routes missing after retirement: %v", missing)
	}
}
