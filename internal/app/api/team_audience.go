package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// forgePermissionSetsContextKey carries the Forge permission sets that the
// exchange verified for this Weave session.
const forgePermissionSetsContextKey = "forge_permission_sets"

const teamAudienceLimit = 32

// normalizeTeamAudience validates a team's declared Forge permission sets.
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

// teamAvailableTo reports whether a session holding permissionSets may use a
// team limited to audience. An empty audience is open to the organization.
func teamAvailableTo(audience, permissionSets []string) bool {
	if len(audience) == 0 {
		return true
	}
	for _, wanted := range audience {
		for _, held := range permissionSets {
			if wanted == held {
				return true
			}
		}
	}
	return false
}

// forgeEmployeeSession returns the caller's Forge permission sets when the
// request comes from a Forge-exchanged employee session. Operator keys and
// other sources are not employees and are not limited by team audience.
func forgeEmployeeSession(c echo.Context) ([]string, bool) {
	if source, _ := c.Get(identitySourceContextKey).(string); source != "forge" {
		return nil, false
	}
	sets, _ := c.Get(forgePermissionSetsContextKey).([]string)
	return sets, true
}

func (s *Server) teamAudiences(ctx context.Context, workspaceID string) (map[string][]string, error) {
	rows, err := s.GetPool().Query(ctx, `SELECT id,audience FROM weave_teams WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var audience []string
		if err := json.Unmarshal(raw, &audience); err != nil {
			return nil, fmt.Errorf("team %s audience is invalid: %w", id, err)
		}
		result[id] = audience
	}
	return result, rows.Err()
}

// ensureTeamAvailable answers 403 team_not_available when a Forge employee
// may not use the team. It returns false when a response was written.
func (s *Server) ensureTeamAvailable(c echo.Context, workspaceID, teamID string) (bool, error) {
	permissionSets, employee := forgeEmployeeSession(c)
	if !employee || s.GetPool() == nil {
		return true, nil
	}
	var raw []byte
	err := s.GetPool().QueryRow(c.Request().Context(), `SELECT audience FROM weave_teams WHERE workspace_id=$1 AND id=$2`, workspaceID, teamID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown teams keep their existing not-found handling downstream.
		return true, nil
	}
	if err != nil {
		return false, workflowStoreFailure(c, fmt.Errorf("read team audience: %w", err))
	}
	var audience []string
	if err := json.Unmarshal(raw, &audience); err != nil {
		return false, workflowStoreFailure(c, fmt.Errorf("decode team audience: %w", err))
	}
	if !teamAvailableTo(audience, permissionSets) {
		return false, workflowError(c, http.StatusForbidden, "team_not_available", "this team is not available to the current account")
	}
	return true, nil
}
