package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestWorkspaceOperationsAreDescribedAndKnown(t *testing.T) {
	kinds := map[string]bool{}
	for _, topic := range workspaceTopics {
		if topic.Title == "" || topic.Covers == "" || len(topic.Rules) == 0 || len(topic.Operations) == 0 {
			t.Errorf("topic %s is incomplete", topic.Topic)
		}
		for _, operation := range topic.Operations {
			var example map[string]any
			if err := json.Unmarshal([]byte(operation.Example), &example); err != nil || example["kind"] != operation.Kind || operation.Summary == "" || operation.Run == nil || kinds[operation.Kind] || (operation.Confirm == "") != (operation.Asks == "") {
				t.Errorf("operation %s: example %v, %v", operation.Kind, example, err)
			}
			kinds[operation.Kind] = true
			if workspaceOperationOf(operation.Kind) == nil {
				t.Errorf("operation %s cannot be found by its kind", operation.Kind)
			}
			// Whatever asks for or hands back a secret says so, so that no caller guesses.
			if names, _ := editStringList(example["enter"]); len(names) > len(operation.Secrets) {
				t.Errorf("operation %s enters secrets it does not declare", operation.Kind)
			}
			for _, secret := range operation.Secrets {
				if _, asField := example[secret.Field]; asField {
					t.Errorf("operation %s shows secret %s as an operation field", operation.Kind, secret.Field)
				}
			}
		}
	}
	if len(kinds) != 21 || workspaceStarts(map[string]any{"kind": "code_task_submit"}) || workspaceStarts(map[string]any{"kind": "run_stop"}) || workspaceStarts(nil) {
		t.Fatalf("kinds = %d", len(kinds))
	}
}

func TestWorkspaceOperationsRunAsTheCallerRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.Config, s.UserStore = echo.New(), &config.Config{JWTSecret: "workspace-operations-test"}, users.NewStore(pool)
	if s.Pool == nil {
		s.Pool = pool
	}
	if s.OrgStore == nil {
		s.OrgStore = orgstore.NewStore(pool)
	}
	if s.Registry == nil {
		s.Registry = agentcatalog.New(pool)
	}
	s.Runtimes = runtimes.NewStore(pool)
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()
	send := func(role, method, path string, body any) (int, map[string]any) {
		t.Helper()
		token, err := s.signJWTWithPermissionSets("ws", "user", []string{role}, "forge", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, host.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		text, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(text, &decoded)
		return resp.StatusCode, decoded
	}
	// apply sends one operation the way the desktop does: as an interactive caller.
	apply := func(role string, operation map[string]any, extra map[string]any) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"operation": operation, "interactive": true}
		for key, value := range extra {
			body[key] = value
		}
		return send(role, http.MethodPost, "/v1/development/workspace-operations", body)
	}
	done := func(role string, operation map[string]any, extra map[string]any) map[string]any {
		t.Helper()
		status, answer := apply(role, operation, extra)
		if status != http.StatusOK || answer["status"] != "done" {
			t.Fatalf("%v: %d %v", operation["kind"], status, answer)
		}
		result, _ := answer["result"].(map[string]any)
		return result
	}
	list := func(value any) []any { items, _ := value.([]any); return items }
	confirmed := map[string]any{"confirmed": true}

	// The catalog: topics, then one topic with what the caller may do.
	status, catalog := send("member", http.MethodGet, "/v1/development/workspace-operations", nil)
	if status != http.StatusOK || len(list(catalog["topics"])) != len(workspaceTopics) {
		t.Fatalf("catalog: %d %v", status, catalog)
	}
	status, guide := send("developer", http.MethodGet, "/v1/development/workspace-operations/node", nil)
	operations := list(guide["operations"])
	if status != http.StatusOK || len(operations) != 7 || operations[0].(map[string]any)["allowed"] != true || operations[1].(map[string]any)["allowed"] != false || operations[1].(map[string]any)["desktop_only"] != true {
		t.Fatalf("node guide: %d %v", status, guide)
	}
	if status, _ := send("developer", http.MethodGet, "/v1/development/workspace-operations/engine", nil); status != http.StatusNotFound {
		t.Fatalf("unknown topic: %d", status)
	}

	// What an operation is refused for before anything runs.
	if status, answer := apply("developer", map[string]any{"kind": "launch"}, nil); status != http.StatusUnprocessableEntity || answer["code"] != "operation_unknown" {
		t.Fatalf("unknown operation: %d %v", status, answer)
	}
	if status, answer := apply("developer", map[string]any{"kind": "node_add", "name": "研发一号机"}, nil); status != http.StatusForbidden {
		t.Fatalf("a developer added a node: %d %v", status, answer)
	}
	if status, answer := apply("member", map[string]any{"kind": "run_list"}, nil); status != http.StatusForbidden {
		t.Fatalf("an employee listed runs: %d %v", status, answer)
	}

	// Nodes: the token is handed back apart from the result, and never to a caller that cannot show it.
	if status, answer := send("admin", http.MethodPost, "/v1/development/workspace-operations", map[string]any{"operation": map[string]any{"kind": "node_add", "name": "研发一号机"}}); status != http.StatusConflict || answer["code"] != "desktop_only" {
		t.Fatalf("a secret was offered to a caller that cannot show it: %d %v", status, answer)
	}
	status, added := apply("admin", map[string]any{"kind": "node_add", "name": "研发一号机"}, nil)
	reveal := list(added["reveal"])
	if status != http.StatusOK || added["status"] != "done" || len(reveal) != 3 {
		t.Fatalf("node_add: %d %v", status, added)
	}
	token, _ := reveal[2].(map[string]any)["value"].(string)
	command, _ := reveal[0].(map[string]any)["value"].(string)
	if result, _ := json.Marshal(added["result"]); token == "" || strings.Contains(string(result), token) || !strings.Contains(command, host.URL+"/install.sh") || !strings.Contains(command, token) {
		t.Fatalf("node token handling: result %s, command %q", result, command)
	}
	if nodes := list(done("member", map[string]any{"kind": "node_list"}, nil)["nodes"]); len(nodes) != 1 || nodes[0].(map[string]any)["name"] != "研发一号机" || nodes[0].(map[string]any)["id"] != nil {
		t.Fatalf("node_list: %v", nodes)
	}
	done("admin", map[string]any{"kind": "node_rename", "node": "研发一号机", "name": "研发主机"}, nil)
	done("admin", map[string]any{"kind": "node_pause", "node": "研发主机", "paused": true}, nil)
	if nodes := list(done("admin", map[string]any{"kind": "node_list"}, nil)["nodes"]); nodes[0].(map[string]any)["name"] != "研发主机" || nodes[0].(map[string]any)["paused"] != true {
		t.Fatalf("renamed and paused node: %v", nodes)
	}
	if status, answer := apply("admin", map[string]any{"kind": "node_pause", "node": "研发一号机", "paused": false}, nil); status != http.StatusNotFound {
		t.Fatalf("a node that is gone: %d %v", status, answer)
	}
	status, rotated := apply("admin", map[string]any{"kind": "node_token", "node": "研发主机"}, nil)
	if status != http.StatusOK || rotated["status"] != "confirmation_required" || rotated["message"] == "" || rotated["title"] != "更换节点的接入令牌" || rotated["subject"] != "研发主机" {
		t.Fatalf("node_token without consent: %d %v", status, rotated)
	}
	if status, rotated = apply("admin", map[string]any{"kind": "node_token", "node": "研发主机"}, confirmed); status != http.StatusOK || len(list(rotated["reveal"])) != 3 || list(rotated["reveal"])[2].(map[string]any)["value"] == token {
		t.Fatalf("node_token: %d %v", status, rotated)
	}
	done("admin", map[string]any{"kind": "node_remove", "node": "研发主机"}, confirmed)
	if nodes := list(done("admin", map[string]any{"kind": "node_list"}, nil)["nodes"]); len(nodes) != 0 {
		t.Fatalf("removed node still listed: %v", nodes)
	}

	// Code repositories: the access token is asked for, never taken from the operation.
	for _, kind := range []string{"repository_list", "repository_save", "repository_archive", "code_task_submit", "run_code", "run_follow_up", "run_pull_request"} {
		if status, _ := apply("developer", map[string]any{"kind": kind}, nil); status < 400 {
			t.Fatalf("retired operation accepted: %s", kind)
		}
	}

	// Refreshing the catalog needs the person's own Forge sign-in, which only the desktop holds.
	if status, answer := apply("developer", map[string]any{"kind": "catalog_refresh"}, nil); status != http.StatusOK || answer["status"] != "needs_input" || answer["forge_session"] != true {
		t.Fatalf("catalog_refresh without a Forge sign-in: %d %v", status, answer)
	}

	// Archiving a team is asked about first, then done.
	status, created := send("developer", http.MethodPost, "/v1/development/teams", map[string]any{"request_id": uuid.NewString(), "name": "线索跟进团队", "objective": "整理销售线索材料"})
	teamID, _ := created["team_id"].(string)
	if status >= 300 || teamID == "" {
		t.Fatalf("create team: %d %v", status, created)
	}
	if status, answer := apply("developer", map[string]any{"kind": "feishu_team_read", "team": "没有这个团队"}, nil); status != http.StatusNotFound {
		t.Fatalf("a team that does not exist: %d %v", status, answer)
	}
	if access := done("developer", map[string]any{"kind": "feishu_team_read", "team_id": teamID}, nil); access["enabled"] != false || access["revision"] != nil {
		t.Fatalf("feishu_team_read: %v", access)
	}
	// A team is named the way the person names it.
	archive := map[string]any{"kind": "team_archive", "team": "线索跟进团队"}
	if status, answer := apply("developer", archive, nil); status != http.StatusOK || answer["status"] != "confirmation_required" || answer["title"] != "归档团队" || answer["subject"] != "线索跟进团队" {
		t.Fatalf("team_archive without consent: %d %v", status, answer)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM weave_teams WHERE workspace_id='ws' AND id=$1`, teamID).Scan(&state); err != nil || state != "active" {
		t.Fatalf("the team was touched before consent: %q %v", state, err)
	}
	done("developer", archive, confirmed)
	if err := pool.QueryRow(context.Background(), `SELECT status FROM weave_teams WHERE workspace_id='ws' AND id=$1`, teamID).Scan(&state); err != nil || state != "archived" {
		t.Fatalf("team after archive: %q %v", state, err)
	}

	// Runs and client sessions read through the same routes the console uses.
	if runs := done("developer", map[string]any{"kind": "run_list", "filter": "active"}, nil); runs["runs"] == nil {
		t.Fatalf("run_list: %v", runs)
	}
	if status, answer := apply("developer", map[string]any{"kind": "run_read", "run_id": "no-such-run"}, nil); status != http.StatusNotFound {
		t.Fatalf("a run that does not exist: %d %v", status, answer)
	}
	if sessions := done("member", map[string]any{"kind": "client_session_list"}, nil); sessions["sessions"] == nil {
		t.Fatalf("client_session_list: %v", sessions)
	}
}
