package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// Operations that are not part of a team's draft — archiving a team, runs,
// nodes, code repositories, Feishu and client sessions — offered to the desktop
// assistant and the MCP entry as one catalog. Each is a call to the route the
// console already uses, made as the caller, so that route's permissions,
// validation and audit are the ones that apply; nothing here decides anything
// a route does not.
//
// A value that must not pass through a conversation is never an operation
// field. The operation names which secrets it wants entered, and only an
// interactive caller — the desktop's own input field — supplies them; a secret
// an operation produces is handed back apart from the result for the same
// caller to show. The MCP entry is not interactive and is refused both.

const workspaceOperationsVersion = "1"

type workspaceSecret struct {
	Field string `json:"field"`
	Label string `json:"label"`
}

type workspaceReveal struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type workspaceOperation struct {
	Kind    string
	Summary string
	Example string
	// Roles the operation is open to; empty means anyone signed in.
	Roles []string
	// Confirm says what the operation does that cannot be taken back, and
	// Asks names it in a few words for whoever puts the question to the person.
	Confirm string
	Asks    string
	// Secrets the operation may ask the desktop to collect, named in "enter".
	Secrets []workspaceSecret
	// Forge marks an operation that needs the caller's own Forge sign-in.
	Forge bool
	// Reveals marks an operation whose outcome includes a secret to show once.
	Reveals bool
	// Starts marks an operation that starts work, limited like other starts.
	Starts bool
	Run    func(*workspaceRun) (any, *mcpEntryError)
}

type workspaceTopic struct {
	Topic      string
	Title      string
	Covers     string
	Rules      []string
	Operations []workspaceOperation
}

// workspaceRun is one operation being carried out for the caller.
type workspaceRun struct {
	call      *mcpEntryCall
	operation map[string]any
	secrets   map[string]string
	forge     string
	origin    string
	reveal    []workspaceReveal
}

var (
	workspaceDevelopers = []string{"developer", "admin", "owner"}
	workspaceAdmins     = []string{"admin", "owner"}
)

const workspaceIdentifierRule = "团队用 team（准确名称）或 team_id 指明，节点和代码仓库用列表中的准确名称；run_id、session_id 来自相应的列表，只用于调用，不要展示给使用者。"

func (run *workspaceRun) text(key string) string { return mcpText(run.operation, key) }

func (run *workspaceRun) need(keys ...string) *mcpEntryError {
	return mcpRequire(run.operation, keys...)
}

func (run *workspaceRun) path(format string, parts ...string) string {
	escaped := make([]any, len(parts))
	for i, part := range parts {
		escaped[i] = url.PathEscape(run.text(part))
	}
	return fmt.Sprintf(format, escaped...)
}

func workspaceDone(key string) map[string]any { return map[string]any{key: true} }

// subject names what the operation is about in the person's words, for the
// question put to them before something that cannot be taken back.
func (run *workspaceRun) subject() string {
	for _, key := range []string{"team", "node", "repository"} {
		if name := run.text(key); name != "" {
			return name
		}
	}
	if run.text("run_id") != "" {
		if task, failure := run.runTask(); failure == nil {
			return editFirst(task.Title, task.TeamName)
		}
	}
	if id := run.text("session_id"); id != "" {
		var listed struct {
			Sessions []map[string]any `json:"sessions"`
		}
		_ = run.call.route(http.MethodGet, "/v1/mcp/sessions", nil, &listed)
		for _, session := range listed.Sessions {
			if session["id"] == id {
				return editString(session["client_name"]) + "（" + editString(session["user_name"]) + "）"
			}
		}
	}
	return ""
}

// teamPath is a route of the team the operation names, by identifier or by
// its exact name among the teams the caller can see.
func (run *workspaceRun) teamPath(suffix string) (string, *mcpEntryError) {
	id, failure := run.teamID()
	return "/v1/teams/" + url.PathEscape(id) + suffix, failure
}

func (run *workspaceRun) teamID() (string, *mcpEntryError) {
	if id := run.text("team_id"); id != "" {
		return id, nil
	}
	name := run.text("team")
	if name == "" {
		return "", mcpFail("invalid_arguments", "缺少 team（团队名称）或 team_id")
	}
	teams, failure := run.call.teams(false)
	if failure != nil {
		return "", failure
	}
	found := ""
	for _, team := range teams {
		if editFirst(team.DisplayName, team.Name) != name {
			continue
		}
		if found != "" {
			return "", mcpFail("conflict", "有多个名为“%s”的团队，请先改名", name)
		}
		found = team.ID
	}
	if found == "" {
		return "", mcpFail("not_found", "没有名为“%s”的团队，或当前账号看不到它", name)
	}
	return found, nil
}

// named finds the one item of a list whose name is the given one.
func workspaceNamed(items []map[string]any, name, what string) (map[string]any, *mcpEntryError) {
	var found map[string]any
	for _, item := range items {
		if editString(item["name"]) != name || item["deleted_at"] != nil {
			continue
		}
		if found != nil {
			return nil, mcpFail("conflict", "有多个名为“%s”的%s，请先改名", name, what)
		}
		found = item
	}
	if found == nil {
		return nil, mcpFail("not_found", "没有名为“%s”的%s", name, what)
	}
	return found, nil
}

func (run *workspaceRun) nodes() ([]map[string]any, *mcpEntryError) {
	var listed struct {
		Runtimes []map[string]any `json:"runtimes"`
	}
	failure := run.call.route(http.MethodGet, "/v1/runtimes", nil, &listed)
	return listed.Runtimes, failure
}

// nodePath is the route of the node the operation names.
func (run *workspaceRun) nodePath(suffix string) (string, *mcpEntryError) {
	if failure := run.need("node"); failure != nil {
		return "", failure
	}
	nodes, failure := run.nodes()
	if failure != nil {
		return "", failure
	}
	node, failure := workspaceNamed(nodes, run.text("node"), "节点")
	if failure != nil {
		return "", failure
	}
	return "/v1/runtimes/" + url.PathEscape(editString(node["id"])) + suffix, nil
}

func (run *workspaceRun) nodeCall(method, suffix string, body any, done string) (any, *mcpEntryError) {
	path, failure := run.nodePath(suffix)
	if failure != nil {
		return nil, failure
	}
	if failure := run.call.route(method, path, body, nil); failure != nil {
		return nil, failure
	}
	return workspaceDone(done), nil
}

// revealToken hands a node's token back as the commands that use it.
func (run *workspaceRun) revealToken(token string) {
	run.reveal = append(run.reveal,
		workspaceReveal{"macOS 或 Linux 上的安装命令", fmt.Sprintf(`curl -fsSL "%[1]s/install.sh" | sh -s -- --server "%[1]s" --token '%[2]s'`, run.origin, token)},
		workspaceReveal{"Windows 上的安装命令", fmt.Sprintf(`& ([scriptblock]::Create((irm "%[1]s/install.ps1"))) -Server "%[1]s" -Token "%[2]s"`, run.origin, token)},
		workspaceReveal{"接入令牌", token})
}

func (run *workspaceRun) repositories() ([]map[string]any, *mcpEntryError) {
	var listed struct {
		Environments []map[string]any `json:"environments"`
	}
	failure := run.call.route(http.MethodGet, "/v1/environments", nil, &listed)
	return listed.Environments, failure
}

func (run *workspaceRun) repository(key string) (map[string]any, *mcpEntryError) {
	items, failure := run.repositories()
	if failure != nil {
		return nil, failure
	}
	return workspaceNamed(items, run.text(key), "代码仓库")
}

var workspaceRepositoryFields = []string{"name", "repository_url", "default_branch", "setup_script", "verify_commands", "push_branches", "default_team_id", "git_username"}

func workspaceRepositoryView(item map[string]any) map[string]any {
	view := map[string]any{"has_access_token": item["git_credential"] == true}
	for _, field := range workspaceRepositoryFields {
		view[field] = item[field]
	}
	return view
}

func (run *workspaceRun) feishuApp() (map[string]any, *mcpEntryError) {
	app := map[string]any{}
	failure := run.call.route(http.MethodGet, "/v1/integrations/feishu/app", nil, &app)
	return app, failure
}

func workspaceFeishuView(app map[string]any) map[string]any {
	delete(app, "revision")
	return app
}

// runTask confirms the caller may see the run the operation names, as the
// console's task list would show it.
func (run *workspaceRun) runTask() (adminTask, *mcpEntryError) {
	var task adminTask
	if failure := run.need("run_id"); failure != nil {
		return task, failure
	}
	if failure := run.call.route(http.MethodGet, run.path("/v1/admin/tasks/%s", "run_id"), nil, &task); failure != nil {
		return task, mcpFail("not_found", "没有这次运行，或当前账号不能查看")
	}
	return task, nil
}

func workspaceTaskView(task adminTask) map[string]any {
	view := map[string]any{"run_id": task.RunID, "title": task.Title, "team": task.TeamName, "status": task.Status, "source": task.Source, "started_by": task.Actor, "mine": task.Mine, "created_at": task.CreatedAt}
	if task.WaitKind != "" {
		view["waiting_for"] = task.WaitKind
	}
	if task.FailureReason != "" {
		view["failure"] = task.FailureReason
	}
	return view
}

var workspaceTopics = []workspaceTopic{
	{Topic: "team", Title: "团队与业务动作目录", Covers: "归档团队；从 Forge 重新读取业务动作目录", Rules: []string{workspaceIdentifierRule, "团队的成员、流程和步骤用受控修改办理，不在这里。"}, Operations: []workspaceOperation{
		{Kind: "team_archive", Summary: "归档一个团队", Example: `{"kind":"team_archive","team":"线索跟进团队"}`, Roles: workspaceDevelopers,
			Asks: "归档团队", Confirm: "归档后团队和它的流程停用，不再接新的工作，也不再出现在团队列表中。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				path, failure := run.teamPath("/development/team")
				if failure != nil {
					return nil, failure
				}
				if failure := run.call.route(http.MethodDelete, path, nil, nil); failure != nil {
					return nil, failure
				}
				return workspaceDone("archived"), nil
			}},
		{Kind: "catalog_refresh", Summary: "用当前登录人的 Forge 账号重新读取业务动作目录；Forge 的业务动作有增减或改名后办理", Example: `{"kind":"catalog_refresh"}`, Roles: workspaceDevelopers, Forge: true,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				var view businessCapabilitiesView
				if failure := run.call.route(http.MethodPost, "/v1/development/business-capabilities/refresh", map[string]any{"forge_token": run.forge}, &view); failure != nil {
					return nil, failure
				}
				return map[string]any{"refreshed": view.Available, "business_actions": len(view.Capabilities), "fetched_at": view.FetchedAt}, nil
			}},
	}},
	{Topic: "run", Title: "运行", Covers: "查看运行列表和一次运行的进展、结果、日志与代码结果；停止运行；追加要求；发起代码任务；创建拉取请求", Rules: []string{workspaceIdentifierRule, "开发者看到本人发起的运行，管理员看到本组织的全部运行。"}, Operations: []workspaceOperation{
		{Kind: "run_list", Summary: "列出运行，新的在前。filter 可为 active（进行中）、attention（需要处理）、done（已结束），省略为全部；可用 team、source（console、desktop、feishu、schedule）筛选；limit 至多 50；before 接上一页返回的 more_before", Example: `{"kind":"run_list","filter":"attention","limit":20}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				query := url.Values{}
				limit := int(editNumber(run.operation["limit"]))
				if limit < 1 || limit > 50 {
					limit = 20
				}
				query.Set("limit", strconv.Itoa(limit))
				for _, key := range []string{"filter", "source", "before"} {
					if value := run.text(key); value != "" {
						query.Set(key, value)
					}
				}
				if run.text("team") != "" || run.text("team_id") != "" {
					id, failure := run.teamID()
					if failure != nil {
						return nil, failure
					}
					query.Set("team", id)
				}
				var listed struct {
					Tasks []adminTask `json:"tasks"`
					Next  string      `json:"next_before"`
				}
				if failure := run.call.route(http.MethodGet, "/v1/admin/tasks?"+query.Encode(), nil, &listed); failure != nil {
					return nil, failure
				}
				runs := []map[string]any{}
				for _, task := range listed.Tasks {
					runs = append(runs, workspaceTaskView(task))
				}
				return map[string]any{"runs": runs, "more_before": listed.Next}, nil
			}},
		{Kind: "run_read", Summary: "读取一次运行：状态、每一步的结果、最终结果和自动检查结论", Example: `{"kind":"run_read","run_id":"…"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				task, failure := run.runTask()
				if failure != nil {
					return nil, failure
				}
				view, failure := run.call.runView(task.RunID, true)
				if failure != nil {
					return nil, failure
				}
				for key, value := range workspaceTaskView(task) {
					if _, set := view[key]; !set {
						view[key] = value
					}
				}
				return view, nil
			}},
		{Kind: "run_logs", Summary: "读取一次运行的输出日志，至多 200 行；search 按文字筛选，after 接上次返回的 last 继续读", Example: `{"kind":"run_logs","run_id":"…","search":"error"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if _, failure := run.runTask(); failure != nil {
					return nil, failure
				}
				query := url.Values{"limit": {"200"}}
				if search := run.text("search"); search != "" {
					query.Set("q", search)
				}
				if after := int64(editNumber(run.operation["after"])); after > 0 {
					query.Set("after", strconv.FormatInt(after, 10))
				}
				var logs struct {
					Lines []struct {
						Stream string `json:"stream"`
						Seq    int64  `json:"seq"`
						At     string `json:"occurred_at"`
						Text   string `json:"text"`
					} `json:"lines"`
					Complete bool `json:"complete"`
				}
				if failure := run.call.route(http.MethodGet, run.path("/v1/admin/tasks/%s/logs", "run_id")+"?"+query.Encode(), nil, &logs); failure != nil {
					return nil, failure
				}
				lines, size, last := []map[string]any{}, 0, int64(0)
				for _, line := range logs.Lines {
					if size += len(line.Text); size > mcpContentLimit {
						logs.Complete = false
						break
					}
					lines, last = append(lines, map[string]any{"at": line.At, "stream": line.Stream, "text": line.Text}), line.Seq
				}
				return map[string]any{"lines": lines, "last": last, "complete": logs.Complete}, nil
			}},
		{Kind: "run_code", Summary: "读取代码任务的结果：仓库与分支、改动的文件、验证结论和拉取请求", Example: `{"kind":"run_code","run_id":"…"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("run_id"); failure != nil {
					return nil, failure
				}
				var code struct {
					Repository string         `json:"repository"`
					Ref        string         `json:"ref"`
					PushBranch string         `json:"push_branch"`
					Commands   []string       `json:"verify_commands"`
					Verdict    map[string]any `json:"verdict"`
					Pull       map[string]any `json:"pull_request"`
					Final      *struct {
						Version struct {
							Changed bool             `json:"changed"`
							Files   []map[string]any `json:"files"`
						} `json:"version"`
					} `json:"final"`
				}
				if failure := run.call.route(http.MethodGet, run.path("/v1/admin/tasks/%s/code", "run_id"), nil, &code); failure != nil {
					return nil, failure
				}
				view := map[string]any{"repository": code.Repository, "ref": code.Ref, "push_branch": code.PushBranch, "verify_commands": code.Commands, "verdict": code.Verdict, "pull_request": code.Pull}
				if code.Final != nil {
					view["changed"], view["files"] = code.Final.Version.Changed, code.Final.Version.Files
				}
				return view, nil
			}},
		{Kind: "run_stop", Summary: "请求停止一次还在进行的运行", Example: `{"kind":"run_stop","run_id":"…"}`, Roles: workspaceDevelopers,
			Asks: "停止运行", Confirm: "停止后这次运行不再继续，已经完成的步骤保留。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("run_id"); failure != nil {
					return nil, failure
				}
				if failure := run.call.route(http.MethodPost, run.path("/v1/runs/%s/stop", "run_id"), map[string]any{"idempotency_key": "workspace-stop-" + run.text("run_id")}, nil); failure != nil {
					return nil, failure
				}
				return workspaceDone("stop_requested"), nil
			}},
		{Kind: "run_follow_up", Summary: "在一次已结束的运行之后追加要求，由同一团队接着做；request_id 是本次请求的 UUID，结果不确定时用同一个值重试", Example: `{"kind":"run_follow_up","run_id":"…","request_id":"…","task":"把结论改写成给客户的邮件"}`, Roles: workspaceDevelopers, Starts: true,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("run_id", "request_id", "task"); failure != nil {
					return nil, failure
				}
				var accepted struct {
					RunID string `json:"run_id"`
				}
				body := map[string]any{"task": run.text("task"), "client_request_id": run.text("request_id")}
				if failure := run.call.route(http.MethodPost, run.path("/v1/admin/tasks/%s/follow-ups", "run_id"), body, &accepted); failure != nil {
					return nil, failure
				}
				return map[string]any{"accepted": true, "run_id": accepted.RunID}, nil
			}},
		{Kind: "code_task_submit", Summary: "把一件代码任务交给团队。repository 是代码仓库名称，省略表示不带仓库；ref 是分支或提交，省略用仓库的默认分支；request_id 同上", Example: `{"kind":"code_task_submit","team":"订单研发团队","repository":"订单服务","request_id":"…","task":"修复金额四舍五入的错误并补上测试"}`, Roles: workspaceDevelopers, Starts: true,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("request_id", "task"); failure != nil {
					return nil, failure
				}
				teamID, failure := run.teamID()
				if failure != nil {
					return nil, failure
				}
				body := map[string]any{"team_id": teamID, "environment_id": "", "ref": run.text("ref"), "task": run.text("task"), "client_request_id": run.text("request_id")}
				if run.text("repository") != "" {
					repository, failure := run.repository("repository")
					if failure != nil {
						return nil, failure
					}
					body["environment_id"] = repository["id"]
				}
				var accepted struct {
					RunID string `json:"run_id"`
				}
				if failure := run.call.route(http.MethodPost, "/v1/admin/tasks", body, &accepted); failure != nil {
					return nil, failure
				}
				return map[string]any{"accepted": true, "run_id": accepted.RunID}, nil
			}},
		{Kind: "run_pull_request", Summary: "为一次验证通过的代码任务创建拉取请求", Example: `{"kind":"run_pull_request","run_id":"…"}`, Roles: workspaceDevelopers,
			Asks: "创建拉取请求", Confirm: "会在代码仓库里创建一个拉取请求，仓库的成员都能看到。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("run_id"); failure != nil {
					return nil, failure
				}
				var pull map[string]any
				failure := run.call.route(http.MethodPost, run.path("/v1/admin/tasks/%s/pull-request", "run_id"), map[string]any{}, &pull)
				return pull, failure
			}},
	}},
	{Topic: "node", Title: "执行节点", Covers: "查看节点；接入新节点；改名、暂停或恢复、重新检测、更换接入令牌、移除", Rules: []string{workspaceIdentifierRule, "查看对所有人开放，其余限管理员。", "接入令牌和带令牌的安装命令只由桌面显示给使用者，不会出现在结果里；不要向使用者索要或复述令牌。"}, Operations: []workspaceOperation{
		{Kind: "node_list", Summary: "列出节点：是否在线、能否接任务、提供哪些引擎及各自是否可用", Example: `{"kind":"node_list"}`,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				nodes, failure := run.nodes()
				if failure != nil {
					return nil, failure
				}
				views := []map[string]any{}
				for _, node := range nodes {
					if node["deleted_at"] != nil {
						continue
					}
					view := map[string]any{"engines": node["engine_readiness"]}
					for _, key := range []string{"name", "online", "accepting", "paused", "health_status", "total_slots", "active_slots", "last_heartbeat_at", "last_failure_reason"} {
						view[key] = node[key]
					}
					views = append(views, view)
				}
				return map[string]any{"nodes": views}, nil
			}},
		{Kind: "node_add", Summary: "登记一个新节点。完成后桌面向使用者显示在那台机器上运行的安装命令", Example: `{"kind":"node_add","name":"研发一号机"}`, Roles: workspaceAdmins, Reveals: true,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("name"); failure != nil {
					return nil, failure
				}
				var created struct {
					Name  string `json:"name"`
					Token string `json:"token"`
				}
				if failure := run.call.route(http.MethodPost, "/v1/runtimes", map[string]any{"name": run.text("name")}, &created); failure != nil {
					return nil, failure
				}
				run.revealToken(created.Token)
				return map[string]any{"added": created.Name, "next": "安装命令已由桌面显示给使用者；在那台机器上运行后节点才会上线"}, nil
			}},
		{Kind: "node_rename", Summary: "给节点改名", Example: `{"kind":"node_rename","node":"研发一号机","name":"研发主机"}`, Roles: workspaceAdmins,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("name"); failure != nil {
					return nil, failure
				}
				return run.nodeCall(http.MethodPut, "", map[string]any{"name": run.text("name")}, "renamed")
			}},
		{Kind: "node_pause", Summary: "paused 为 true 时暂停节点接新任务，为 false 时恢复；正在做的任务不受影响", Example: `{"kind":"node_pause","node":"研发一号机","paused":true}`, Roles: workspaceAdmins,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				paused, isBool := run.operation["paused"].(bool)
				if !isBool {
					return nil, mcpFail("invalid_arguments", "paused 须为 true 或 false")
				}
				return run.nodeCall(http.MethodPut, "/paused", map[string]any{"paused": paused}, "saved")
			}},
		{Kind: "node_probe", Summary: "让节点重新检测它上面的引擎；结果稍后在 node_list 中更新", Example: `{"kind":"node_probe","node":"研发一号机"}`, Roles: workspaceAdmins,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				return run.nodeCall(http.MethodPost, "/probe", map[string]any{}, "probe_requested")
			}},
		{Kind: "node_token", Summary: "更换节点的接入令牌。完成后桌面向使用者显示新的安装命令", Example: `{"kind":"node_token","node":"研发一号机"}`, Roles: workspaceAdmins, Reveals: true,
			Asks: "更换节点的接入令牌", Confirm: "原来的接入令牌立即失效，那台机器要用新令牌重新接入后才能继续接任务。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				path, failure := run.nodePath("/token")
				if failure != nil {
					return nil, failure
				}
				var rotated struct {
					Token string `json:"token"`
				}
				if failure := run.call.route(http.MethodPost, path, map[string]any{}, &rotated); failure != nil {
					return nil, failure
				}
				run.revealToken(rotated.Token)
				return map[string]any{"replaced": true, "next": "新的安装命令已由桌面显示给使用者"}, nil
			}},
		{Kind: "node_remove", Summary: "移除节点", Example: `{"kind":"node_remove","node":"研发一号机"}`, Roles: workspaceAdmins,
			Asks: "移除节点", Confirm: "移除后节点不再接任务，分配在它上面的成员需要改用别的节点。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				return run.nodeCall(http.MethodDelete, "", nil, "removed")
			}},
	}},
	{Topic: "repository", Title: "代码仓库", Covers: "查看、新增、修改和归档代码任务使用的代码仓库", Rules: []string{workspaceIdentifierRule, "访问令牌只由桌面的输入框收取：在 enter 中写 git_token，桌面会请使用者填写；不要向使用者索要令牌，对话里出现的令牌不使用、不复述。"}, Operations: []workspaceOperation{
		{Kind: "repository_list", Summary: "列出代码仓库及其默认分支、启动脚本、验证命令和是否已保存访问令牌", Example: `{"kind":"repository_list"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				items, failure := run.repositories()
				if failure != nil {
					return nil, failure
				}
				views := []map[string]any{}
				for _, item := range items {
					views = append(views, workspaceRepositoryView(item))
				}
				return map[string]any{"repositories": views}, nil
			}},
		{Kind: "repository_save", Summary: "新增或修改代码仓库。修改时用 repository 指出现有名称，只写要改的字段；新增时省略 repository，并给 name、repository_url。可写字段：name、repository_url、default_branch、setup_script、verify_commands（字符串数组）、push_branches（是否推送分支）、default_team_id、git_username。要保存或更换访问令牌时写 \"enter\":[\"git_token\"]；clear_token 为 true 时清除已保存的令牌", Example: `{"kind":"repository_save","name":"订单服务","repository_url":"https://git.example.com/shop/orders.git","default_branch":"main","verify_commands":["make test"],"enter":["git_token"]}`, Roles: workspaceDevelopers,
			Secrets: []workspaceSecret{{"git_token", "代码仓库的访问令牌"}},
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				body, method, path := map[string]any{}, http.MethodPost, "/v1/environments"
				if run.text("repository") != "" {
					current, failure := run.repository("repository")
					if failure != nil {
						return nil, failure
					}
					for _, field := range workspaceRepositoryFields {
						body[field] = current[field]
					}
					method, path = http.MethodPut, "/v1/environments/"+url.PathEscape(editString(current["id"]))
				}
				for _, field := range workspaceRepositoryFields {
					if value, given := run.operation[field]; given {
						body[field] = value
					}
				}
				if token, entered := run.secrets["git_token"]; entered {
					body["git_token"] = token
				} else if run.operation["clear_token"] == true {
					body["git_token"] = ""
				}
				saved := map[string]any{}
				if failure := run.call.route(method, path, body, &saved); failure != nil {
					return nil, failure
				}
				return workspaceRepositoryView(saved), nil
			}},
		{Kind: "repository_archive", Summary: "归档一个代码仓库", Example: `{"kind":"repository_archive","repository":"订单服务"}`, Roles: workspaceDevelopers,
			Asks: "归档代码仓库", Confirm: "归档后不能再用这个代码仓库发起代码任务，已保存的访问令牌一并失效。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("repository"); failure != nil {
					return nil, failure
				}
				current, failure := run.repository("repository")
				if failure != nil {
					return nil, failure
				}
				if failure := run.call.route(http.MethodDelete, "/v1/environments/"+url.PathEscape(editString(current["id"])), nil, nil); failure != nil {
					return nil, failure
				}
				return workspaceDone("archived"), nil
			}},
	}},
	{Topic: "feishu", Title: "飞书", Covers: "查看、保存、检测和移除本组织的飞书应用；设置某个团队是否经飞书接工作和发通知", Rules: []string{workspaceIdentifierRule, "查看和团队设置对开发者开放，保存、检测和移除应用限管理员。", "应用密钥、校验令牌和加密密钥只由桌面的输入框收取：在 enter 中写出要填写的项，桌面会请使用者逐项填写；不要向使用者索要这些值，对话里出现的不使用、不复述。"}, Operations: []workspaceOperation{
		{Kind: "feishu_read", Summary: "读取飞书应用：来源、应用标识、租户标识、哪些密钥已保存、回调地址、最近一次检测和已绑定的员工数", Example: `{"kind":"feishu_read"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				app, failure := run.feishuApp()
				if failure != nil {
					return nil, failure
				}
				return workspaceFeishuView(app), nil
			}},
		{Kind: "feishu_save", Summary: "保存本组织的飞书应用。app_id、tenant_key 省略时沿用已保存的；enter 列出要填写或更换的密钥：app_secret（应用密钥）、verification_token（校验令牌）、encrypt_key（加密密钥），首次保存须填写前两项", Example: `{"kind":"feishu_save","app_id":"cli_a1b2c3","tenant_key":"2ed263bf32cf1651","enter":["app_secret","verification_token"]}`, Roles: workspaceAdmins,
			Secrets: []workspaceSecret{{"app_secret", "飞书应用密钥"}, {"verification_token", "飞书事件校验令牌"}, {"encrypt_key", "飞书事件加密密钥"}},
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				current, failure := run.feishuApp()
				if failure != nil {
					return nil, failure
				}
				body := map[string]any{"expectedRevision": current["revision"], "appId": editFirst(run.text("app_id"), editString(current["appId"])), "tenantKey": editFirst(run.text("tenant_key"), editString(current["tenantKey"]))}
				if current["source"] != "workspace" {
					body["expectedRevision"] = 0
				}
				for field, name := range map[string]string{"app_secret": "appSecret", "verification_token": "verificationToken", "encrypt_key": "encryptKey"} {
					if value, entered := run.secrets[field]; entered {
						body[name] = value
					}
				}
				saved := map[string]any{}
				if failure := run.call.route(http.MethodPut, "/v1/integrations/feishu/app", body, &saved); failure != nil {
					return nil, failure
				}
				return workspaceFeishuView(saved), nil
			}},
		{Kind: "feishu_check", Summary: "用已保存的凭据向飞书检测一次连接", Example: `{"kind":"feishu_check"}`, Roles: workspaceAdmins,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				var result map[string]any
				failure := run.call.route(http.MethodPost, "/v1/integrations/feishu/app/check", map[string]any{}, &result)
				return result, failure
			}},
		{Kind: "feishu_remove", Summary: "移除本组织保存的飞书应用", Example: `{"kind":"feishu_remove"}`, Roles: workspaceAdmins,
			Asks: "移除飞书应用", Confirm: "移除后员工不能再经飞书交接工作和接收通知，已保存的密钥一并删除。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				current, failure := run.feishuApp()
				if failure != nil {
					return nil, failure
				}
				if failure := run.call.route(http.MethodDelete, "/v1/integrations/feishu/app", map[string]any{"expectedRevision": current["revision"]}, nil); failure != nil {
					return nil, failure
				}
				return workspaceDone("removed"), nil
			}},
		{Kind: "feishu_team_read", Summary: "读取一个团队的飞书设置：是否经飞书接工作、使用哪条流程、哪些情况发通知", Example: `{"kind":"feishu_team_read","team":"线索跟进团队"}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				path, failure := run.teamPath("/feishu-access")
				if failure != nil {
					return nil, failure
				}
				access := map[string]any{}
				if failure := run.call.route(http.MethodGet, path, nil, &access); failure != nil {
					return nil, failure
				}
				return workspaceFeishuView(access), nil
			}},
		{Kind: "feishu_team_save", Summary: "修改一个团队的飞书设置，只写要改的项：enabled；workflow（流程的准确名称）或 workflow_id（空字符串表示用默认流程）；notify 中的 result（完成）、revisionRequired（需要补充）、humanReview（需要人工确认）、failure（失败）、cancelled（取消）", Example: `{"kind":"feishu_team_save","team":"线索跟进团队","enabled":true,"notify":{"result":true,"failure":true}}`, Roles: workspaceDevelopers,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				path, failure := run.teamPath("/feishu-access")
				if failure != nil {
					return nil, failure
				}
				current := map[string]any{}
				if failure := run.call.route(http.MethodGet, path, nil, &current); failure != nil {
					return nil, failure
				}
				notify := editObject(current["notify"])
				if notify == nil {
					notify = map[string]any{}
				}
				for key, value := range editObject(run.operation["notify"]) {
					notify[key] = value
				}
				body := map[string]any{"expectedRevision": current["revision"], "enabled": current["enabled"], "workflowId": current["workflowId"], "notify": notify}
				if value, given := run.operation["enabled"]; given {
					body["enabled"] = value
				}
				if value, given := run.operation["workflow_id"]; given {
					body["workflowId"] = value
				}
				if name := run.text("workflow"); name != "" {
					var listed struct {
						Workflows []map[string]any `json:"workflows"`
					}
					if failure := run.call.route(http.MethodGet, strings.TrimSuffix(path, "/feishu-access")+"/workflows", nil, &listed); failure != nil {
						return nil, failure
					}
					flow, failure := workspaceNamed(listed.Workflows, name, "流程")
					if failure != nil {
						return nil, failure
					}
					body["workflowId"] = flow["id"]
				}
				saved := map[string]any{}
				if failure := run.call.route(http.MethodPut, path, body, &saved); failure != nil {
					return nil, failure
				}
				return workspaceFeishuView(saved), nil
			}},
	}},
	{Topic: "client_session", Title: "客户端会话", Covers: "查看用本人账号登录 Weave 的外部客户端会话；收回一个会话", Rules: []string{workspaceIdentifierRule, "每个人看到并能收回自己的会话，管理员看到并能收回本组织的全部会话。"}, Operations: []workspaceOperation{
		{Kind: "client_session_list", Summary: "列出外部客户端的登录会话：客户端名称、账号、登录和最近使用时间、到期时间", Example: `{"kind":"client_session_list"}`,
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				var listed struct {
					Sessions []map[string]any `json:"sessions"`
				}
				if failure := run.call.route(http.MethodGet, "/v1/mcp/sessions", nil, &listed); failure != nil {
					return nil, failure
				}
				views := []map[string]any{}
				for _, session := range listed.Sessions {
					views = append(views, map[string]any{"session_id": session["id"], "client": session["client_name"], "account": session["user_name"], "mine": session["own"], "created_at": session["created_at"], "last_used_at": session["last_used_at"], "expires_at": session["expires_at"]})
				}
				return map[string]any{"sessions": views}, nil
			}},
		{Kind: "client_session_revoke", Summary: "收回一个客户端会话", Example: `{"kind":"client_session_revoke","session_id":"…"}`,
			Asks: "收回客户端会话", Confirm: "收回后那个客户端立即不能再用这次登录访问 Weave，需要重新登录。",
			Run: func(run *workspaceRun) (any, *mcpEntryError) {
				if failure := run.need("session_id"); failure != nil {
					return nil, failure
				}
				if failure := run.call.route(http.MethodDelete, run.path("/v1/mcp/sessions/%s", "session_id"), nil, nil); failure != nil {
					return nil, failure
				}
				return workspaceDone("revoked"), nil
			}},
	}},
}

func workspaceOperationOf(kind string) *workspaceOperation {
	for i := range workspaceTopics {
		for j := range workspaceTopics[i].Operations {
			if workspaceTopics[i].Operations[j].Kind == kind {
				return &workspaceTopics[i].Operations[j]
			}
		}
	}
	return nil
}

// workspaceStarts reports whether an operation starts work.
func workspaceStarts(operation map[string]any) bool {
	found := workspaceOperationOf(editString(operation["kind"]))
	return found != nil && found.Starts
}

func (operation *workspaceOperation) open(role string) bool {
	return len(operation.Roles) == 0 || editOneOf(role, operation.Roles...)
}

func (s *Server) handleListWorkspaceOperations(c echo.Context) error {
	topics := make([]map[string]string, len(workspaceTopics))
	for i, topic := range workspaceTopics {
		topics[i] = map[string]string{"topic": topic.Topic, "title": topic.Title, "covers": topic.Covers}
	}
	return c.JSON(http.StatusOK, map[string]any{"version": workspaceOperationsVersion, "topics": topics})
}

func (s *Server) handleGetWorkspaceOperationTopic(c echo.Context) error {
	for _, topic := range workspaceTopics {
		if topic.Topic != c.Param("topic") {
			continue
		}
		operations := []map[string]any{}
		for i := range topic.Operations {
			operation := &topic.Operations[i]
			doc := map[string]any{"kind": operation.Kind, "summary": operation.Summary, "example": operation.Example, "allowed": operation.open(firstRole(c))}
			if operation.Confirm != "" {
				doc["confirm"] = operation.Confirm
			}
			if len(operation.Secrets) > 0 {
				doc["secrets"] = operation.Secrets
			}
			if operation.Forge || operation.Reveals {
				doc["desktop_only"] = true
			}
			operations = append(operations, doc)
		}
		return c.JSON(http.StatusOK, map[string]any{"topic": topic.Topic, "title": topic.Title, "covers": topic.Covers, "operations": operations, "rules": topic.Rules})
	}
	return workflowError(c, http.StatusNotFound, "operation_topic_not_found", "没有这一类操作")
}

func workspaceStatus(failure *mcpEntryError) int {
	if failure.Status >= 400 {
		return failure.Status
	}
	if status, known := map[string]int{"forbidden": http.StatusForbidden, "not_found": http.StatusNotFound, "conflict": http.StatusConflict}[failure.Code]; known {
		return status
	}
	return http.StatusUnprocessableEntity
}

// handleApplyWorkspaceOperation carries out one operation, or says what it
// still needs first: the person's confirmation, or values that only an
// interactive caller can supply.
func (s *Server) handleApplyWorkspaceOperation(c echo.Context) error {
	var request struct {
		Operation   map[string]any    `json:"operation"`
		Confirmed   bool              `json:"confirmed"`
		Interactive bool              `json:"interactive"`
		Secrets     map[string]string `json:"secrets"`
		ForgeToken  string            `json:"forge_token"`
	}
	if err := c.Bind(&request); err != nil || request.Operation == nil {
		return workflowError(c, http.StatusUnprocessableEntity, "operation_invalid", "须提供一项操作")
	}
	operation := workspaceOperationOf(editString(request.Operation["kind"]))
	if operation == nil {
		return workflowError(c, http.StatusUnprocessableEntity, "operation_unknown", "没有这项操作，请先读取工作区操作的类别清单和写法")
	}
	if !operation.open(firstRole(c)) {
		return workflowError(c, http.StatusForbidden, "forbidden", "当前账号没有办理这项操作的权限")
	}
	enter := []string{}
	if raw, given := request.Operation["enter"]; given {
		names, valid := editStringList(raw)
		for _, name := range names {
			known := false
			for _, secret := range operation.Secrets {
				known = known || secret.Field == name
			}
			valid = valid && known
		}
		if !valid {
			return workflowError(c, http.StatusUnprocessableEntity, "operation_invalid", "enter 只能列出这项操作说明里的密钥项")
		}
		enter = names
	}
	if (operation.Forge || operation.Reveals || len(enter) > 0) && !request.Interactive {
		return workflowError(c, http.StatusConflict, "desktop_only", "这项操作要填写或显示密钥，或要用到本人的 Forge 登录，只能在桌面或 Weave 管理端办理")
	}
	run := &workspaceRun{call: s.entryCall(c), operation: request.Operation, secrets: map[string]string{}, forge: strings.TrimSpace(request.ForgeToken), origin: requestOrigin(c.Request())}
	if operation.Confirm != "" && !request.Confirmed {
		return c.JSON(http.StatusOK, map[string]any{"status": "confirmation_required", "title": operation.Asks, "subject": run.subject(), "message": operation.Confirm})
	}
	missing := []workspaceSecret{}
	for _, secret := range operation.Secrets {
		if !editOneOf(secret.Field, enter...) {
			continue
		}
		if value := request.Secrets[secret.Field]; strings.TrimSpace(value) != "" {
			run.secrets[secret.Field] = value
		} else {
			missing = append(missing, secret)
		}
	}
	if needsForge := operation.Forge && run.forge == ""; len(missing) > 0 || needsForge {
		return c.JSON(http.StatusOK, map[string]any{"status": "needs_input", "secrets": missing, "forge_session": needsForge})
	}
	result, failure := operation.Run(run)
	if failure != nil {
		return c.JSON(workspaceStatus(failure), map[string]any{"code": failure.Code, "message": failure.Message})
	}
	response := map[string]any{"status": "done", "result": result}
	if len(run.reveal) > 0 {
		response["reveal"] = run.reveal
	}
	return c.JSON(http.StatusOK, response)
}
