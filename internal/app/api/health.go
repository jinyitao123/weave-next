package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v4"
)

var buildCommit = "unknown"
var buildVersion = "development"

// SetBuildCommit sets the source revision reported by the health endpoint.
func SetBuildCommit(commit string) {
	if commit == "" {
		buildCommit = "unknown"
		return
	}
	buildCommit = commit
}

// SetBuildVersion sets the product version reported by the health endpoint.
func SetBuildVersion(version string) {
	if strings.TrimSpace(version) == "" {
		buildVersion = "development"
		return
	}
	buildVersion = version
}

func (s *Server) handleHealth(c echo.Context) error {
	response := map[string]string{
		"status":       "ok",
		"version":      buildVersion,
		"build_commit": buildCommit,
	}
	if buildCommit == "unknown" && os.Getenv("WEAVE_DEV_MODE") == "true" {
		response["build_commit_warning"] = "build commit unknown; rebuild via scripts/refresh-weave.sh"
	}
	return c.JSON(http.StatusOK, response)
}

// handleReady reports whether the dependencies required by the complete
// platform surface are initialized. Health remains a liveness signal; callers
// that intend to use teams, providers, or MCP-backed execution should use this
// endpoint before dispatching work.
func (s *Server) handleReady(c echo.Context) error {
	missing := make([]string, 0, 8)
	if s.Pool == nil || s.Pool.Ping(c.Request().Context()) != nil {
		missing = append(missing, "database")
	}
	checks := []struct {
		name      string
		available bool
	}{
		{name: "api_key_store", available: s.KeyStore != nil},
		{name: "organization_store", available: s.OrgStore != nil},
		{name: "workflow_store", available: s.Workflow != nil},
		{name: "credential_store", available: s.Credentials != nil},
		{name: "delivery_target_store", available: s.DeliveryTargets != nil},
		{name: "mcp_registry", available: s.MCPRegistry != nil},
	}
	for _, check := range checks {
		if !check.available {
			missing = append(missing, check.name)
		}
	}
	if len(missing) != 0 {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"status":  "not_ready",
			"missing": missing,
		})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}
