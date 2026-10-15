package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// editTools are the workspace's MCP services and library skills a member may
// be given, read once per request so edits name them instead of guessing.
type editTools struct {
	servers []editMCPServer
	skills  []editLibrarySkill
}

// editMCPServer is one MCP service of the workspace. Usable says it can be
// frozen into a member: enabled, reachable over HTTP and probed at least once.
type editMCPServer struct {
	ID, Name, Slug string
	Usable         bool
}

type editLibrarySkill struct {
	ID, Name string
	Version  int64
}

type developmentQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readDevelopmentTools(ctx context.Context, q developmentQuerier, ws string) (editTools, error) {
	tools := editTools{}
	rows, err := q.Query(ctx, `SELECT id,display_name,slug,
			enabled AND transport='streamable_http' AND last_handshake_at IS NOT NULL AND revoked_at IS NULL
		FROM weave_mcp_servers WHERE workspace_id=$1 AND deleted_at IS NULL ORDER BY display_name,slug`, ws)
	if err != nil {
		return tools, err
	}
	for rows.Next() {
		var server editMCPServer
		if err = rows.Scan(&server.ID, &server.Name, &server.Slug, &server.Usable); err != nil {
			rows.Close()
			return tools, err
		}
		tools.servers = append(tools.servers, server)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return tools, err
	}
	rows, err = q.Query(ctx, `SELECT id,name,latest_version FROM weave_skills WHERE workspace_id=$1 AND latest_version>0 ORDER BY name`, ws)
	if err != nil {
		return tools, err
	}
	defer rows.Close()
	for rows.Next() {
		var skill editLibrarySkill
		if err = rows.Scan(&skill.ID, &skill.Name, &skill.Version); err != nil {
			return tools, err
		}
		tools.skills = append(tools.skills, skill)
	}
	return tools, rows.Err()
}

// developmentMemberTools is what a developer may give a member, by name.
type developmentMemberTools struct {
	MCPServers []developmentMCPServerView `json:"mcpServers"`
	Skills     []string                   `json:"skills"`
}

type developmentMCPServerView struct {
	Name   string `json:"name"`
	Usable bool   `json:"usable"`
}

func (t editTools) view() developmentMemberTools {
	view := developmentMemberTools{MCPServers: []developmentMCPServerView{}, Skills: []string{}}
	for _, server := range t.servers {
		view.MCPServers = append(view.MCPServers, developmentMCPServerView{Name: server.Name, Usable: server.Usable})
	}
	for _, skill := range t.skills {
		view.Skills = append(view.Skills, skill.Name)
	}
	return view
}

// handleDevelopmentMemberTools lists the MCP services and library skills a
// developer may give a member, with the server ids the draft stores.
func (s *Server) handleDevelopmentMemberTools(c echo.Context) error {
	tools, err := readDevelopmentTools(c.Request().Context(), s.Pool, getTenant(c))
	if err != nil {
		return err
	}
	servers := make([]map[string]any, 0, len(tools.servers))
	for _, server := range tools.servers {
		servers = append(servers, map[string]any{"id": server.ID, "name": server.Name, "usable": server.Usable})
	}
	skills := make([]string, 0, len(tools.skills))
	for _, skill := range tools.skills {
		skills = append(skills, skill.Name)
	}
	return c.JSON(http.StatusOK, map[string]any{"mcp_servers": servers, "skills": skills})
}

func (t editTools) server(name string) (editMCPServer, bool) {
	for _, server := range t.servers {
		if server.Name == name || server.Slug == name {
			return server, true
		}
	}
	return editMCPServer{}, false
}

func (t editTools) serverByID(id string) (editMCPServer, bool) {
	for _, server := range t.servers {
		if server.ID == id {
			return server, true
		}
	}
	return editMCPServer{}, false
}

func (t editTools) skill(name string) (editLibrarySkill, bool) {
	for _, skill := range t.skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return editLibrarySkill{}, false
}

func editSelected(operation map[string]any) (bool, error) {
	selected, ok := operation["selected"].(bool)
	if !ok {
		return false, editFail("selected 须为 true（添加）或 false（移除）")
	}
	return selected, nil
}

// changeMemberMCP gives a member one of the workspace's MCP services, or takes
// it away. Its read-only tools run as they are; tools that write are refused by
// the same write gate in trials and in published work.
func (e *editSession) changeMemberMCP(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	name, err := editText(operation, "server", "MCP 服务名称", 120)
	if err != nil {
		return err
	}
	selected, err := editSelected(operation)
	if err != nil {
		return err
	}
	server, found := e.tools.server(name)
	if !found {
		return editFail("工作区没有名为“%s”的 MCP 服务", name)
	}
	cfg := &target.Configuration
	if !selected {
		if !slices.Contains(cfg.MCPServerIDs, server.ID) {
			return editFail("成员“%s”没有 MCP 服务“%s”", cfg.DisplayName, server.Name)
		}
		cfg.MCPServerIDs = slices.DeleteFunc(slices.Clone(cfg.MCPServerIDs), func(id string) bool { return id == server.ID })
		e.note("移除成员的 MCP 服务：%s（%s）", cfg.DisplayName, server.Name)
		return nil
	}
	if !server.Usable {
		return editFail("MCP 服务“%s”未启用、不是 HTTP 服务或还没有连通检测成功，暂不能给成员使用", server.Name)
	}
	if slices.Contains(cfg.MCPServerIDs, server.ID) {
		return editFail("成员“%s”已有 MCP 服务“%s”", cfg.DisplayName, server.Name)
	}
	cfg.MCPServerIDs = append(slices.Clone(cfg.MCPServerIDs), server.ID)
	e.note("给成员添加 MCP 服务：%s（%s）", cfg.DisplayName, server.Name)
	return nil
}

// changeMemberLibrarySkill gives a built-in member a skill from the workspace
// skill library, pinned to its latest version when the draft is prepared.
func (e *editSession) changeMemberLibrarySkill(operation map[string]any) error {
	target, err := e.settingsTarget(operation, "技能库技能")
	if err != nil {
		return err
	}
	name, err := editText(operation, "skill", "技能库技能名称", 120)
	if err != nil {
		return err
	}
	selected, err := editSelected(operation)
	if err != nil {
		return err
	}
	cfg := &target.Configuration
	if !selected {
		if !slices.Contains(cfg.SkillNames, name) {
			return editFail("成员“%s”没有技能库技能“%s”", cfg.DisplayName, name)
		}
		cfg.SkillNames = slices.DeleteFunc(slices.Clone(cfg.SkillNames), func(item string) bool { return item == name })
		e.note("移除成员的技能库技能：%s（%s）", cfg.DisplayName, name)
		return nil
	}
	if _, found := e.tools.skill(name); !found {
		return editFail("技能库里没有名为“%s”的技能", name)
	}
	if slices.Contains(cfg.SkillNames, name) {
		return editFail("成员“%s”已有技能库技能“%s”", cfg.DisplayName, name)
	}
	for _, inline := range cfg.Skills {
		if strings.TrimSpace(inline.Name) == name {
			return editFail("成员“%s”已有同名技能“%s”，请先移除或改名", cfg.DisplayName, name)
		}
	}
	cfg.SkillNames = append(slices.Clone(cfg.SkillNames), name)
	e.note("给成员添加技能库技能：%s（%s）", cfg.DisplayName, name)
	return nil
}

// changeMemberToolRule adds or removes one rule on a built-in member's list of
// tools it may use without asking (allow) or must ask before using (ask).
func (e *editSession) changeMemberToolRule(operation map[string]any) error {
	target, err := e.settingsTarget(operation, "工具权限")
	if err != nil {
		return err
	}
	list := editString(operation["list"])
	if list != "allow" && list != "ask" {
		return editFail("list 须为 allow（允许直接使用）或 ask（使用前需确认）")
	}
	tool, err := editText(operation, "tool", "工具名称", 200)
	if err != nil {
		return err
	}
	selected, err := editSelected(operation)
	if err != nil {
		return err
	}
	cfg := &target.Configuration
	rules, label := &cfg.PermissionAllow, "允许直接使用"
	if list == "ask" {
		rules, label = &cfg.PermissionAsk, "使用前需确认"
	}
	if !selected {
		if !slices.Contains(*rules, tool) {
			return editFail("成员“%s”的“%s”里没有“%s”", cfg.DisplayName, label, tool)
		}
		*rules = slices.DeleteFunc(slices.Clone(*rules), func(item string) bool { return item == tool })
		e.note("从成员的“%s”中移除工具：%s（%s）", label, cfg.DisplayName, tool)
		return nil
	}
	if slices.Contains(*rules, tool) {
		return editFail("成员“%s”的“%s”已有“%s”", cfg.DisplayName, label, tool)
	}
	*rules = append(slices.Clone(*rules), tool)
	e.note("给成员的“%s”添加工具：%s（%s）", label, cfg.DisplayName, tool)
	return nil
}

// memberToolBindings turns a draft member's MCP services and library skills
// into the bindings of its new version. A service keeps the write gate: every
// tool not declared read-only is refused, in trials as in published work. A
// skill the member already had keeps its pinned version; a new one is pinned
// to the library's latest.
func memberToolBindings(ctx context.Context, tx pgx.Tx, ws string, cfg teamMemberAgentConfiguration, current registry.AgentRecord) ([]registry.MCPServerConfig, []registry.SkillRef, error) {
	servers := []registry.MCPServerConfig{}
	if len(cfg.MCPServerIDs) > 0 && engine.IsCLIEngine(cfg.Engine) && strings.TrimSpace(cfg.RuntimeID) == "" {
		return nil, nil, developmentError("成员“" + cfg.DisplayName + "”使用外部引擎并带有 MCP 服务，请先为它选择执行节点")
	}
	for _, id := range cfg.MCPServerIDs {
		preview, err := mcpregistry.ResolveCurrentMCPToolsTx(ctx, tx, ws, id, mcpregistry.MCPAgentPolicy{})
		if err != nil {
			return nil, nil, developmentError("成员“" + cfg.DisplayName + "”的一项 MCP 服务暂不可用，请检查服务是否启用并已连通")
		}
		writeTools := []string{}
		for _, tool := range preview.Tools {
			if !tool.ReadOnly {
				writeTools = append(writeTools, tool.Name)
			}
		}
		servers = append(servers, registry.MCPServerConfig{ServerID: id, WriteTools: writeTools})
	}
	skills := []registry.SkillRef{}
	for _, name := range cfg.SkillNames {
		if index := slices.IndexFunc(current.SkillRefs, func(ref registry.SkillRef) bool {
			return ref.Name == name && ref.SourceType == registry.SourceTypeRegistryVersion
		}); index >= 0 {
			skills = append(skills, current.SkillRefs[index])
			continue
		}
		var id string
		var version int64
		if err := tx.QueryRow(ctx, `SELECT id,latest_version FROM weave_skills WHERE workspace_id=$1 AND name=$2 AND latest_version>0`, ws, name).Scan(&id, &version); err != nil {
			return nil, nil, developmentError("成员“" + cfg.DisplayName + "”的技能库技能“" + name + "”已不存在")
		}
		skills = append(skills, registry.SkillRef{SourceType: registry.SourceTypeRegistryVersion, SkillID: id, SkillVersion: &version, Name: name})
	}
	return servers, skills, nil
}
