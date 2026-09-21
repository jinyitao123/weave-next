package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

func (s *Server) seedTeamMemberConfigDraft(c echo.Context) (*teamMemberConfigDraftResponse, error) {
	if s.Pool == nil || s.Registry == nil {
		return nil, echo.NewHTTPError(http.StatusServiceUnavailable, "team configuration store unavailable")
	}
	ctx, workspaceID, teamID, agentID := c.Request().Context(), getTenant(c), c.Param("id"), c.Param("agent")
	var agentName string
	var duty, whenToUse, contextInstruction, defaultKind, resultRequirement string
	var allowedKinds []string
	var enabled bool
	err := s.Pool.QueryRow(ctx, `
		SELECT agent.name,
		       COALESCE(worker.duty, ''), COALESCE(worker.when_to_use, ''),
		       COALESCE(worker.context_instruction, ''), COALESCE(worker.allowed_kinds, ARRAY[]::TEXT[]),
		       COALESCE(worker.default_kind, ''), COALESCE(worker.result_requirement, ''),
		       CASE WHEN team.lead_avatar_id=agent.id THEN true ELSE COALESCE(worker.enabled, false) END
		FROM weave_teams AS team
		JOIN weave_agents AS agent ON agent.workspace_id=team.workspace_id AND agent.id=$3 AND agent.deleted=false
		LEFT JOIN weave_team_workers AS worker ON worker.workspace_id=team.workspace_id AND worker.team_id=team.id AND worker.worker_agent_id=agent.id
		WHERE team.workspace_id=$1 AND team.id=$2 AND (team.lead_avatar_id=agent.id OR worker.worker_agent_id IS NOT NULL)
	`, workspaceID, teamID, agentID).Scan(&agentName, &duty, &whenToUse, &contextInstruction, &allowedKinds, &defaultKind, &resultRequirement, &enabled)
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
			WHERE workspace_id=$1 AND team_id=$2 AND agent_id=$3 AND revision=$8 AND $8>0
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
