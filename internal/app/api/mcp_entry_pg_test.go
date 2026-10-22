package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestMCPTermsCompareWordsAndChinesePairs(t *testing.T) {
	got := mcpTerms("线索跟进 MES2 上线，核")
	want := map[string]bool{"线索": true, "索跟": true, "跟进": true, "mes2": true, "上线": true, "核": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terms = %v", got)
	}
}

func TestMCPEntryServesTeamToolsAsTheCallerRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.Config, s.UserStore = echo.New(), &config.Config{JWTSecret: "mcp-entry-test"}, users.NewStore(pool)
	if s.Pool == nil {
		s.Pool = pool
	}
	if s.OrgStore == nil {
		s.OrgStore = orgstore.NewStore(pool)
	}
	if s.Registry == nil {
		s.Registry = agentcatalog.New(pool)
	}
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	s.teamRunCancel = &teamrun.CancelService{Transactions: pool, Runs: runs, Tasks: s.Tasks}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()
	rpc := func(user, role, method string, params any) (int, map[string]any) {
		t.Helper()
		token, err := s.signJWTWithPermissionSets("ws", user, []string{role}, "forge", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/mcp", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		return resp.StatusCode, decoded
	}
	// tool calls one tool and returns its decoded answer and whether it reported an error.
	tool := func(user, role, name string, arguments map[string]any) (map[string]any, bool) {
		t.Helper()
		status, response := rpc(user, role, "tools/call", map[string]any{"name": name, "arguments": arguments})
		result, _ := response["result"].(map[string]any)
		if status != http.StatusOK || result == nil {
			t.Fatalf("%s: %d %v", name, status, response)
		}
		var answer map[string]any
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			t.Fatalf("%s answered %q", name, text)
		}
		return answer, result["isError"] == true
	}
	list := func(value any) []any { items, _ := value.([]any); return items }

	if status, response := rpc("user", "admin", "initialize", map[string]any{}); status != http.StatusOK || response["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "weave" || !strings.Contains(response["result"].(map[string]any)["instructions"].(string), "weave_development_guide") {
		t.Fatalf("initialize: %d %v", status, response)
	}
	if status, response := rpc("user", "admin", "ping", nil); status != http.StatusOK || response["result"] == nil {
		t.Fatalf("ping: %d %v", status, response)
	}
	_, listed := rpc("user", "admin", "tools/list", nil)
	tools := list(listed["result"].(map[string]any)["tools"])
	// 15 team tools, device_enroll and 11 device control tools.
	if len(tools) != 27 {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, raw := range tools {
		described := raw.(map[string]any)
		schema, _ := described["inputSchema"].(map[string]any)
		if described["description"] == "" || schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("tool %v is not fully described: %v", described["name"], schema)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if resp, err := host.Client().Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a call without a credential must be refused: %v %v", resp, err)
	}

	// Developing a team: create, read, look up how to write a change, preview, save.
	requestID := uuid.NewString()
	created, failed := tool("user", "admin", "weave_team_create", map[string]any{"request_id": requestID, "name": "线索跟进团队", "objective": "整理销售线索材料，判断是否值得跟进"})
	teamID, _ := created["team_id"].(string)
	if failed || teamID == "" {
		t.Fatalf("create: %v", created)
	}
	if again, _ := tool("user", "admin", "weave_team_create", map[string]any{"request_id": requestID, "name": "线索跟进团队", "objective": "整理销售线索材料，判断是否值得跟进"}); again["team_id"] != teamID {
		t.Fatalf("a repeated create made another team: %v", again)
	}
	draft, failed := tool("user", "admin", "weave_development_read", map[string]any{"team_id": teamID})
	team, _ := draft["team"].(map[string]any)
	if failed || len(list(team["members"])) != 2 || len(list(team["workflows"])) != 1 || len(list(draft["editable"])) != len(developmentOperationTopics) || len(list(draft["issues"])) != 2 {
		t.Fatalf("draft: %v", draft)
	}
	if text, _ := json.Marshal(draft); strings.Contains(string(text), "agent_id") || strings.Contains(string(text), `"kind":`) {
		t.Fatalf("the draft view leaks internals or operation shapes: %s", text)
	}
	flow := list(team["workflows"])[0].(map[string]any)
	steps := list(flow["steps"])
	if flow["name"] != "业务流程" || len(steps) != 3 || steps[1].(map[string]any)["executor"] != "业务办理员" || steps[2].(map[string]any)["delivers"] != "业务办理员" {
		t.Fatalf("flow view: %v", flow)
	}
	if guide, failed := tool("user", "admin", "weave_development_guide", map[string]any{"topic": "member"}); failed || len(list(guide["operations"])) != 3 {
		t.Fatalf("guide: %v", guide)
	}
	if missing, failed := tool("user", "admin", "weave_development_guide", map[string]any{"topic": "engine"}); !failed || missing["code"] != "operation_topic_not_found" {
		t.Fatalf("unknown topic: %v", missing)
	}
	// Operations outside the draft: described here, carried out unless they need a secret shown or typed.
	if guide, failed := tool("user", "admin", "weave_workspace_guide", map[string]any{}); failed || len(list(guide["topics"])) != len(workspaceTopics) {
		t.Fatalf("workspace guide: %v", guide)
	}
	if guide, failed := tool("user", "admin", "weave_workspace_guide", map[string]any{"topic": "node"}); failed || len(list(guide["operations"])) != 7 {
		t.Fatalf("workspace node guide: %v", guide)
	}
	if sessions, failed := tool("user", "admin", "weave_workspace_apply", map[string]any{"operation": map[string]any{"kind": "client_session_list"}}); failed || sessions["status"] != "done" {
		t.Fatalf("workspace apply: %v", sessions)
	}
	if asked, failed := tool("user", "admin", "weave_workspace_apply", map[string]any{"operation": map[string]any{"kind": "team_archive", "team_id": teamID}}); failed || asked["status"] != "confirmation_required" {
		t.Fatalf("an archive without consent: %v", asked)
	}
	// repository_save left with the code repository line on 2026-10-21.
	for _, operation := range []map[string]any{{"kind": "node_add", "name": "研发一号机"}, {"kind": "catalog_refresh"}} {
		if refused, failed := tool("user", "admin", "weave_workspace_apply", map[string]any{"operation": operation, "confirmed": true}); !failed || refused["code"] != "desktop_only" {
			t.Fatalf("%v reached an agent that cannot show or take a secret: %v", operation["kind"], refused)
		}
	}
	change := []map[string]any{{"kind": "member", "member": "业务办理员", "duty": "整理线索并给出是否跟进的结论"}}
	preview, failed := tool("user", "admin", "weave_development_apply", map[string]any{"team_id": teamID, "request_id": uuid.NewString(), "expected_revision": draft["revision"], "dry_run": true, "operations": change})
	if failed || preview["saved"] != false || len(list(preview["changes"])) != 1 {
		t.Fatalf("preview: %v", preview)
	}
	saved, failed := tool("user", "admin", "weave_development_apply", map[string]any{"team_id": teamID, "request_id": uuid.NewString(), "expected_revision": draft["revision"], "operations": change})
	if failed || saved["saved"] != true || saved["revision"] == draft["revision"] {
		t.Fatalf("save: %v", saved)
	}
	refused, failed := tool("user", "admin", "weave_development_apply", map[string]any{"team_id": teamID, "request_id": uuid.NewString(), "expected_revision": saved["revision"], "operations": []map[string]any{{"kind": "team", "objective": "x"}, {"kind": "member", "member": "不存在的人", "duty": "x"}}})
	if !failed || refused["code"] != "operation_invalid" || !strings.HasPrefix(refused["message"].(string), "第 2 项：") {
		t.Fatalf("refusal: %v", refused)
	}
	if unknown, failed := tool("user", "admin", "weave_development_trial", map[string]any{"team_id": teamID, "workflow_id": flow["workflow_id"], "request_id": uuid.NewString(), "revision": saved["revision"], "input": "试一下", "simulate_actions": []string{"转为商机"}}); !failed || unknown["code"] != "invalid_arguments" {
		t.Fatalf("simulating an action the flow does not use: %v", unknown)
	}

	// Someone without a development role cannot read or change a draft.
	for name, arguments := range map[string]map[string]any{
		"weave_development_read": {"team_id": teamID},
		"weave_team_create":      {"request_id": uuid.NewString(), "name": "成员建的团队", "objective": "不应建成"},
	} {
		if answer, failed := tool("user-a", "member", name, arguments); !failed || answer["code"] != "forbidden" {
			t.Errorf("%s as a member: %v", name, answer)
		}
	}

	// Using a team: find it, see what it takes on, hand work over, read it back.
	usable, failed := tool("user-a", "member", "weave_team_list", map[string]any{"purpose": "use"})
	if failed || len(list(usable["teams"])) != 1 {
		t.Fatalf("usable teams: %v", usable)
	}
	published := list(usable["teams"])[0].(map[string]any)
	found, failed := tool("user-a", "member", "weave_team_search", map[string]any{"work_summary": published["name"].(string) + " " + published["objective"].(string)})
	if failed || len(list(found["teams"])) != 1 || list(found["teams"])[0].(map[string]any)["team_id"] != published["team_id"] {
		t.Fatalf("search: %v", found)
	}
	if none, _ := tool("user-a", "member", "weave_team_search", map[string]any{"work_summary": "zzzz 毫不相干"}); len(list(none["teams"])) != 0 {
		t.Fatalf("unrelated search: %v", none)
	}
	described, failed := tool("user-a", "member", "weave_team_describe", map[string]any{"team_id": published["team_id"]})
	// The dispatch fixture has no run reader for the listing's latest-run column,
	// so here the listing itself declines; with one, the answer is the published flows.
	if message, _ := described["message"].(string); failed && message != "read_store_unavailable" || !failed && len(list(described["workflows"])) != 1 {
		t.Fatalf("describe: %v", described)
	}
	workRequest := uuid.NewString()
	work := map[string]any{"team_id": published["team_id"], "request_id": workRequest, "goal": "核对这份报价的合计", "materials": []map[string]any{{"name": "报价单", "text": "设备 1800 元，安装 500 元，合计 2300 元。"}}}
	accepted, failed := tool("user-a", "member", "weave_work_submit", work)
	runID, _ := accepted["run_id"].(string)
	if failed || runID == "" {
		t.Fatalf("submit: %v", accepted)
	}
	if again, _ := tool("user-a", "member", "weave_work_submit", work); again["run_id"] != runID {
		t.Fatalf("a repeated submit started another run: %v", again)
	}
	var input string
	if err := pool.QueryRow(t.Context(), `SELECT request->>'input' FROM weave_workflow_admission_requests WHERE workspace_id='ws' AND request->>'run_id'=$1`, runID).Scan(&input); err != nil || !strings.Contains(input, "核对这份报价的合计") || !strings.Contains(input, "材料：报价单") {
		t.Fatalf("queued input = %s %v", input, err)
	}
	read, failed := tool("user-a", "member", "weave_work_read", map[string]any{"run_id": runID})
	if failed || read["finished"] != false || read["status"] != "queued" {
		t.Fatalf("read: %v", read)
	}
	// Only the submitter and those who read every run may read or stop it.
	for _, name := range []string{"weave_work_read", "weave_work_cancel"} {
		if other, failed := tool("user-b", "member", name, map[string]any{"run_id": runID}); !failed || other["code"] != "not_found" {
			t.Fatalf("%s by another member: %v", name, other)
		}
	}
	if seen, failed := tool("user", "admin", "weave_work_read", map[string]any{"run_id": runID}); failed || seen["status"] != "queued" {
		t.Fatalf("an administrator reads the run: %v", seen)
	}
	if stopped, failed := tool("user", "admin", "weave_work_cancel", map[string]any{"run_id": runID}); !failed || stopped["code"] != "not_found" {
		t.Fatalf("an administrator stopped work they did not hand over: %v", stopped)
	}
	if large, failed := tool("user-a", "member", "weave_work_submit", map[string]any{"team_id": published["team_id"], "request_id": uuid.NewString(), "goal": "太大", "materials": []map[string]any{{"name": "大文件", "text": strings.Repeat("字", 80000)}}}); !failed || large["code"] != "too_large" {
		t.Fatalf("oversized materials: %v", large)
	}
	if unknown, failed := tool("user", "admin", "weave_no_such_tool", map[string]any{}); !failed || unknown["code"] != "unknown_tool" {
		t.Fatalf("unknown tool: %v", unknown)
	}
}
