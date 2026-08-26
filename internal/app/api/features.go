package api

import (
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/labstack/echo/v4"
)

var errMetaTeamDisabled = errors.New("metateam_disabled")

func (s *Server) metaTeamEnabled() bool {
	// Nil configuration is used by focused handler tests and predates feature
	// flags; preserve the historical enabled behavior there.
	return s == nil || s.Config == nil || s.Config.MetaTeamEnabled
}

func (s *Server) metaTeamRunDisabled(agentName, intent string) bool {
	return !s.metaTeamEnabled() && (metateam.IsAgentName(agentName) ||
		intent == conversation.IntentCreateTeam || intent == "optimize_team")
}

func (s *Server) handleGetFeatures(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"metateam": map[string]bool{"enabled": s.metaTeamEnabled()},
	})
}

func metaTeamDisabledResponse(c echo.Context) error {
	return c.JSON(http.StatusForbidden, map[string]string{
		"code": "metateam_disabled", "error": "metateam_disabled",
	})
}
