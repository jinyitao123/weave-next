package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type teamMemberAgentConfiguration struct {
	DisplayName     string          `json:"display_name"`
	Role            string          `json:"role"`
	Engine          string          `json:"engine"`
	RuntimeID       string          `json:"runtime_id"`
	Model           string          `json:"model"`
	SystemPrompt    string          `json:"system_prompt"`
	SkillNames      []string        `json:"skill_names"`
	MCPServerIDs    []string        `json:"mcp_server_ids"`
	PermissionAllow []string        `json:"permission_allow"`
	PermissionAsk   []string        `json:"permission_ask"`
	PermissionDeny  []string        `json:"permission_deny"`
	MemoryEnabled   bool            `json:"memory_enabled"`
	MemoryScope     string          `json:"memory_scope"`
	MaxTokens       int64           `json:"max_tokens"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	StepBudget      int64           `json:"step_budget"`
	MaxCostUSD      float64         `json:"max_cost_usd"`
	OutputSchema    json.RawMessage `json:"output_schema,omitempty"`
}

type teamMemberRelationshipDraft struct {
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
	Enabled            bool     `json:"enabled"`
}

type teamMemberConfigDraftResponse struct {
	Version                string                       `json:"version"`
	TeamID                 string                       `json:"team_id"`
	AgentID                string                       `json:"agent_id"`
	AgentName              string                       `json:"agent_name"`
	BaseAgentVersion       int                          `json:"base_agent_version"`
	Revision               int                          `json:"revision"`
	PublishedConfiguration teamMemberAgentConfiguration `json:"published_configuration"`
	PublishedRelationship  teamMemberRelationshipDraft  `json:"published_relationship"`
	Configuration          teamMemberAgentConfiguration `json:"configuration"`
	Relationship           teamMemberRelationshipDraft  `json:"relationship"`
	UpdatedAt              time.Time                    `json:"updated_at"`
	UpdatedBy              string                       `json:"updated_by,omitempty"`
}

type saveTeamMemberConfigDraftRequest struct {
	Revision      int                          `json:"revision"`
	Configuration teamMemberAgentConfiguration `json:"configuration"`
	Relationship  teamMemberRelationshipDraft  `json:"relationship"`
}

type applyTeamMemberConfigDraftRequest struct {
	Revision int `json:"revision"`
}

func (s *Server) seedTeamMemberConfigDraft(c echo.Context) (*teamMemberConfigDraftResponse, error) {
	if s.Pool == nil || s.Registry == nil {
		return nil, echo.NewHTTPError(http.StatusServiceUnavailable, "team configuration store unavailable")
	}
	ctx, workspaceID, teamID, agentID := c.Request().Context(), getTenant(c), c.Param("id"), c.Param("agent")
	var agentName string
	var lead bool
	var duty, whenToUse, contextInstruction, defaultKind, resultRequirement string
	var allowedKinds []string
	var enabled bool
	err := s.Pool.QueryRow(ctx, `
		SELECT agent.name,
		       COALESCE(worker.duty, ''), COALESCE(worker.when_to_use, ''),
		       COALESCE(worker.context_instruction, ''), COALESCE(worker.allowed_kinds, ARRAY[]::TEXT[]),
		       COALESCE(worker.default_kind, ''), COALESCE(worker.result_requirement, ''),
		       CASE WHEN team.lead_avatar_id=agent.id THEN true ELSE COALESCE(worker.enabled, false) END,
		       team.lead_avatar_id=agent.id
		FROM weave_teams AS team
		JOIN weave_agents AS agent ON agent.workspace_id=team.workspace_id AND agent.id=$3 AND agent.deleted=false
		LEFT JOIN weave_team_workers AS worker ON worker.workspace_id=team.workspace_id AND worker.team_id=team.id AND worker.worker_agent_id=agent.id
		WHERE team.workspace_id=$1 AND team.id=$2 AND (team.lead_avatar_id=agent.id OR worker.worker_agent_id IS NOT NULL)
	`, workspaceID, teamID, agentID).Scan(&agentName, &duty, &whenToUse, &contextInstruction, &allowedKinds, &defaultKind, &resultRequirement, &enabled, &lead)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "team member not found")
	}
	if err != nil {
		return nil, err
	}
	record, err := s.Registry.Get(ctx, workspaceID, agentName)
	if err != nil {
		return nil, err
	}
	skillNames := make([]string, 0, len(record.SkillRefs)+len(record.Spec.Skills))
	for _, skill := range record.SkillRefs {
		if strings.TrimSpace(skill.Name) != "" {
			skillNames = append(skillNames, skill.Name)
		}
	}
	for _, skill := range record.Spec.Skills {
		if strings.TrimSpace(skill.Name) != "" {
			skillNames = append(skillNames, skill.Name)
		}
	}
	serverIDs := make([]string, 0, len(record.MCPServers))
	for _, server := range record.MCPServers {
		if strings.TrimSpace(server.ServerID) != "" {
			serverIDs = append(serverIDs, server.ServerID)
		}
	}
	memoryEnabled, memoryScope := false, ""
	if record.MemoryConfig != nil {
		memoryEnabled, memoryScope = record.MemoryConfig.Enabled, record.MemoryConfig.Scope
	}
	var outputSchema json.RawMessage
	if record.OutputSchema != nil {
		outputSchema = append(json.RawMessage(nil), (*record.OutputSchema)...)
	}
	if lead && strings.TrimSpace(duty) == "" {
		duty = record.Spec.Identity.Core
	}
	configuration := teamMemberAgentConfiguration{
		DisplayName: record.DisplayName, Role: record.Role, Engine: record.Engine, RuntimeID: record.RuntimeID, Model: record.Model,
		SystemPrompt: record.Spec.SystemPrompt, SkillNames: skillNames, MCPServerIDs: serverIDs,
		PermissionAllow: record.Permissions.Allow, PermissionAsk: record.Permissions.Ask, PermissionDeny: record.Permissions.Deny,
		MemoryEnabled: memoryEnabled, MemoryScope: memoryScope, MaxTokens: record.MaxTokens, MaxOutputTokens: record.MaxOutputTokens,
		StepBudget: record.StepBudget, MaxCostUSD: record.MaxCostUSD, OutputSchema: outputSchema,
	}
	relationship := teamMemberRelationshipDraft{Duty: duty, WhenToUse: whenToUse, ContextInstruction: contextInstruction, AllowedKinds: allowedKinds, DefaultKind: defaultKind, ResultRequirement: resultRequirement, Enabled: enabled}
	return &teamMemberConfigDraftResponse{
		Version: "1", TeamID: teamID, AgentID: record.ID, AgentName: record.Name,
		BaseAgentVersion: record.Version, Revision: 0, UpdatedAt: record.UpdatedAt,
		PublishedConfiguration: configuration, PublishedRelationship: relationship,
		Configuration: configuration, Relationship: relationship,
	}, nil
}

func teamMemberResourceNames(record *registry.AgentRecord) ([]string, []string) {
	skills := make([]string, 0, len(record.SkillRefs)+len(record.Spec.Skills))
	for _, skill := range record.SkillRefs {
		skills = append(skills, strings.TrimSpace(skill.Name))
	}
	for _, skill := range record.Spec.Skills {
		skills = append(skills, strings.TrimSpace(skill.Name))
	}
	servers := make([]string, 0, len(record.MCPServers))
	for _, server := range record.MCPServers {
		servers = append(servers, strings.TrimSpace(server.ServerID))
	}
	slices.Sort(skills)
	slices.Sort(servers)
	return skills, servers
}

func sameNames(left, right []string) bool {
	a, b := append([]string(nil), left...), append([]string(nil), right...)
	for i := range a {
		a[i] = strings.TrimSpace(a[i])
	}
	for i := range b {
		b[i] = strings.TrimSpace(b[i])
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func validateAppliedRelationship(value teamMemberRelationshipDraft) error {
	if strings.TrimSpace(value.Duty) == "" {
		return errors.New("duty is required")
	}
	if len(value.AllowedKinds) == 0 {
		return errors.New("allowed_kinds must not be empty")
	}
	foundDefault := false
	seen := map[string]bool{}
	for _, kind := range value.AllowedKinds {
		if kind != "consult" && kind != "dispatch" && kind != "handoff" {
			return fmt.Errorf("invalid allowed kind %q", kind)
		}
		if seen[kind] {
			return fmt.Errorf("duplicate allowed kind %q", kind)
		}
		seen[kind], foundDefault = true, foundDefault || kind == value.DefaultKind
	}
	if !foundDefault {
		return errors.New("default_kind must belong to allowed_kinds")
	}
	return nil
}

func (s *Server) applyTeamMemberConfigDraft(ctx context.Context, workspaceID, teamID, agentID, userID string, revision int) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var baseVersion, storedRevision int
	var configurationJSON, relationshipJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT base_agent_version, revision, configuration, relationship
		FROM weave_team_member_config_drafts
		WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3 AND revision=$4
		FOR UPDATE
	`, workspaceID, teamID, agentID, revision).Scan(&baseVersion, &storedRevision, &configurationJSON, &relationshipJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return echo.NewHTTPError(http.StatusConflict, "配置草稿已被更新，请刷新后继续")
	}
	if err != nil {
		return err
	}
	var configuration teamMemberAgentConfiguration
	var relationship teamMemberRelationshipDraft
	if err := json.Unmarshal(configurationJSON, &configuration); err != nil {
		return err
	}
	if err := json.Unmarshal(relationshipJSON, &relationship); err != nil {
		return err
	}

	var agentName, role string
	var data []byte
	var lead bool
	err = tx.QueryRow(ctx, `
		SELECT agent.name, agent.role, agent.spec, team.lead_avatar_id=agent.id
		FROM weave_teams AS team
		JOIN weave_agents AS agent ON agent.workspace_id=team.workspace_id AND agent.id=$3 AND agent.deleted=false
		LEFT JOIN weave_team_workers AS worker ON worker.workspace_id=team.workspace_id AND worker.team_id=team.id AND worker.worker_agent_id=agent.id
		WHERE team.workspace_id=$1 AND team.id=$2 AND (team.lead_avatar_id=agent.id OR worker.worker_agent_id IS NOT NULL)
		FOR UPDATE OF team, agent
	`, workspaceID, teamID, agentID).Scan(&agentName, &role, &data, &lead)
	if errors.Is(err, pgx.ErrNoRows) {
		return echo.NewHTTPError(http.StatusNotFound, "team member not found")
	}
	if err != nil {
		return err
	}
	var record registry.AgentRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.Version != baseVersion {
		return echo.NewHTTPError(http.StatusConflict, "成员正式配置已更新，请刷新草稿后继续")
	}
	if configuration.Role != role {
		return echo.NewHTTPError(http.StatusBadRequest, "成员身份不能在这里切换")
	}
	skills, servers := teamMemberResourceNames(&record)
	if !sameNames(skills, configuration.SkillNames) || !sameNames(servers, configuration.MCPServerIDs) {
		return echo.NewHTTPError(http.StatusBadRequest, "技能和工具绑定需要在能力页单独调整")
	}
	if !lead {
		if err := validateAppliedRelationship(relationship); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}

	record.DisplayName, record.Engine, record.RuntimeID, record.Model = strings.TrimSpace(configuration.DisplayName), configuration.Engine, strings.TrimSpace(configuration.RuntimeID), strings.TrimSpace(configuration.Model)
	record.Spec.SystemPrompt = configuration.SystemPrompt
	if lead {
		record.Spec.Identity.Core = strings.TrimSpace(relationship.Duty)
		record.Spec.Identity.Raw = record.Spec.Identity.Core
	}
	record.Permissions = registry.PermissionConfig{Allow: configuration.PermissionAllow, Ask: configuration.PermissionAsk, Deny: configuration.PermissionDeny}
	if record.MemoryConfig == nil {
		record.MemoryConfig = &registry.MemoryConfig{}
	}
	record.MemoryConfig.Enabled, record.MemoryConfig.Scope = configuration.MemoryEnabled, configuration.MemoryScope
	record.MaxTokens, record.MaxOutputTokens, record.StepBudget, record.MaxCostUSD = configuration.MaxTokens, configuration.MaxOutputTokens, configuration.StepBudget, configuration.MaxCostUSD
	if len(configuration.OutputSchema) == 0 || string(configuration.OutputSchema) == "null" {
		record.OutputSchema = nil
	} else {
		raw := append(json.RawMessage(nil), configuration.OutputSchema...)
		record.OutputSchema = &raw
	}
	if err := s.Registry.PutTx(ctx, tx, workspaceID, &record); err != nil {
		return err
	}
	if !lead {
		tag, err := tx.Exec(ctx, `UPDATE weave_team_workers SET duty=$4, when_to_use=$5, context_instruction=$6, allowed_kinds=$7, default_kind=$8, result_requirement=$9, enabled=$10, updated_at=now() WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3`, workspaceID, teamID, agentID, relationship.Duty, relationship.WhenToUse, relationship.ContextInstruction, relationship.AllowedKinds, relationship.DefaultKind, relationship.ResultRequirement, relationship.Enabled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return echo.NewHTTPError(http.StatusNotFound, "team member not found")
		}
		var status string
		var enabledWorkers int
		if err := tx.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM weave_team_workers WHERE workspace_id=$1 AND team_id=$2 AND enabled) FROM weave_teams WHERE workspace_id=$1 AND id=$2`, workspaceID, teamID).Scan(&status, &enabledWorkers); err != nil {
			return err
		}
		if status == "active" && enabledWorkers == 0 {
			return echo.NewHTTPError(http.StatusConflict, "运行中的团队至少需要一位可参与成员")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE weave_team_member_config_drafts SET base_agent_version=$5, revision=0, updated_by=$6, updated_at=now() WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3 AND revision=$4`, workspaceID, teamID, agentID, storedRevision, record.Version, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) handleApplyTeamMemberConfigDraft(c echo.Context) error {
	if s.Pool == nil || s.Registry == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "team configuration store unavailable")
	}
	var request applyTeamMemberConfigDraftRequest
	if err := c.Bind(&request); err != nil || request.Revision < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_team_member_config_apply", "error": "invalid team member configuration apply"})
	}
	if err := s.applyTeamMemberConfigDraft(c.Request().Context(), getTenant(c), c.Param("id"), c.Param("agent"), getUserID(c), request.Revision); err != nil {
		return err
	}
	return s.handleGetTeamMemberConfigDraft(c)
}

func (s *Server) handleGetTeamMemberConfigDraft(c echo.Context) error {
	seed, err := s.seedTeamMemberConfigDraft(c)
	if err != nil {
		return err
	}
	var configuration, relationship []byte
	err = s.Pool.QueryRow(c.Request().Context(), `
		SELECT base_agent_version, revision, configuration, relationship, updated_at, updated_by
		FROM weave_team_member_config_drafts WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3
	`, getTenant(c), seed.TeamID, seed.AgentID).Scan(&seed.BaseAgentVersion, &seed.Revision, &configuration, &relationship, &seed.UpdatedAt, &seed.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusOK, seed)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(configuration, &seed.Configuration); err != nil {
		return err
	}
	if err := json.Unmarshal(relationship, &seed.Relationship); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, seed)
}

func (s *Server) handlePutTeamMemberConfigDraft(c echo.Context) error {
	seed, err := s.seedTeamMemberConfigDraft(c)
	if err != nil {
		return err
	}
	var request saveTeamMemberConfigDraftRequest
	if err := c.Bind(&request); err != nil || request.Revision < 0 || strings.TrimSpace(request.Configuration.DisplayName) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_team_member_config_draft", "error": "invalid team member configuration draft"})
	}
	configuration, err := json.Marshal(request.Configuration)
	if err != nil {
		return err
	}
	relationship, err := json.Marshal(request.Relationship)
	if err != nil {
		return err
	}
	result := *seed
	err = s.Pool.QueryRow(c.Request().Context(), `
		WITH updated AS (
			UPDATE weave_team_member_config_drafts
			SET base_agent_version=$4, revision=revision+1, configuration=$5::jsonb,
			    relationship=$6::jsonb, updated_by=$7, updated_at=now()
			WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3 AND revision=$8
			RETURNING revision, updated_at
		), inserted AS (
			INSERT INTO weave_team_member_config_drafts(workspace_id,team_id,agent_id,base_agent_version,revision,configuration,relationship,updated_by)
			SELECT $1,$2,$3,$4,1,$5::jsonb,$6::jsonb,$7 WHERE $8=0
			ON CONFLICT (workspace_id,team_id,agent_id) DO NOTHING
			RETURNING revision, updated_at
		)
		SELECT revision, updated_at FROM updated
		UNION ALL
		SELECT revision, updated_at FROM inserted
		LIMIT 1
	`, getTenant(c), seed.TeamID, seed.AgentID, seed.BaseAgentVersion, string(configuration), string(relationship), getUserID(c), request.Revision).Scan(&result.Revision, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusConflict, map[string]string{"code": "draft_revision_conflict", "error": "配置草稿已被更新，请刷新后继续"})
	}
	if err != nil {
		return err
	}
	result.Configuration, result.Relationship, result.UpdatedBy = request.Configuration, request.Relationship, getUserID(c)
	return c.JSON(http.StatusOK, result)
}
