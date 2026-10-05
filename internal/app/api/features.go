package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// platformInternalAgentPrefix marks agents that belong to the platform rather
// than to a workspace. The retired meta-team and graph designer live under it.
// Their records stay in existing databases, but no client may run, read,
// change or delete them: they only work with tools that no longer exist.
const platformInternalAgentPrefix = "__"

var errPlatformInternalAgent = errors.New("platform_internal_agent")

func isPlatformInternalAgent(name string) bool {
	return strings.HasPrefix(name, platformInternalAgentPrefix)
}

func platformInternalAgentResponse(c echo.Context) error {
	return c.JSON(http.StatusForbidden, map[string]string{
		"code": "platform_internal_agent", "error": "platform_internal_agent",
	})
}
