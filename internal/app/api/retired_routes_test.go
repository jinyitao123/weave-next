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

// Local login and registration are the only retired routes still gated by
// configuration; every other retired route has been deleted from the code.
var localAuthRoutes = map[string]bool{"POST /v1/auth/login": true, "POST /v1/auth/register": true}

func TestRetiredRoutesAreNeverRegistered(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"default":            {JWTSecret: "test"},
		"local login closed": {JWTSecret: "test", DisableLocalLogin: true},
	} {
		routes := registeredRoutes(t, cfg)
		var stillRegistered []string
		for _, route := range retiredRoutes {
			if routes[route] && !(localAuthRoutes[route] && !cfg.DisableLocalLogin) {
				stillRegistered = append(stillRegistered, route)
			}
		}
		sort.Strings(stillRegistered)
		if len(stillRegistered) != 0 {
			t.Fatalf("%s: retired routes still registered: %v", name, stillRegistered)
		}
	}
}

func TestSupportedClientRoutesSurviveRetirement(t *testing.T) {
	routes := registeredRoutes(t, &config.Config{JWTSecret: "test", DisableLocalLogin: true})
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

func TestLocalLoginRoutesFollowTheDisableFlag(t *testing.T) {
	open := registeredRoutes(t, &config.Config{JWTSecret: "test"})
	closed := registeredRoutes(t, &config.Config{JWTSecret: "test", DisableLocalLogin: true})
	for route := range localAuthRoutes {
		if !open[route] {
			t.Errorf("%s missing while local login is enabled", route)
		}
		if closed[route] {
			t.Errorf("%s registered while local login is disabled", route)
		}
	}
}
