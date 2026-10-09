package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

func TestPullRequestOpensOncePerResultBranchRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	ctx := context.Background()
	var created atomic.Int32
	var received map[string]any
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret-token" || r.URL.Path != "/repos/acme/app/pulls" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		created.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/app/pull/7","number":7}`))
	}))
	defer github.Close()
	previous := githubAPIBase
	githubAPIBase = func(host string) string {
		if host != "github.com" {
			t.Errorf("api host = %s", host)
		}
		return github.URL
	}
	defer func() { githubAPIBase = previous }()

	server, pool := newTeamDispatchTestServer(t)
	call := func(handler echo.HandlerFunc, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		if id != "" {
			c.SetParamNames("id")
			c.SetParamValues(id)
		}
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		c.Set("roles", []string{"admin"})
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	saved := call(server.handleSaveEnvironment, "", `{"name":"应用","repository_url":"https://github.com/acme/app.git","default_branch":"main","verify_commands":["go test ./..."],"push_branches":true,"git_token":"s3cret-token"}`)
	var environment adminEnvironment
	if err := json.Unmarshal(saved.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	submitted := call(server.handleSubmitAdminTask, "", `{"team_id":"team","environment_id":"`+environment.ID+"\",\"task\":\"修复加法\",\"client_request_id\":\"00000000-0000-0000-0000-0000000000b1\"}")
	var run workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &run); err != nil || run.RunID == "" {
		t.Fatalf("submit = %d %s", submitted.Code, submitted.Body.String())
	}
	if refused := call(server.handleCreatePullRequest, run.RunID, ""); refused.Code != http.StatusConflict {
		t.Fatalf("running task = %d %s", refused.Code, refused.Body.String())
	}
	code, _ := server.runCodeWorkspace(ctx, "ws", run.RunID)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at,terminal_at)
		VALUES('ws',$1,'succeeded','team','flow',1,$1,'manual',$2,$3,$4,$4,$4)`, run.RunID, run.TaskID, "establish:"+run.RunID, now); err != nil {
		t.Fatal(err)
	}
	version := fmt.Sprintf(`{"schema_version":1,"repository":"https://github.com/acme/app.git","ref":"main","base_sha":"base","parent_sha":"base","head_sha":"head1","tree_sha":"tree","node_id":"verify","changed":true,"files":[],"patch":"none","push":{"branch":%q,"status":"pushed"}}`, code.PushBranch)
	evidence := `{"schema_version":1,"tree":"tree","tree_unchanged":true,"commit":"head1","node_id":"verify","commands":[{"command":"go test ./...","exit_code":0,"started_at":"2026-10-07T00:00:00Z","duration_ms":1,"output_sha256":"x","output_bytes":0,"output_tail":""}]}`
	artifacts, _ := json.Marshal([]map[string]string{
		{"path": "code/version.json", "content_type": "application/json", "content": version},
		{"path": "code/evidence.json", "content_type": "application/json", "content": evidence},
	})
	result, _ := json.Marshal(map[string]any{"artifacts": json.RawMessage(artifacts)})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,result,completed_at,actor_subject)
		VALUES('pr-stage','ws','worker','lead',1,'api','completed','engine_exec','agent',2,'team_free_collab',$1,'{"node_id":"verify"}'::jsonb,$2::jsonb,$3,'{"workspace_id":"ws","user_id":"user"}'::jsonb)`, run.RunID, string(result), now); err != nil {
		t.Fatal(err)
	}

	opened := call(server.handleCreatePullRequest, run.RunID, "")
	var pull taskPullRequest
	if opened.Code != http.StatusCreated || json.Unmarshal(opened.Body.Bytes(), &pull) != nil || pull.Number != 7 {
		t.Fatalf("open = %d %s", opened.Code, opened.Body.String())
	}
	if received["head"] != code.PushBranch || received["base"] != "main" || received["title"] != "修复加法" ||
		!strings.Contains(fmt.Sprint(received["body"]), "验证通过") || !strings.Contains(fmt.Sprint(received["body"]), "`head1`") {
		t.Fatalf("pull request request = %+v", received)
	}
	again := call(server.handleCreatePullRequest, run.RunID, "")
	if again.Code != http.StatusOK || created.Load() != 1 {
		t.Fatalf("second open = %d, created %d", again.Code, created.Load())
	}
	view, _, err := loadTaskCode(ctx, pool, "ws", run.RunID, "succeeded")
	if err != nil || view.PullRequest == nil || view.PullRequest.URL != "https://github.com/acme/app/pull/7" {
		t.Fatalf("code view pull request = %+v %v", view.PullRequest, err)
	}
}

func TestGitHubExistingPullRequestIsReused(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for acme:weave/x."}]}`))
			return
		}
		if r.URL.Query().Get("head") != "acme:weave/x" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"html_url":"https://github.com/acme/app/pull/3","number":3}]`))
	}))
	defer github.Close()
	pull, message, err := githubCreatePull(context.Background(), github.URL+"/repos/acme/app/pulls", "token", "acme", map[string]any{"head": "weave/x", "base": "main"})
	if err != nil || pull == nil || pull.Number != 3 {
		t.Fatalf("existing = %+v %q %v", pull, message, err)
	}
	if _, _, _, ok := githubRepository("git@github.com:acme/app.git"); ok {
		t.Fatal("ssh address accepted")
	}
	if host, owner, repo, ok := githubRepository("https://ghe.example.com/acme/app"); !ok || host != "ghe.example.com" || owner != "acme" || repo != "app" {
		t.Fatal("enterprise address rejected")
	}
}
