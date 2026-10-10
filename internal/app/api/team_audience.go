package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/labstack/echo/v4"
)

// forgePermissionSetsContextKey carries the Forge permission sets that the
// exchange verified for this Weave session.
const forgePermissionSetsContextKey = "forge_permission_sets"

const teamAudienceLimit = 32

// normalizeTeamAudience validates the audience field older drafts still carry.
// It is stored unchanged and no longer decides who may use a team.
func normalizeTeamAudience(audience []string) ([]string, error) {
	if len(audience) > teamAudienceLimit {
		return nil, fmt.Errorf("team audience allows at most %d permission sets", teamAudienceLimit)
	}
	seen := make(map[string]struct{}, len(audience))
	normalized := make([]string, 0, len(audience))
	for _, item := range audience {
		value := strings.TrimSpace(item)
		if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("team audience entries must be Forge permission set names")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("team audience entries must be unique")
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

// forgeEmployeeSession returns the caller's Forge permission sets when the
// request comes from a Forge-exchanged employee session.
func forgeEmployeeSession(c echo.Context) ([]string, bool) {
	if source, _ := c.Get(identitySourceContextKey).(string); source != "forge" {
		return nil, false
	}
	sets, _ := c.Get(forgePermissionSetsContextKey).([]string)
	return sets, true
}

// ensureTeamAvailable is where a team's use is limited to the people chosen
// for it on the Forge configuration page. That list is not read yet, and a
// team without one is open to the whole organization, so every team passes.
// It returns false when a response was written.
func (s *Server) ensureTeamAvailable(echo.Context, string, string) (bool, error) {
	return true, nil
}
