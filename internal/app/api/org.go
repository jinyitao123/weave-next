package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"

	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type AgentSummary struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	DisplayName        string   `json:"display_name"`
	Engine             string   `json:"engine"`
	RuntimeID          string   `json:"runtime_id"`
	Role               string   `json:"role"`
	Duty               string   `json:"duty"`
	ConfiguredDuty     string   `json:"configured_duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
	Enabled            bool     `json:"enabled"`
}

type TeamRoster struct {
	Team    org.Team       `json:"team"`
	Lead    *AgentSummary  `json:"lead"`
	Workers []AgentSummary `json:"workers"`
}

type teamRequest struct {
	Name string `json:"name"`
}

type teamProfileRequest struct {
	DisplayName       string    `json:"display_name"`
	Objective         string    `json:"objective"`
	ExpectedUpdatedAt time.Time `json:"expected_updated_at"`
}

type teamWorkerWriteRequest struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
}

type createTeamResponse struct {
	org.Team
	Workers []org.TeamWorker `json:"workers"`
}

type teamRosterRequest struct {
	IdempotencyKey    string                           `json:"idempotency_key"`
	ExpectedUpdatedAt time.Time                        `json:"expected_updated_at"`
	DesiredTeamStatus string                           `json:"desired_team_status"`
	LeadAgentID       string                           `json:"lead_agent_id"`
	Workers           []registry.TeamRosterWorkerInput `json:"workers"`
	Reason            string                           `json:"reason"`
}

type teamDispatchRulesRequest struct {
	Execution        string `json:"execution"`
	LegTimeoutSec    int    `json:"leg_timeout_sec"`
	GroupDeadlineSec int    `json:"group_deadline_sec"`
	Quorum           int    `json:"quorum"`
}

type teamDispatchRulesResponse struct {
	org.TeamDispatchRules
	Execution string `json:"execution"`
}

func (s *Server) handleListTeams(c echo.Context) error {
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	teams, err := s.OrgStore.ListTeams(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	include := parseTeamInclude(c.QueryParam("include"))
	switch status := c.QueryParam("status"); status {
	case "":
		teams = filterDefaultVisibleTeams(teams, include["platform"])
	case "active", "needs_repair", "building", "archived":
		teams = filterTeamsByStatus(teams, status)
		if !include["platform"] {
			teams = filterOutPlatformTeams(teams)
		}
	case "all":
		if !include["platform"] {
			teams = filterOutPlatformTeams(teams)
		}
	default:
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid status filter"})
	}
	// Decision 002: an employee sees only the teams their Forge permission
	// sets may use. Developers ask for the development view explicitly.
	if permissionSets, employee := forgeEmployeeSession(c); employee && s.GetPool() != nil {
		role := firstRole(c)
		if c.QueryParam("purpose") != "development" || (role != "developer" && role != "admin") {
			audiences, err := s.teamAudiences(ctx, workspaceID)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "team audience unavailable"})
			}
			available := teams[:0]
			for _, team := range teams {
				if teamAvailableTo(audiences[team.ID], permissionSets) {
					available = append(available, team)
				}
			}
			teams = available
		}
	}
	if !include["roster"] && !include["summary"] {
		return c.JSON(http.StatusOK, teams)
	}

	agents, err := s.Registry.List(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	workers, err := s.Registry.ListTeamWorkers(ctx, workspaceID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	rosters := aggregateTeamRosters(teams, agents, workers)
	if include["summary"] {
		return c.JSON(http.StatusOK, s.buildTeamRosterSummaryItems(ctx, workspaceID, rosters))
	}
	return c.JSON(http.StatusOK, rosters)
}

func parseTeamInclude(raw string) map[string]bool {
	result := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result[part] = true
		}
	}
	return result
}

func filterDefaultVisibleTeams(teams []org.Team, includePlatform bool) []org.Team {
	filtered := make([]org.Team, 0, len(teams))
	for _, team := range teams {
		if team.Status == "building" {
			continue
		}
		if !includePlatform && isPlatformTeam(team) {
			continue
		}
		filtered = append(filtered, team)
	}
	return filtered
}

func filterOutPlatformTeams(teams []org.Team) []org.Team {
	filtered := make([]org.Team, 0, len(teams))
	for _, team := range teams {
		if !isPlatformTeam(team) {
			filtered = append(filtered, team)
		}
	}
	return filtered
}

func isPlatformTeam(team org.Team) bool {
	return strings.HasPrefix(team.ID, "__") || strings.HasPrefix(team.Name, "__")
}

// filterTeamsByStatus keeps only teams with the exact status, preserving
// store order; result is always non-nil so the JSON shape stays `[]`.
func filterTeamsByStatus(teams []org.Team, status string) []org.Team {
	filtered := make([]org.Team, 0, len(teams))
	for _, team := range teams {
		if team.Status == status {
			filtered = append(filtered, team)
		}
	}
	return filtered
}

func aggregateTeamRosters(
	teams []org.Team,
	agents []registry.AgentRecord,
	teamWorkers []registry.TeamWorker,
) []TeamRoster {
	rosters := make([]TeamRoster, len(teams))
	teamIndexes := make(map[string]int, len(teams))
	for i, team := range teams {
		rosters[i] = TeamRoster{
			Team:    team,
			Workers: make([]AgentSummary, 0),
		}
		teamIndexes[team.ID] = i
	}
	agentsByID := make(map[string]registry.AgentRecord, len(agents))
	for _, agent := range agents {
		agentsByID[agent.ID] = agent
	}
	for teamIndex, team := range teams {
		agent, ok := agentsByID[team.LeadAvatarID]
		if !ok || agent.Role != "avatar" {
			continue
		}
		summary := AgentSummary{
			ID:          agent.ID,
			Name:        agent.Name,
			DisplayName: agent.DisplayName,
			Engine:      agent.Engine,
			RuntimeID:   agent.RuntimeID,
			Role:        agent.Role,
			Duty:        deriveDuty("", agent),
			// The registered lead has no separate worker enable toggle.
			Enabled: true,
		}
		rosters[teamIndex].Lead = &summary
	}
	for _, worker := range teamWorkers {
		teamIndex, ok := teamIndexes[worker.TeamID]
		if !ok {
			continue
		}
		agent, ok := agentsByID[worker.WorkerAgentID]
		if !ok || agent.Role != "worker" {
			continue
		}
		rosters[teamIndex].Workers = append(rosters[teamIndex].Workers, AgentSummary{
			ID:                 agent.ID,
			Name:               agent.Name,
			DisplayName:        agent.DisplayName,
			Engine:             agent.Engine,
			RuntimeID:          agent.RuntimeID,
			Role:               agent.Role,
			Duty:               deriveDuty(worker.Duty, agent),
			ConfiguredDuty:     worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       append([]string(nil), worker.AllowedKinds...),
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
			Enabled:            worker.Enabled,
		})
	}
	for i := range rosters {
		sort.Slice(rosters[i].Workers, func(a, b int) bool {
			return rosters[i].Workers[a].Name < rosters[i].Workers[b].Name
		})
	}

	return rosters
}

func deriveDuty(instruction string, rec registry.AgentRecord) string {
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		return instruction
	}

	core := strings.TrimSpace(rec.Spec.Identity.Core)
	if core == "" {
		return ""
	}
	runes := []rune(core)
	for i, r := range runes {
		switch r {
		case '\n', '\r':
			runes = runes[:i]
		case '.', '!', '?', '。', '！', '？':
			runes = runes[:i+1]
		default:
			continue
		}
		break
	}
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return strings.TrimSpace(string(runes))
}

func (s *Server) handleCreateTeam(c echo.Context) error {
	var req org.CreateActiveTeamInput
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "invalid_team_request", "error": "invalid team request body",
		})
	}
	if strings.TrimSpace(req.Name) != "" && strings.TrimSpace(req.LeadAvatarID) == "" && len(req.Workers) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "atomic_creation_wizard_pending", "error": "团队创建向导即将上线",
		})
	}
	result, err := s.OrgStore.CreateActiveTeam(c.Request().Context(), getTenant(c), req)
	if err != nil {
		switch {
		case errors.Is(err, org.ErrInvalidTeamCreationInput), errors.Is(err, org.ErrInvalidTeamWorkerKinds):
			return c.JSON(http.StatusBadRequest, map[string]string{
				"code": "invalid_team_request", "error": err.Error(),
			})
		case errors.Is(err, org.ErrTeamLeadUnavailable):
			return c.JSON(http.StatusConflict, map[string]string{
				"code": "team_lead_unavailable", "error": err.Error(),
			})
		case errors.Is(err, org.ErrTeamWorkerUnavailable):
			return c.JSON(http.StatusConflict, map[string]string{
				"code": "team_worker_unavailable", "error": err.Error(),
			})
		case errors.Is(err, org.ErrTeamLeadConflict):
			return c.JSON(http.StatusConflict, map[string]string{
				"code": "team_lead_conflict", "error": err.Error(),
			})
		case errors.Is(err, org.ErrTeamNameConflict):
			return c.JSON(http.StatusConflict, map[string]string{
				"code": "team_name_conflict", "error": err.Error(),
			})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"code": "team_create_failed", "error": "team creation failed",
			})
		}
	}
	return c.JSON(http.StatusCreated, createTeamResponse{Team: result.Team, Workers: result.Workers})
}

func (s *Server) handleRenameTeam(c echo.Context) error {
	var req teamRequest
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if err := s.OrgStore.RenameTeam(c.Request().Context(), getTenant(c), c.Param("id"), req.Name); err != nil {
		if errors.Is(err, org.ErrArchivedTeamImmutable) {
			return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleUpdateTeamProfile(c echo.Context) error {
	var req teamProfileRequest
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.DisplayName) == "" || strings.TrimSpace(req.Objective) == "" || req.ExpectedUpdatedAt.IsZero() {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_team_profile", "error": "invalid team profile"})
	}
	team, err := s.OrgStore.UpdateTeamProfile(c.Request().Context(), getTenant(c), c.Param("id"), org.UpdateTeamProfileInput{
		DisplayName: req.DisplayName, Objective: req.Objective, ExpectedUpdatedAt: req.ExpectedUpdatedAt,
	})
	if err != nil {
		switch {
		case errors.Is(err, org.ErrTeamNotFound):
			return c.JSON(http.StatusNotFound, map[string]string{"code": "team_not_found", "error": "team not found"})
		case errors.Is(err, org.ErrTeamWriteConflict):
			return c.JSON(http.StatusConflict, map[string]string{"code": "team_write_conflict", "error": "团队资料已被更新，请刷新后继续"})
		case errors.Is(err, org.ErrArchivedTeamImmutable):
			return c.JSON(http.StatusConflict, map[string]string{"code": "team_archived", "error": "已归档团队不能修改"})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{"code": "team_profile_update_failed", "error": "team profile update failed"})
		}
	}
	return c.JSON(http.StatusOK, team)
}

func (s *Server) handleCreateTeamWorker(c echo.Context) error {
	if s.TeamWorkers == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "team_worker_store_unavailable", "error": "team worker store unavailable"})
	}
	var req teamWorkerWriteRequest
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.WorkerAgentID) == "" || strings.TrimSpace(req.Duty) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_team_worker", "error": "invalid team worker"})
	}
	worker, err := s.TeamWorkers.Create(c.Request().Context(), getTenant(c), registry.TeamWorker{
		TeamID: c.Param("id"), WorkerAgentID: req.WorkerAgentID, Duty: req.Duty, WhenToUse: req.WhenToUse,
		ContextInstruction: req.ContextInstruction, AllowedKinds: req.AllowedKinds, DefaultKind: req.DefaultKind,
		ResultRequirement: req.ResultRequirement, Enabled: true,
	})
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"code": "team_worker_create_failed", "error": err.Error()})
	}
	return c.JSON(http.StatusCreated, worker)
}

func (s *Server) handleDeleteTeamWorker(c echo.Context) error {
	if s.TeamWorkers == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "team_worker_store_unavailable", "error": "team worker store unavailable"})
	}
	if err := s.TeamWorkers.Delete(c.Request().Context(), getTenant(c), c.Param("id"), c.Param("worker")); err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"code": "team_worker_remove_failed", "error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleGetTeamDispatchRules(c echo.Context) error {
	rules, err := s.OrgStore.GetTeamDispatchRules(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		if errors.Is(err, org.ErrTeamNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, teamDispatchRulesResponse{
		TeamDispatchRules: rules,
		Execution:         "parallel",
	})
}

func (s *Server) handlePutTeamDispatchRules(c echo.Context) error {
	var req teamDispatchRulesRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid dispatch rules request"})
	}
	if req.Execution == "serial" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "串行未支持"})
	}
	if req.Execution != "" && req.Execution != "parallel" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "execution must be parallel"})
	}
	if req.LegTimeoutSec < 0 || req.GroupDeadlineSec < 0 || req.Quorum < 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "dispatch rule values must be non-negative"})
	}

	rules := org.TeamDispatchRules{
		TeamID:           c.Param("id"),
		LegTimeoutSec:    req.LegTimeoutSec,
		GroupDeadlineSec: req.GroupDeadlineSec,
		Quorum:           req.Quorum,
	}
	if err := s.OrgStore.PutTeamDispatchRules(c.Request().Context(), getTenant(c), rules); err != nil {
		switch {
		case errors.Is(err, org.ErrTeamNotFound):
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, org.ErrArchivedTeamImmutable):
			return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, org.ErrInvalidTeamDispatchRules), errors.Is(err, org.ErrQuorumExceedsTeamSize):
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		default:
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}
	return c.JSON(http.StatusOK, teamDispatchRulesResponse{
		TeamDispatchRules: rules,
		Execution:         "parallel",
	})
}

func (s *Server) handleUpdateTeamRoster(c echo.Context) error {
	var req teamRosterRequest
	if err := decodeRevocationJSON(c, &req); err != nil {
		return writeRevocationError(
			c, http.StatusBadRequest, "invalid_revocation_request", "invalid roster request body", nil,
		)
	}
	key, keyErr := accessChangeKey(c)
	if keyErr != nil {
		return keyErr
	}
	if req.IdempotencyKey != "" && req.IdempotencyKey != key {
		return echo.NewHTTPError(http.StatusBadRequest, "这项变更的信息不一致，请重新操作。")
	}
	req.IdempotencyKey = key
	if err := validateTeamRosterRequest(req); err != nil {
		return writeRevocationError(
			c, http.StatusBadRequest, "invalid_revocation_request", err.Error(), nil,
		)
	}

	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	teamID := c.Param("id")
	if s.Registry == nil {
		return writeRevocationError(
			c, http.StatusServiceUnavailable, "revocation_store_unavailable", "revocation store is unavailable", nil,
		)
	}

	block, grant := rosterAccessResources(teamID, req)
	intent, err := newAccessChangeIntent(c, "team.roster", teamID, req, block, grant, []admissionfence.Resource{admissionfence.Team(teamID)})
	if err != nil {
		return err
	}
	result, err := s.applyAccessChange(ctx, intent, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		response, err := s.Registry.ApplyTeamRosterCommandTx(ctx, tx, registry.TeamRosterCommand{
			WorkspaceID:       workspaceID,
			TeamID:            teamID,
			IdempotencyKey:    req.IdempotencyKey,
			ExpectedUpdatedAt: req.ExpectedUpdatedAt,
			DesiredTeamStatus: req.DesiredTeamStatus,
			LeadAgentID:       req.LeadAgentID,
			Workers:           req.Workers,
			OperatorID:        getUserID(c),
			Reason:            req.Reason,
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(response)
	})
	return writeAccessChangeOutcome(c, result, err, http.StatusOK)
}

func validateTeamRosterRequest(req teamRosterRequest) error {
	if req.IdempotencyKey == "" || req.IdempotencyKey != strings.TrimSpace(req.IdempotencyKey) {
		return fmt.Errorf("idempotency_key is required and must be trimmed")
	}
	if req.ExpectedUpdatedAt.IsZero() {
		return fmt.Errorf("expected_updated_at is required")
	}
	if req.DesiredTeamStatus != "active" && req.DesiredTeamStatus != "archived" {
		return fmt.Errorf("desired_team_status must be active or archived")
	}
	if req.LeadAgentID == "" || req.LeadAgentID != strings.TrimSpace(req.LeadAgentID) {
		return fmt.Errorf("lead_agent_id is required and must be trimmed")
	}
	if len(req.Workers) == 0 {
		return fmt.Errorf("team roster requires at least one configured worker")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	seen := make(map[string]struct{}, len(req.Workers)+1)
	seen[req.LeadAgentID] = struct{}{}
	for _, worker := range req.Workers {
		agentID := worker.WorkerAgentID
		if agentID == "" || agentID != strings.TrimSpace(agentID) {
			return fmt.Errorf("worker_agent_id is required and must be trimmed")
		}
		if _, exists := seen[agentID]; exists {
			return fmt.Errorf("agent %q appears more than once in roster", agentID)
		}
		seen[agentID] = struct{}{}
		if len(worker.AllowedKinds) == 0 {
			return fmt.Errorf("worker %q allowed_kinds must not be empty", agentID)
		}
		defaultAllowed := false
		kinds := make(map[string]struct{}, len(worker.AllowedKinds))
		for _, kind := range worker.AllowedKinds {
			if kind != "consult" && kind != "dispatch" && kind != "handoff" {
				return fmt.Errorf("worker %q has invalid allowed kind %q", agentID, kind)
			}
			if _, duplicate := kinds[kind]; duplicate {
				return fmt.Errorf("worker %q repeats allowed kind %q", agentID, kind)
			}
			kinds[kind] = struct{}{}
			defaultAllowed = defaultAllowed || kind == worker.DefaultKind
		}
		if !defaultAllowed {
			return fmt.Errorf("worker %q default_kind must belong to allowed_kinds", agentID)
		}
	}
	return nil
}

func (s *Server) handleDeleteTeam(c echo.Context) error {
	return writeRevocationError(
		c,
		http.StatusConflict,
		"team_roster_write_required",
		"team archive must be expressed through the complete roster command",
		map[string]any{"required_route": "/v1/teams/" + c.Param("id") + "/roster"},
	)
}

func rosterAccessResources(teamID string, req teamRosterRequest) (block, grant []admissionfence.Resource) {
	if req.DesiredTeamStatus == "archived" {
		block = append(block, admissionfence.Team(teamID))
	} else {
		grant = append(grant, admissionfence.Team(teamID))
	}
	for _, worker := range req.Workers {
		base := admissionfence.Worker(teamID, worker.WorkerAgentID)
		if worker.Enabled {
			grant = append(grant, base)
		} else {
			block = append(block, base)
		}
		allowed := map[string]bool{}
		for _, kind := range worker.AllowedKinds {
			allowed[kind] = true
		}
		for _, kind := range []string{"consult", "dispatch", "handoff"} {
			key := base
			key.Version = kind
			if worker.Enabled && allowed[kind] {
				grant = append(grant, key)
			} else {
				block = append(block, key)
			}
		}
	}
	return block, grant
}
