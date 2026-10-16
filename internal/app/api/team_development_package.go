package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/kernel/engine"
)

// A team package carries one team's draft from one workspace to another. Its
// members are told apart by keys instead of agent ids, and everything that
// only means something in its own workspace travels by name: the node a
// member runs on, its MCP services and its library skills (with their text).
// It carries no secrets, no trials or runs and no publication state.
const (
	teamPackageFormat   = "weave-team-package"
	teamPackageVersion  = 1
	teamPackageMaxBytes = 4 << 20
)

type teamPackage struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	ExportedAt time.Time           `json:"exported_at"`
	Team       teamPackageTeam     `json:"team"`
	Members    []teamPackageMember `json:"members"`
	Workflows  []teamPackageFlow   `json:"workflows"`
}

type teamPackageTeam struct {
	Name      string `json:"name"`
	Objective string `json:"objective"`
}

type teamPackageMember struct {
	Key string `json:"key"`
	// Configuration is the draft member's, without its node, MCP service ids
	// and library skill names; those travel by name below.
	Configuration teamMemberAgentConfiguration `json:"configuration"`
	Relationship  teamMemberRelationshipDraft  `json:"relationship"`
	Node          string                       `json:"node,omitempty"`
	MCPServers    []string                     `json:"mcp_servers"`
	LibrarySkills []teamMemberInlineSkill      `json:"library_skills"`
}

type teamPackageFlow struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Trigger     json.RawMessage `json:"trigger_config"`
	Graph       json.RawMessage `json:"graph_definition"`
}

// handleExportTeamDevelopment returns the team's saved draft as a package.
func (s *Server) handleExportTeamDevelopment(c echo.Context) error {
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, http.StatusNotFound, "development_not_found", "团队不存在，或还没有读取过开发草稿")
	}
	if err != nil {
		return err
	}
	pkg := teamPackage{Format: teamPackageFormat, Version: teamPackageVersion, ExportedAt: time.Now().UTC(),
		Team: teamPackageTeam{Name: d.Document.Name, Objective: d.Document.Objective}, Members: []teamPackageMember{}, Workflows: []teamPackageFlow{}}
	nodes := map[string]string{}
	for _, node := range s.developmentRuntimes(ctx, ws) {
		nodes[node.ID] = node.Name
	}
	keys := map[string]string{}
	for index, member := range d.Document.Members {
		key := fmt.Sprintf("member-%d", index+1)
		if member.Configuration.Role == "avatar" {
			key = "lead"
		}
		keys[member.ID] = key
		cfg := member.Configuration
		entry := teamPackageMember{Key: key, Relationship: member.Relationship, Node: nodes[strings.TrimSpace(cfg.RuntimeID)], MCPServers: []string{}, LibrarySkills: []teamMemberInlineSkill{}}
		for _, serverID := range cfg.MCPServerIDs {
			var name string
			if err = tx.QueryRow(ctx, `SELECT display_name FROM weave_mcp_servers WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL`, ws, serverID).Scan(&name); err == nil {
				entry.MCPServers = append(entry.MCPServers, name)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		for _, skillName := range cfg.SkillNames {
			var skill teamMemberInlineSkill
			err = tx.QueryRow(ctx, `SELECT v.name,v.description,v.body,v.always_active FROM weave_skills s
				JOIN weave_skill_versions v ON v.workspace_id=s.workspace_id AND v.skill_id=s.id AND v.version=s.latest_version
				WHERE s.workspace_id=$1 AND s.name=$2`, ws, skillName).Scan(&skill.Name, &skill.Description, &skill.Body, &skill.AlwaysActive)
			if err == nil {
				entry.LibrarySkills = append(entry.LibrarySkills, skill)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		cfg.RuntimeID, cfg.MCPServerIDs, cfg.SkillNames = "", []string{}, []string{}
		entry.Configuration = cfg
		pkg.Members = append(pkg.Members, entry)
	}
	for _, flow := range d.Document.Workflows {
		graph, err := remapGraphMembers(flow.Graph, keys, false)
		if err != nil {
			return err
		}
		pkg.Workflows = append(pkg.Workflows, teamPackageFlow{Name: flow.Name, Description: flow.Description, Trigger: flow.Trigger, Graph: graph})
	}
	return c.JSON(http.StatusOK, pkg)
}

// remapGraphMembers rewrites the agent each worker step names. strict refuses
// a step whose agent is not in the map; otherwise such a step is left alone.
func remapGraphMembers(raw json.RawMessage, members map[string]string, strict bool) (json.RawMessage, error) {
	var graph map[string]any
	if err := json.Unmarshal(raw, &graph); err != nil {
		return nil, echo.NewHTTPError(422, "流程定义无效")
	}
	nodes, _ := graph["nodes"].([]any)
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		config, _ := node["config"].(map[string]any)
		agent, named := config["agent_id"].(string)
		if !named {
			continue
		}
		if mapped, known := members[agent]; known {
			config["agent_id"] = mapped
		} else if strict {
			return nil, echo.NewHTTPError(422, fmt.Sprintf("流程步骤“%s”的执行成员不在团队包里", editString(node["label"])))
		}
	}
	return json.Marshal(graph)
}

// handleImportDevelopmentTeam makes a new team from a package once per request
// id. It never overwrites a team and never publishes: the new draft is trialled
// and published like any other.
func (s *Server) handleImportDevelopmentTeam(c echo.Context) error {
	if s.Pool == nil || s.Registry == nil || s.OrgStore == nil {
		return echo.NewHTTPError(503, "开发服务暂不可用")
	}
	if c.Request().ContentLength > teamPackageMaxBytes {
		return workflowError(c, http.StatusRequestEntityTooLarge, "team_package_too_large", "团队包不能超过 4 MiB")
	}
	var request struct {
		RequestID string      `json:"request_id"`
		Package   teamPackage `json:"package"`
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, teamPackageMaxBytes)
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowError(c, http.StatusUnprocessableEntity, "team_package_invalid", "团队包格式无效")
	}
	requestID, err := uuid.Parse(request.RequestID)
	if err != nil {
		return workflowError(c, http.StatusUnprocessableEntity, "team_create_invalid", "须提供有效的 request_id")
	}
	pkg := request.Package
	if refusal := checkTeamPackage(pkg); refusal != "" {
		return workflowError(c, http.StatusUnprocessableEntity, "team_package_invalid", refusal)
	}
	name, objective := strings.TrimSpace(pkg.Team.Name), strings.TrimSpace(pkg.Team.Objective)
	canonical, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	packageSum := sha256.Sum256(canonical)
	var adjustments []string
	return s.createDevelopmentTeamOnce(c, requestID, name, objective, "import:"+hex.EncodeToString(packageSum[:]), func() (string, error) {
		teamID, err := s.createStarterTeam(c, requestID.String(), name, objective, "business")
		if err != nil {
			return "", err
		}
		adjustments, err = s.applyTeamPackage(c.Request().Context(), getTenant(c), teamID, getUserID(c), pkg)
		return teamID, err
	}, func() []string { return adjustments })
}

// checkTeamPackage says what makes a package unusable, or "" when it is fine.
func checkTeamPackage(pkg teamPackage) string {
	if pkg.Format != teamPackageFormat || pkg.Version != teamPackageVersion {
		return "不是本版本支持的团队包"
	}
	name, objective := strings.TrimSpace(pkg.Team.Name), strings.TrimSpace(pkg.Team.Objective)
	if name == "" || utf8.RuneCountInString(name) > 80 || objective == "" || utf8.RuneCountInString(objective) > 2000 {
		return "团队包须有团队名称（80 字以内）和目标（2000 字以内）"
	}
	keys, leads, workers := map[string]string{}, 0, 0
	for _, member := range pkg.Members {
		if member.Key == "" || keys[member.Key] != "" {
			return "团队包里的成员标识为空或重复"
		}
		keys[member.Key] = member.Key
		switch member.Configuration.Role {
		case "avatar":
			leads++
		case "worker":
			workers++
		default:
			return "团队包里有身份无效的成员"
		}
	}
	if leads != 1 || workers < 1 || len(pkg.Members) > 32 || len(pkg.Workflows) > 20 {
		return "团队包须有一位负责人和至少一位成员（合计最多 32 位），流程最多 20 条"
	}
	// Validate the complete draft before the starter path persists any rows.
	doc := developmentDocument{Name: name, Objective: objective, Audience: []string{}}
	for _, member := range pkg.Members {
		doc.Members = append(doc.Members, developmentMember{ID: uuid.NewString(), Configuration: member.Configuration, Relationship: member.Relationship})
	}
	for _, flow := range pkg.Workflows {
		if _, err := remapGraphMembers(flow.Graph, keys, true); err != nil {
			return "团队包流程无效，或流程步骤的执行成员不在团队包里"
		}
		trigger := flow.Trigger
		if len(trigger) == 0 {
			trigger = json.RawMessage(`{}`)
		}
		doc.Workflows = append(doc.Workflows, developmentWorkflow{ID: uuid.NewString(), Name: flow.Name, Description: flow.Description, Graph: flow.Graph, Trigger: trigger})
	}
	if err := validateDevelopmentDocument(doc); err != nil {
		return "团队包的成员名称或流程定义无效"
	}
	return ""
}

// applyTeamPackage replaces a new team's draft with the package. The team's
// own lead stays the lead and takes the package lead's settings; members get
// new ids. What only means something by name is found in this workspace; what
// is not found is left out and named in the returned adjustments.
func (s *Server) applyTeamPackage(ctx context.Context, ws, teamID, actor string, pkg teamPackage) ([]string, error) {
	models, _ := s.developmentModels(ctx, ws)
	nodes := s.developmentRuntimes(ctx, ws)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, teamID, true)
	if err != nil {
		return nil, err
	}
	tools, err := readDevelopmentTools(ctx, tx, ws)
	if err != nil {
		return nil, err
	}
	leadID := ""
	for _, member := range d.Document.Members {
		if member.Configuration.Role == "avatar" {
			leadID = member.ID
		}
	}
	if leadID == "" {
		return nil, errors.New("new team has no lead")
	}
	ids := map[string]string{}
	for _, member := range pkg.Members {
		ids[member.Key] = uuid.NewString()
		if member.Configuration.Role == "avatar" {
			ids[member.Key] = leadID
		}
	}
	adjustments := []string{}
	note := func(format string, args ...any) { adjustments = append(adjustments, fmt.Sprintf(format, args...)) }
	members := make([]developmentMember, 0, len(pkg.Members))
	for _, entry := range pkg.Members {
		cfg := entry.Configuration
		name := strings.TrimSpace(cfg.DisplayName)
		cfg.RuntimeID, cfg.MCPServerIDs, cfg.SkillNames = "", []string{}, []string{}
		if cfg.Skills == nil {
			cfg.Skills = []teamMemberInlineSkill{}
		}
		if cfg.Model != "" && !slices.Contains(models, cfg.Model) {
			note("成员“%s”用的模型“%s”在本组织没有启用，已清空，请重新选择", name, cfg.Model)
			cfg.Model = ""
		}
		if engine.IsCLIEngine(cfg.Engine) {
			if index := slices.IndexFunc(nodes, func(node editRuntime) bool {
				return node.Name == entry.Node && slices.Contains(node.Engines, cfg.Engine)
			}); entry.Node != "" && index >= 0 {
				cfg.RuntimeID = nodes[index].ID
			} else {
				note("成员“%s”使用外部引擎 %s，本工作区没有可用的同名执行节点“%s”，请为它选择节点", name, cfg.Engine, entry.Node)
			}
		}
		for _, serverName := range entry.MCPServers {
			if server, found := tools.server(serverName); found && server.Usable {
				cfg.MCPServerIDs = append(cfg.MCPServerIDs, server.ID)
			} else {
				note("成员“%s”的 MCP 服务“%s”在本工作区不存在或不可用，已去掉", name, serverName)
			}
		}
		for _, skill := range entry.LibrarySkills {
			if _, found := tools.skill(skill.Name); found {
				cfg.SkillNames = append(cfg.SkillNames, skill.Name)
				continue
			}
			if !slices.ContainsFunc(cfg.Skills, func(inline teamMemberInlineSkill) bool { return inline.Name == skill.Name }) {
				cfg.Skills = append(cfg.Skills, skill)
			}
			note("技能库里没有“%s”，已作为成员“%s”的成员技能带入", skill.Name, name)
		}
		members = append(members, developmentMember{ID: ids[entry.Key], Configuration: cfg, Relationship: entry.Relationship})
	}
	flows := make([]developmentWorkflow, 0, len(pkg.Workflows))
	for _, flow := range pkg.Workflows {
		graph, err := remapGraphMembers(flow.Graph, ids, true)
		if err != nil {
			return nil, err
		}
		trigger := flow.Trigger
		if len(trigger) == 0 {
			trigger = json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
		}
		flows = append(flows, developmentWorkflow{ID: uuid.NewString(), Name: flow.Name, Description: flow.Description, Graph: graph, Trigger: trigger})
	}
	d.Document = developmentDocument{Name: strings.TrimSpace(pkg.Team.Name), Objective: strings.TrimSpace(pkg.Team.Objective), Audience: []string{}, Members: members, Workflows: flows}
	if err = validateDevelopmentDocument(d.Document); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$3,revision=revision+1,updated_by=$4,updated_at=now() WHERE workspace_id=$1 AND team_id=$2`,
		ws, teamID, encodeDevelopment(d.Document), actor); err != nil {
		return nil, err
	}
	return adjustments, tx.Commit(ctx)
}
