package api

import (
	"net/http"
	"os"

	"github.com/labstack/echo/v4"
)

var buildCommit = "unknown"

// SetBuildCommit sets the source revision reported by the health endpoint.
func SetBuildCommit(commit string) {
	if commit == "" {
		buildCommit = "unknown"
		return
	}
	buildCommit = commit
}

func (s *Server) handleHealth(c echo.Context) error {
	response := map[string]string{
		"status":       "ok",
		"version":      "1.0.0",
		"build_commit": buildCommit,
	}
	if buildCommit == "unknown" && os.Getenv("WEAVE_DEV_MODE") == "true" {
		response["build_commit_warning"] = "build commit unknown; rebuild via scripts/refresh-weave.sh"
	}
	return c.JSON(http.StatusOK, response)
}
