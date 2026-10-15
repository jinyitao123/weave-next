package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestDevelopmentTeamIsCreatedOncePerRequestRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "create-workspace", UserID: "developer"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('create-workspace','create-workspace','Create'); INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer','create-workspace','developer','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	server := &Server{Pool: pool, Store: teamDispatchPoolStore{pool: pool}, Registry: agentcatalog.New(pool), OrgStore: orgstore.NewStore(pool)}
	call := func(method string, body any, handler echo.HandlerFunc, teamID string) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		c := echo.New().NewContext(req, rr)
		if teamID != "" {
			c.SetParamNames("id")
			c.SetParamValues(teamID)
		}
		c.Set("tenant", "create-workspace")
		c.Set("user_id", "developer")
		if err := handler(c); err != nil {
			httpError, isHTTP := err.(*echo.HTTPError)
			if !isHTTP {
				t.Fatalf("handler: %v", err)
			}
			return httpError.Code, map[string]any{"error": httpError.Message}
		}
		var decoded map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response: %v %s", err, rr.Body.String())
		}
		return rr.Code, decoded
	}
	create := func(requestID, name, objective, kind string) (int, map[string]any) {
		t.Helper()
		return call("POST", map[string]any{"request_id": requestID, "name": name, "objective": objective, "kind": kind}, server.handleCreateDevelopmentTeam, "")
	}

	requestID := uuid.NewString()
	status, created := create(requestID, "线索跟进团队", "整理销售线索材料，判断是否值得跟进", "business")
	if status != http.StatusCreated || created["name"] != "线索跟进团队" || created["team_id"] == "" {
		t.Fatalf("create: %d %v", status, created)
	}
	teamID := created["team_id"].(string)

	// The team opens as a draft with its lead, one member and the plain flow.
	status, draft := call("GET", nil, server.handleGetTeamDevelopment, teamID)
	if status != http.StatusOK || draft["revision"] != created["revision"] {
		t.Fatalf("draft: %d %v", status, draft["revision"])
	}
	document := draft["document"].(map[string]any)
	members, flows := document["members"].([]any), document["workflows"].([]any)
	if len(members) != 2 || len(flows) != 1 || document["objective"] != "整理销售线索材料，判断是否值得跟进" {
		t.Fatalf("starter draft = %v", document)
	}
	names := map[string]string{}
	for _, raw := range members {
		member := raw.(map[string]any)
		names[member["configuration"].(map[string]any)["display_name"].(string)] = member["relationship"].(map[string]any)["duty"].(string)
	}
	if names["团队负责人"] != "理解任务目标并汇总可核验结果" || names["业务办理员"] == "" {
		t.Fatalf("starter members = %v", names)
	}
	flow := flows[0].(map[string]any)
	if flow["name"] != "业务流程" || flow["description"] != "整理销售线索材料，判断是否值得跟进" {
		t.Fatalf("starter flow = %v", flow["name"])
	}
	// No model provider is configured here, so what is left is choosing a model for the lead and the member.
	issues := draft["issues"].([]any)
	if len(issues) != 2 || issues[0].(map[string]any)["code"] != "member_model_missing" || issues[1].(map[string]any)["code"] != "member_model_missing" {
		t.Fatalf("starter issues = %v", issues)
	}

	// The same request again returns the same team and creates nothing.
	status, replay := create(requestID, "线索跟进团队", "整理销售线索材料，判断是否值得跟进", "business")
	if status != http.StatusOK || replay["team_id"] != teamID || replay["revision"] != created["revision"] {
		t.Fatalf("replay: %d %v", status, replay)
	}
	if status, conflict := create(requestID, "另一个团队", "别的目标", "business"); status != http.StatusConflict || conflict["code"] != "request_conflict" {
		t.Fatalf("reused request id: %d %v", status, conflict)
	}
	if status, refused := create(uuid.NewString(), "运维团队", "处理告警", "ops"); status != http.StatusUnprocessableEntity || refused["code"] != "team_kind_unsupported" {
		t.Fatalf("unknown kind: %d %v", status, refused)
	}
	if status, invalid := create("not-a-uuid", "团队", "目标", "business"); status != http.StatusUnprocessableEntity || invalid["code"] != "team_create_invalid" {
		t.Fatalf("invalid request: %d %v", status, invalid)
	}
	var teams, agents, records int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM weave_teams WHERE workspace_id='create-workspace'),
		(SELECT count(*) FROM weave_agents WHERE workspace_id='create-workspace' AND NOT deleted),
		(SELECT count(*) FROM weave_team_development_creations WHERE workspace_id='create-workspace' AND actor_id='developer' AND team_id=$1)`, teamID).Scan(&teams, &agents, &records); err != nil || teams != 1 || agents != 2 || records != 1 {
		t.Fatalf("teams=%d agents=%d records=%d err=%v", teams, agents, records, err)
	}

	// A code team starts like the console's: coding then independent
	// verification on another engine, sent back while verification fails.
	status, codeTeam := create(uuid.NewString(), "支付服务开发", "为支付服务实现需求，并在合入前通过测试", "code")
	if status != http.StatusCreated || codeTeam["team_id"] == "" {
		t.Fatalf("code team: %d %v", status, codeTeam)
	}
	status, codeDraft := call("GET", nil, server.handleGetTeamDevelopment, codeTeam["team_id"].(string))
	if status != http.StatusOK {
		t.Fatalf("code draft: %d %v", status, codeDraft)
	}
	codeDocument := codeDraft["document"].(map[string]any)
	engines := map[string]string{}
	for _, raw := range codeDocument["members"].([]any) {
		configuration := raw.(map[string]any)["configuration"].(map[string]any)
		engines[configuration["display_name"].(string)] = configuration["engine"].(string)
	}
	if engines["团队负责人"] != "loom" || engines["编码"] != "claude" || engines["验证"] != "codex" {
		t.Fatalf("code team members = %v", engines)
	}
	codeFlows := codeDocument["workflows"].([]any)
	if len(codeFlows) != 1 || codeFlows[0].(map[string]any)["name"] != "开发与验证" {
		t.Fatalf("code team flow = %v", codeFlows)
	}
	graph := codeFlows[0].(map[string]any)["graph_definition"].(map[string]any)
	types := map[string]int{}
	for _, raw := range graph["nodes"].([]any) {
		types[raw.(map[string]any)["type"].(string)]++
	}
	if types["loop"] != 1 || types["worker"] != 2 || types["deliver"] != 1 || len(graph["edges"].([]any)) != 4 {
		t.Fatalf("code team graph = %v", types)
	}

	// A second request is a second team, even under the same name.
	status, second := create(uuid.NewString(), "线索跟进团队", "整理销售线索材料，判断是否值得跟进", "business")
	if status != http.StatusCreated || second["team_id"] == teamID {
		t.Fatalf("second team: %d %v", status, second)
	}
}
