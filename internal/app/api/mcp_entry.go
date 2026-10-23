package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
)

// The MCP entry lets an outside agent develop and use teams. It adds no
// behaviour of its own: every tool is one or two calls to the routes the
// console and the desktop already use, made with the caller's own credential,
// so validation, permissions and audit are the ones those routes apply.

const mcpEntryInstructions = "Weave 的团队开发与使用工具。修改团队前先用 weave_development_read 读取草稿和可修改的类别，再用 weave_development_guide 读取要用到的那一类写法；不要猜测字段。使用者要求出错时要按规定处理时，用 guarantee 类别设置失败分支，不要只写进成员的工作要求。团队草稿以外的操作（归档团队、运行、执行节点、飞书、客户端会话）用 weave_workspace_guide 读取类别和写法，再用 weave_workspace_apply 办理；要填写或显示密钥的操作不经这里办理。经这里交接的工作不带业务动作授权，需要办理业务的部分由员工在桌面办理。团队和流程的标识只用于调用，不要展示给使用者。设备工具（device_*）只对本人账号会话开放，用来查看和操作使用者本人绑定的 Mac 与 Windows：先 device_list，再 device_session_open，ready 后截图或读取界面结构再操作；会改变机器的操作带 op_id，结果不确定时用同一个 op_id 重试。不得用设备工具在 Forge 或小满界面中办理业务写入。"

const (
	mcpMaterialLimit = 200 << 10
	mcpContentLimit  = 64 << 10
)

type mcpEntryTool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(*mcpEntryCall, map[string]any) (any, *mcpEntryError)
}

type mcpEntryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Status is the status of the route that refused, when one did.
	Status int `json:"-"`
}

func mcpFail(code, format string, args ...any) *mcpEntryError {
	return &mcpEntryError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// mcpEntryCall is one tool call made on behalf of the authenticated caller.
type mcpEntryCall struct {
	server *Server
	ctx    context.Context
	auth   string
	remote string
	host   string
	secure bool
	tenant string
	user   string
	role   string
	// reader may read every run of the workspace, as in the console.
	reader bool
}

// submitted reports whether the caller may read a piece of handed-over work:
// its submitter, or an administrator or owner. Nobody else learns it exists.
func (call *mcpEntryCall) submitted(runID string) (mine bool, failure *mcpEntryError) {
	pool := call.server.GetPool()
	if pool == nil {
		return false, mcpFail("failed", "服务暂不可用")
	}
	var actor string
	err := pool.QueryRow(call.ctx, `SELECT COALESCE(actor_subject->>'user_id','') FROM weave_workflow_admission_requests WHERE workspace_id=$1 AND request->>'run_id'=$2`, call.tenant, runID).Scan(&actor)
	if err != nil || actor != call.user && !call.reader {
		return false, mcpFail("not_found", "没有这件工作")
	}
	return actor == call.user, nil
}

type mcpCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *mcpCapture) Header() http.Header         { return w.header }
func (w *mcpCapture) WriteHeader(status int)      { w.status = status }
func (w *mcpCapture) Write(b []byte) (int, error) { return w.body.Write(b) }

// route calls one of Weave's own routes as the caller and decodes its JSON.
func (call *mcpEntryCall) route(method, path string, body, into any) *mcpEntryError {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return mcpFail("invalid_arguments", "参数无法编码")
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(call.ctx, method, path, reader)
	if err != nil {
		return mcpFail("invalid_arguments", "参数无效")
	}
	request.Header.Set(echo.HeaderAuthorization, call.auth)
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	request.RemoteAddr = call.remote
	// Routes that hand out commands name this server by the address the caller used.
	request.Host = call.host
	if call.secure {
		request.Header.Set("X-Forwarded-Proto", "https")
	}
	capture := &mcpCapture{header: http.Header{}, status: http.StatusOK}
	call.server.Echo.ServeHTTP(capture, request)
	if capture.status >= 400 {
		var failure struct {
			Code    string `json:"code"`
			Error   any    `json:"error"`
			Message string `json:"message"`
			Index   *int   `json:"index"`
		}
		_ = json.Unmarshal(capture.body.Bytes(), &failure)
		message := failure.Message
		if text, isText := failure.Error.(string); isText && text != "" {
			message = text
		}
		code := failure.Code
		if code == "" {
			code = map[int]string{401: "unauthorized", 403: "forbidden", 404: "not_found", 409: "conflict", 422: "invalid"}[capture.status]
		}
		if code == "" {
			code = "failed"
		}
		if message == "" {
			message = http.StatusText(capture.status)
		}
		if failure.Index != nil {
			message = fmt.Sprintf("第 %d 项：%s", *failure.Index+1, message)
		}
		return &mcpEntryError{Code: code, Message: message, Status: capture.status}
	}
	if into != nil && capture.body.Len() > 0 {
		if err := json.Unmarshal(capture.body.Bytes(), into); err != nil {
			return mcpFail("failed", "服务端返回了无法读取的结果")
		}
	}
	return nil
}

func mcpText(args map[string]any, key string) string {
	text, _ := args[key].(string)
	return strings.TrimSpace(text)
}

func mcpRequire(args map[string]any, keys ...string) *mcpEntryError {
	for _, key := range keys {
		if mcpText(args, key) == "" {
			return mcpFail("invalid_arguments", "缺少 %s", key)
		}
	}
	return nil
}

func mcpClip(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	return strings.ToValidUTF8(text[:limit], ""), true
}

// mcpTerms splits text into the units team search compares: words for Latin
// text and overlapping pairs for Chinese, the same way the desktop does.
func mcpTerms(text string) map[string]bool {
	terms := map[string]bool{}
	var word []rune
	han := false
	flush := func() {
		if len(word) == 0 {
			return
		}
		if han && len(word) > 1 {
			for i := 0; i+1 < len(word); i++ {
				terms[string(word[i:i+2])] = true
			}
		} else {
			terms[string(word)] = true
		}
		word = word[:0]
	}
	for _, r := range strings.ToLower(text) {
		isHan := unicode.Is(unicode.Han, r)
		isWord := r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
		if !isHan && !isWord || len(word) > 0 && isHan != han {
			flush()
		}
		if isHan || isWord {
			han = isHan
			word = append(word, r)
		}
	}
	flush()
	return terms
}

type mcpTeam struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Objective   string `json:"objective"`
	Workflow    string `json:"default_workflow_id"`
}

func (team mcpTeam) view() map[string]any {
	return map[string]any{"team_id": team.ID, "name": editFirst(team.DisplayName, team.Name), "objective": team.Objective, "published": team.Workflow != ""}
}

func (call *mcpEntryCall) teams(development bool) ([]mcpTeam, *mcpEntryError) {
	if development && !roleAtLeast(call.role, roleDeveloper) {
		return nil, mcpFail("forbidden", "当前账号没有开发团队的权限")
	}
	var teams []mcpTeam
	if failure := call.route(http.MethodGet, "/v1/teams?status=active", nil, &teams); failure != nil {
		return nil, failure
	}
	return teams, nil
}

func (call *mcpEntryCall) catalog() []catalogCapability {
	var view businessCapabilitiesView
	if call.route(http.MethodGet, "/v1/development/business-capabilities", nil, &view) != nil {
		return nil
	}
	return view.Capabilities
}

func mcpCapabilityNames(ids []string, catalog []catalogCapability) []string {
	names := []string{}
	for _, id := range ids {
		name := id[strings.LastIndex(id, ".")+1:]
		if capability := editCapability(catalog, id); capability != nil && capability.Name != "" {
			name = capability.Name
		}
		names = append(names, name)
	}
	return names
}

// mcpDraftView is the draft as an agent reads it: names instead of identifiers.
func mcpDraftView(draft developmentDraft, catalog []catalogCapability) map[string]any {
	doc := draft.Document
	memberName := map[string]string{}
	members := []map[string]any{}
	lead := ""
	for _, member := range doc.Members {
		cfg := member.Configuration
		memberName[member.ID] = cfg.DisplayName
		role := "成员"
		if cfg.Role == "avatar" {
			role, lead = "负责人", cfg.DisplayName
		}
		members = append(members, map[string]any{
			"name": cfg.DisplayName, "role": role, "enabled": member.Relationship.Enabled, "duty": member.Relationship.Duty,
			"instruction": cfg.SystemPrompt, "result_requirement": member.Relationship.ResultRequirement,
			"business_actions": mcpCapabilityNames(cfg.BusinessCapabilityIDs, catalog), "fixed_output": len(cfg.OutputSchema) > 0,
			"model": cfg.Model,
		})
	}
	flows := []map[string]any{}
	for _, flow := range doc.Workflows {
		view := map[string]any{"workflow_id": flow.ID, "name": flow.Name, "description": flow.Description}
		if graph, err := decodeEditGraph(flow.Graph); err == nil {
			label := func(id string) string { return editString(editNode(graph, id)["label"]) }
			steps := []map[string]any{}
			for _, node := range editItems(graph, "nodes") {
				step := map[string]any{"name": editString(node["label"])}
				config := editObject(node["config"])
				switch editNodeType(node) {
				case "lead":
					step["type"], step["executor"], step["requirement"] = "负责人处理", lead, editString(config["instruction"])
				case "worker":
					step["type"], step["executor"], step["requirement"] = "成员执行", memberName[editString(config["agent_id"])], editString(config["result_requirement"])
				case "parallel":
					step["type"] = "并行分工"
				case "join":
					step["type"], step["policy"] = "汇合结果", editString(config["policy"])
				case "deliver":
					step["type"], step["delivers"] = "交付结果", label(editResultSource(node))
				case "wait":
					step["type"] = "等待"
					if task := editObject(config["task"]); editString(config["kind"]) == "human" {
						step["type"], step["instructions"], step["approver"] = "人工审批", editString(task["instructions"]), editString(task["audience_ref"])
					}
				case "condition":
					step["type"] = "条件分支"
				case "loop":
					step["type"], step["max_rounds"] = "循环", config["max_iterations"]
				default:
					step["type"] = "其他步骤"
				}
				next := []string{}
				for _, edge := range editEdges(graph, "from_node_id", editString(node["id"])) {
					when := map[string]string{"failure": "（失败时）", "case": "（条件满足时）", "default": "（其他情况）", "timeout": "（超时）"}[editString(edge["route"])]
					next = append(next, label(editString(edge["to_node_id"]))+when)
				}
				if len(next) > 0 {
					step["next"] = next
				}
				steps = append(steps, step)
			}
			view["steps"], view["can_ask_for_materials"] = steps, editProtocolOn(graph)
			if completion, err := editCompletionRequirement(graph); err == nil && completion != nil {
				view["must_succeed"] = mcpCapabilityNames(completion.Capabilities, catalog)
			}
		}
		flows = append(flows, view)
	}
	topics := []map[string]string{}
	for _, topic := range developmentOperationTopics {
		topics = append(topics, map[string]string{"topic": topic.Topic, "title": topic.Title, "covers": topic.Covers})
	}
	trials := []map[string]any{}
	for i, trial := range draft.Trials {
		if i == 5 {
			break
		}
		trials = append(trials, map[string]any{"request_id": trial.RequestID, "workflow_id": trial.WorkflowID, "status": trial.Status, "of_current_draft": trial.Revision == draft.Revision})
	}
	available := []map[string]string{}
	for _, capability := range catalog {
		if capability.Status == "available" && capability.ExecutionMode != "employee_only" {
			available = append(available, map[string]string{"name": capability.Name, "description": capability.Description, "effect": capability.Effect})
		}
	}
	return map[string]any{
		"revision": draft.Revision, "published": draft.PublishedRevision > 0, "unpublished_changes": draft.Revision != draft.PublishedRevision,
		"ready_to_publish": draft.PublicationReadiness.Ready,
		"team":             map[string]any{"name": doc.Name, "objective": doc.Objective, "members": members, "workflows": flows},
		"issues":           draft.Issues, "editable": topics, "console_only": developmentConsoleOnly,
		"business_actions": available, "trials": trials,
	}
}

func (call *mcpEntryCall) draft(teamID string) (developmentDraft, *mcpEntryError) {
	var draft developmentDraft
	failure := call.route(http.MethodGet, "/v1/teams/"+url.PathEscape(teamID)+"/development", nil, &draft)
	return draft, failure
}

// mcpRunView is a run as an agent reads it: where it stands, what each step
// produced, and whether the automatic checks passed.
func (call *mcpEntryCall) runView(runID string, queued bool) (map[string]any, *mcpEntryError) {
	var activity struct {
		Status   string `json:"status"`
		WaitKind string `json:"wait_kind"`
		Members  []struct {
			Name   string `json:"name"`
			Stages []struct {
				Name          string   `json:"name"`
				Status        string   `json:"status"`
				FailureReason string   `json:"failure_reason"`
				OutputRefs    []string `json:"output_refs"`
				Outputs       []struct {
					Content   string `json:"content"`
					Truncated bool   `json:"truncated"`
				} `json:"outputs"`
			} `json:"stages"`
		} `json:"members"`
		Deliverables []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"deliverables"`
		Delivery *struct {
			Status string `json:"verification_status"`
			Checks []struct {
				Title  string `json:"title"`
				ID     string `json:"check_id"`
				Status string `json:"status"`
			} `json:"checks"`
		} `json:"delivery"`
	}
	path := "/v1/runs/" + url.PathEscape(runID)
	if failure := call.route(http.MethodGet, path+"/activity", nil, &activity); failure != nil {
		// Work that was accepted but has not started yet has no activity to read.
		if queued && failure.Code == "not_found" {
			return map[string]any{"run_id": runID, "status": "queued", "finished": false, "steps": []map[string]any{}}, nil
		}
		return nil, failure
	}
	saved := func(id string) (string, bool) {
		var output struct {
			Content   string `json:"content"`
			Truncated bool   `json:"truncated"`
		}
		// The console route records a view of someone else's run; a member
		// without console access reads the saved record of their own run.
		if call.route(http.MethodGet, "/v1/admin/tasks/"+url.PathEscape(runID)+"/outputs/"+url.PathEscape(id), nil, &output) != nil &&
			call.route(http.MethodGet, "/v1/deliverables/"+url.PathEscape(id), nil, &output) != nil {
			return "", false
		}
		text, clipped := mcpClip(output.Content, mcpContentLimit)
		return text, clipped || output.Truncated
	}
	steps := []map[string]any{}
	for _, member := range activity.Members {
		for _, stage := range member.Stages {
			step := map[string]any{"name": stage.Name, "member": member.Name, "status": stage.Status}
			if stage.FailureReason != "" {
				step["failure"] = stage.FailureReason
			}
			results, truncated := []string{}, false
			for _, output := range stage.Outputs {
				text, clipped := mcpClip(output.Content, mcpContentLimit)
				results, truncated = append(results, text), truncated || clipped || output.Truncated
			}
			for _, id := range stage.OutputRefs {
				if text, clipped := saved(id); text != "" {
					results, truncated = append(results, text), truncated || clipped
				}
			}
			if len(results) > 0 {
				step["results"], step["truncated"] = results, truncated
			}
			steps = append(steps, step)
		}
	}
	view := map[string]any{"run_id": runID, "status": activity.Status, "steps": steps, "finished": editOneOf(activity.Status, "succeeded", "failed", "cancelled")}
	if activity.WaitKind != "" {
		view["waiting_for"] = activity.WaitKind
	}
	for _, deliverable := range activity.Deliverables {
		if deliverable.Kind == "final" {
			if text, clipped := saved(deliverable.ID); text != "" {
				view["result"], view["result_truncated"] = text, clipped
			}
		}
	}
	if activity.Delivery != nil {
		checks := []map[string]string{}
		for _, check := range activity.Delivery.Checks {
			checks = append(checks, map[string]string{"check": editFirst(check.Title, check.ID), "status": check.Status})
		}
		view["verification"], view["checks"] = activity.Delivery.Status, checks
	}
	return view, nil
}

func mcpSchema(properties string, required ...string) json.RawMessage {
	list, _ := json.Marshal(required)
	if len(required) == 0 {
		list = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` + properties + `},"required":` + string(list) + `}`)
}

const (
	mcpTeamProp     = `"team_id":{"type":"string","description":"团队标识，来自团队列表或查找结果"}`
	mcpRequestProp  = `"request_id":{"type":"string","description":"本次请求的 UUID；结果不确定时用同一个值重试"}`
	mcpWorkflowProp = `"workflow_id":{"type":"string","description":"流程标识，来自草稿或团队承接能力"}`
)

var mcpEntryTools = []mcpEntryTool{
	{Name: "weave_team_list", Description: "列出团队。purpose 为 develop 时列出可以开发的团队（需要开发权限），为 use 时列出可以交接工作的团队。",
		Schema: mcpSchema(`"purpose":{"type":"string","enum":["develop","use"]}`, "purpose"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			purpose := mcpText(args, "purpose")
			if !editOneOf(purpose, "develop", "use") {
				return nil, mcpFail("invalid_arguments", "purpose 须为 develop 或 use")
			}
			teams, failure := call.teams(purpose == "develop")
			if failure != nil {
				return nil, failure
			}
			views := []map[string]any{}
			for _, team := range teams {
				if purpose == "develop" || team.Workflow != "" {
					views = append(views, team.view())
				}
			}
			return map[string]any{"teams": views}, nil
		}},
	{Name: "weave_development_read", Description: "读取一个团队的开发草稿：成员、流程与步骤、草稿还差什么、可以修改的类别、可绑定的业务动作和最近的试跑。修改必须基于这里返回的准确名称和修订。",
		Schema: mcpSchema(mcpTeamProp, "team_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if failure := mcpRequire(args, "team_id"); failure != nil {
				return nil, failure
			}
			draft, failure := call.draft(mcpText(args, "team_id"))
			if failure != nil {
				return nil, failure
			}
			return mcpDraftView(draft, call.catalog()), nil
		}},
	{Name: "weave_development_guide", Description: "读取一类团队修改的准确写法和例子。类别来自草稿的 editable 清单；预览或保存前，对要用到的每一类先读一次。",
		Schema: mcpSchema(`"topic":{"type":"string"}`, "topic"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var topic map[string]any
			failure := call.route(http.MethodGet, "/v1/development/operations/"+url.PathEscape(mcpText(args, "topic")), nil, &topic)
			return topic, failure
		}},
	{Name: "weave_development_apply", Description: "按受控修改更新团队草稿。dry_run 为 true 时只返回会带来的变化，不保存；保存不改变已发布的团队。任何一项不合法时整组不保存。",
		Schema: mcpSchema(mcpTeamProp+`,`+mcpRequestProp+`,"expected_revision":{"type":"integer","description":"读取草稿时的修订"},"dry_run":{"type":"boolean"},"operations":{"type":"array","minItems":1,"maxItems":24,"items":{"type":"object"},"description":"受控修改，写法见 weave_development_guide"}`, "team_id", "request_id", "expected_revision", "operations"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if failure := mcpRequire(args, "team_id", "request_id"); failure != nil {
				return nil, failure
			}
			body := map[string]any{"expected_revision": args["expected_revision"], "request_id": mcpText(args, "request_id"), "dry_run": args["dry_run"] == true, "operations": args["operations"]}
			var result map[string]any
			failure := call.route(http.MethodPost, "/v1/teams/"+url.PathEscape(mcpText(args, "team_id"))+"/development/operations", body, &result)
			return result, failure
		}},
	{Name: "weave_team_create", Description: "新建一个团队。业务团队带负责人、一位业务办理员和基础流程；代码团队带负责人、编码与验证两位成员，以及验证未通过时退回编码的流程。只在使用者明确要求新建时调用。同一个 request_id 重发返回同一个团队。",
		Schema: mcpSchema(mcpRequestProp+`,"name":{"type":"string","maxLength":80},"objective":{"type":"string","maxLength":2000,"description":"团队处理什么事，用员工平时的说法"},"kind":{"type":"string","enum":["business","code"],"description":"business 业务团队（默认）；code 代码团队"}`, "request_id", "name", "objective"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var result map[string]any
			failure := call.route(http.MethodPost, "/v1/development/teams", map[string]any{"request_id": mcpText(args, "request_id"), "name": mcpText(args, "name"), "objective": mcpText(args, "objective"), "kind": mcpText(args, "kind")}, &result)
			return result, failure
		}},
	{Name: "weave_development_trial", Description: "在已保存的草稿上隔离试跑一条流程。只做模拟，不访问业务系统。simulate_actions 列出本次允许模拟的业务动作名称，省略则不开放任何动作。返回接受回执，结果用 weave_development_trial_read 读取。",
		Schema: mcpSchema(mcpTeamProp+`,`+mcpWorkflowProp+`,`+mcpRequestProp+`,"revision":{"type":"integer","description":"要试跑的草稿修订，须是当前修订"},"input":{"type":"string","description":"试跑用的任务原话"},"simulate_actions":{"type":"array","items":{"type":"string"}}`, "team_id", "workflow_id", "request_id", "revision", "input"),
		Run:    mcpTrial},
	{Name: "weave_development_trial_read", Description: "读取一次试跑：状态、每一步的结果和自动检查结论。",
		Schema: mcpSchema(mcpTeamProp+`,`+mcpRequestProp, "team_id", "request_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			draft, failure := call.draft(mcpText(args, "team_id"))
			if failure != nil {
				return nil, failure
			}
			for _, trial := range draft.Trials {
				if trial.RequestID == mcpText(args, "request_id") {
					if trial.RunID == "" {
						return map[string]any{"status": trial.Status, "finished": false}, nil
					}
					view, failure := call.runView(trial.RunID, false)
					if failure == nil {
						view["of_current_draft"], view["ready_to_publish"] = trial.Revision == draft.Revision, draft.PublicationReadiness.Ready
						delete(view, "run_id")
					}
					return view, failure
				}
			}
			return nil, mcpFail("not_found", "没有这次试跑")
		}},
	{Name: "weave_development_publish", Description: "发布团队草稿，之后新发起的工作使用这一版。只在使用者明确要求发布时调用；条件不满足时返回还差什么。",
		Schema: mcpSchema(mcpTeamProp+`,"revision":{"type":"integer","description":"要发布的草稿修订"}`, "team_id", "revision"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			teamID := mcpText(args, "team_id")
			var published developmentDraft
			if failure := call.route(http.MethodPost, "/v1/teams/"+url.PathEscape(teamID)+"/development/publish", map[string]any{"revision": args["revision"]}, &published); failure != nil {
				if draft, readFailure := call.draft(teamID); readFailure == nil && len(draft.Issues) > 0 {
					return map[string]any{"published": false, "reason": failure.Message, "issues": draft.Issues}, nil
				}
				return nil, failure
			}
			return map[string]any{"published": true, "revision": published.PublishedRevision}, nil
		}},
	{Name: "weave_workspace_guide", Description: "读取团队草稿以外的操作：不给 topic 时返回类别清单（团队与业务动作目录、运行、执行节点、飞书、客户端会话），给 topic 时返回这一类每项操作的准确写法、是否需要确认和当前账号能否办理。办理前对要用到的类别先读一次。",
		Schema: mcpSchema(`"topic":{"type":"string"}`),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			path := "/v1/development/workspace-operations"
			if topic := mcpText(args, "topic"); topic != "" {
				path += "/" + url.PathEscape(topic)
			}
			var guide map[string]any
			failure := call.route(http.MethodGet, path, nil, &guide)
			return guide, failure
		}},
	{Name: "weave_workspace_apply", Description: "办理一项团队草稿以外的操作，写法见 weave_workspace_guide。说明里带 confirm 的操作不可撤回：第一次调用返回 confirmation_required 和后果说明，把后果告诉使用者并得到明确同意后，再带 confirmed 为 true 调用。",
		Schema: mcpSchema(`"operation":{"type":"object","description":"一项操作，含 kind"},"confirmed":{"type":"boolean","description":"使用者已明确同意不可撤回的后果"}`, "operation"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var result map[string]any
			failure := call.route(http.MethodPost, "/v1/development/workspace-operations", map[string]any{"operation": args["operation"], "confirmed": args["confirmed"] == true}, &result)
			return result, failure
		}},
	{Name: "weave_team_search", Description: "按一段工作摘要查找可能合适的团队，至多 8 个。这是名称与目标的文字相关性筛选，是否合适由你结合上下文判断。",
		Schema: mcpSchema(`"work_summary":{"type":"string","maxLength":4000,"description":"根据完整对话概括的工作与期望结果"}`, "work_summary"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if failure := mcpRequire(args, "work_summary"); failure != nil {
				return nil, failure
			}
			teams, failure := call.teams(false)
			if failure != nil {
				return nil, failure
			}
			query := mcpTerms(mcpText(args, "work_summary"))
			type scored struct {
				team  mcpTeam
				score int
			}
			found := []scored{}
			for _, team := range teams {
				score := 0
				for term := range mcpTerms(editFirst(team.DisplayName, team.Name) + " " + team.Objective) {
					if query[term] {
						score++
					}
				}
				if score > 0 && team.Workflow != "" {
					found = append(found, scored{team, score})
				}
			}
			sort.SliceStable(found, func(i, j int) bool { return found[i].score > found[j].score })
			views := []map[string]any{}
			for i, item := range found {
				if i == 8 {
					break
				}
				views = append(views, item.team.view())
			}
			return map[string]any{"teams": views, "matching": "名称与目标的文字相关性筛选；请结合完整对话判断是否适合"}, nil
		}},
	{Name: "weave_team_describe", Description: "查看一个团队已发布的流程：接什么、需要什么、交付什么，以及流程会用到的业务动作（仅供了解，不经这里执行）。",
		Schema: mcpSchema(mcpTeamProp, "team_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var listed struct {
				Workflows []struct {
					ID           string   `json:"id"`
					Name         string   `json:"name"`
					Description  string   `json:"description"`
					Published    int      `json:"published_version"`
					Capabilities []string `json:"business_capability_ids"`
				} `json:"workflows"`
			}
			if failure := call.route(http.MethodGet, "/v1/teams/"+url.PathEscape(mcpText(args, "team_id"))+"/workflows", nil, &listed); failure != nil {
				return nil, failure
			}
			catalog, flows := call.catalog(), []map[string]any{}
			for _, flow := range listed.Workflows {
				if flow.Published > 0 {
					flows = append(flows, map[string]any{"workflow_id": flow.ID, "name": flow.Name, "description": flow.Description, "business_actions": mcpCapabilityNames(flow.Capabilities, catalog)})
				}
			}
			return map[string]any{"workflows": flows, "note": "业务动作不经这里执行；需要办理业务的部分由员工在桌面办理"}, nil
		}},
	{Name: "weave_work_submit", Description: "把一件只读工作交给团队。materials 是文本材料（名称与正文）。不带业务动作授权。返回接单回执和 run_id；接单不表示处理完成，结果用 weave_work_read 读取。",
		Schema: mcpSchema(mcpTeamProp+`,`+mcpWorkflowProp+`,`+mcpRequestProp+`,"goal":{"type":"string","maxLength":20000,"description":"交给团队的工作目标和期望结果"},"materials":{"type":"array","maxItems":16,"items":{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"text":{"type":"string"}},"required":["name","text"]}}`, "team_id", "request_id", "goal"),
		Run:    mcpSubmit},
	{Name: "weave_work_read", Description: "读取一件已交接工作的状态、每一步的结果、最终结果和自动检查结论。",
		Schema: mcpSchema(`"run_id":{"type":"string"}`, "run_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if failure := mcpRequire(args, "run_id"); failure != nil {
				return nil, failure
			}
			if _, failure := call.submitted(mcpText(args, "run_id")); failure != nil {
				return nil, failure
			}
			return call.runView(mcpText(args, "run_id"), true)
		}},
	{Name: "weave_work_cancel", Description: "请求停止一件本人交接的工作。",
		Schema: mcpSchema(`"run_id":{"type":"string"}`, "run_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if mine, failure := call.submitted(mcpText(args, "run_id")); failure != nil || !mine {
				return nil, mcpFail("not_found", "没有这件由你交接的工作")
			}
			if failure := call.route(http.MethodPost, "/v1/runs/"+url.PathEscape(mcpText(args, "run_id"))+"/stop", map[string]any{}, nil); failure != nil {
				return nil, failure
			}
			return map[string]any{"stop_requested": true}, nil
		}},
	{Name: "device_enroll", Description: "为使用者本人添加一台 Mac 或 Windows 设备：返回一次性注册码（10 分钟内有效，只能用一次）和在那台机器上运行的一条注册命令。只接受本人账号登录的会话。把命令交给使用者，或在你已能到达那台机器时替使用者运行；Mac 上最后要使用者本人在系统设置中打开“辅助功能”和“屏幕录制”。",
		Schema: mcpSchema(``),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var enrollment map[string]any
			if failure := call.route(http.MethodPost, "/v1/devices/enrollments", map[string]any{}, &enrollment); failure != nil {
				return nil, failure
			}
			return enrollment, nil
		}},
}

func mcpTrial(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
	if failure := mcpRequire(args, "team_id", "workflow_id", "request_id", "input"); failure != nil {
		return nil, failure
	}
	teamID, workflowID := mcpText(args, "team_id"), mcpText(args, "workflow_id")
	draft, failure := call.draft(teamID)
	if failure != nil {
		return nil, failure
	}
	var flow *developmentWorkflow
	for i := range draft.Document.Workflows {
		if draft.Document.Workflows[i].ID == workflowID {
			flow = &draft.Document.Workflows[i]
		}
	}
	if flow == nil {
		return nil, mcpFail("not_found", "草稿里没有这条流程")
	}
	graph, err := decodeEditGraph(flow.Graph)
	if err != nil {
		return nil, mcpFail("invalid", "流程定义无法读取")
	}
	catalog := call.catalog()
	simulate := map[string]bool{}
	wanted, _ := args["simulate_actions"].([]any)
	for _, name := range wanted {
		text, _ := name.(string)
		simulate[text] = true
	}
	actions, seen := []map[string]any{}, map[string]bool{}
	for _, member := range editFlowMembers(&draft.Document, graph) {
		for _, id := range member.Configuration.BusinessCapabilityIDs {
			capability := editCapability(catalog, id)
			if seen[id] {
				continue
			}
			seen[id] = true
			if capability == nil {
				return nil, mcpFail("business_action_unavailable", "流程用到的一项业务动作不在目录中，请在管理端刷新目录或重新选择")
			}
			key := strings.TrimPrefix(id, "forge:action:")
			object, action, _ := strings.Cut(key, ".")
			actions = append(actions, map[string]any{
				"capability_id": id, "name": action, "object_name": object, "label": capability.Name, "description": capability.Description,
				"requires_record": capability.RequiresRecord, "requires_confirmation": capability.RequiresConfirmation,
				"simulation_authorized": simulate[capability.Name], "params": capability.Params,
			})
			delete(simulate, capability.Name)
		}
	}
	for name := range simulate {
		return nil, mcpFail("invalid_arguments", "业务动作“%s”不属于这条流程", name)
	}
	body := map[string]any{"revision": args["revision"], "workflow_id": workflowID, "request_id": mcpText(args, "request_id"), "input": args["input"], "business_actions": actions}
	if failure := call.route(http.MethodPost, "/v1/teams/"+url.PathEscape(teamID)+"/development/trials", body, nil); failure != nil {
		return nil, failure
	}
	return map[string]any{"accepted": true, "request_id": mcpText(args, "request_id")}, nil
}

func mcpSubmit(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
	if failure := mcpRequire(args, "team_id", "request_id", "goal"); failure != nil {
		return nil, failure
	}
	task := strings.Builder{}
	task.WriteString(mcpText(args, "goal"))
	materials, _ := args["materials"].([]any)
	for _, item := range materials {
		material := editObject(item)
		name, text := mcpText(material, "name"), editString(material["text"])
		if name == "" || strings.TrimSpace(text) == "" {
			return nil, mcpFail("invalid_arguments", "每份材料都要有名称和正文")
		}
		task.WriteString("\n\n## 材料：" + name + "\n\n" + text)
	}
	if task.Len() > mcpMaterialLimit {
		return nil, mcpFail("too_large", "目标和材料合计不能超过 %d KB", mcpMaterialLimit>>10)
	}
	body := map[string]any{"task": task.String(), "client_request_id": mcpText(args, "request_id")}
	if workflowID := mcpText(args, "workflow_id"); workflowID != "" {
		body["workflow_id"] = workflowID
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if failure := call.route(http.MethodPost, "/v1/teams/"+url.PathEscape(mcpText(args, "team_id"))+"/dispatch", body, &accepted); failure != nil {
		return nil, failure
	}
	return map[string]any{"accepted": true, "run_id": accepted.RunID, "note": "已接单，尚未处理完成"}, nil
}

// entryCall is the caller of this request, for calling Weave's own routes as them.
func (s *Server) entryCall(c echo.Context) *mcpEntryCall {
	return &mcpEntryCall{
		server: s, ctx: c.Request().Context(), auth: c.Request().Header.Get(echo.HeaderAuthorization), remote: c.Request().RemoteAddr,
		host: c.Request().Host, secure: adminRequestSecure(c.Request()),
		tenant: getTenant(c), user: getUserID(c), role: firstRole(c), reader: adminRunReader(c),
	}
}

// handleMCPEntry serves the tools to an authenticated caller: a person signed in
// with their own account, or the holder of a key, as on the console.
func (s *Server) handleMCPEntry(c echo.Context) error {
	call := s.entryCall(c)
	return serveWeaveTools(c, mcpEntryTools, mcpEntryInstructions, func(name, arguments string) (string, bool) {
		for _, tool := range mcpEntryTools {
			if tool.Name != name {
				continue
			}
			args := map[string]any{}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				raw, _ := json.Marshal(mcpFail("invalid_arguments", "参数不是有效的 JSON 对象"))
				return string(raw), true
			}
			// A workspace operation that starts work is limited like handing work over.
			limited := name
			if name == "weave_workspace_apply" && workspaceStarts(editObject(args["operation"])) {
				limited = "weave_work_submit"
			}
			if wait := s.mcpThrottle(c, limited); wait != nil {
				raw, _ := json.Marshal(wait)
				return string(raw), true
			}
			result, failure := tool.Run(call, args)
			if failure != nil {
				raw, _ := json.Marshal(failure)
				return string(raw), true
			}
			raw, err := json.Marshal(result)
			if err != nil {
				return `{"code":"failed","message":"结果无法编码"}`, true
			}
			return string(raw), false
		}
		return `{"code":"unknown_tool","message":"没有这个工具"}`, true
	})
}
