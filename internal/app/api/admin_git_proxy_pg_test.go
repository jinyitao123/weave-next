package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func gitFixture(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = dir
	command.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost"), env...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestGitProxyServesOnlyTheRunRepositoryAndResultBranchRealPG(t *testing.T) {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git unavailable")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		if _, err := os.Stat(backend + ".exe"); err != nil {
			t.Skip("git-http-backend unavailable")
		}
		backend += ".exe"
	}
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("31", 32))
	ctx := context.Background()

	// The upstream accepts only the environment's credential.
	projects := t.TempDir()
	bare := filepath.Join(projects, "app.git")
	gitFixture(t, projects, nil, "init", "--bare", "--initial-branch=main", bare)
	gitFixture(t, bare, nil, "config", "http.receivepack", "true")
	work := t.TempDir()
	gitFixture(t, work, nil, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, work, nil, "add", ".")
	gitFixture(t, work, nil, "commit", "-m", "init")
	gitFixture(t, work, nil, "push", bare, "main")
	gitBackend := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, token, ok := r.BasicAuth(); !ok || user != "x-access-token" || token != "s3cret-token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gitBackend.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	server, pool := newTeamDispatchTestServer(t)
	server.Pool = pool
	server.Runtimes = runtimes.NewStore(pool)
	server.Echo = echo.New()
	host := server.Echo.Group("/v1/runtime", server.runtimeAuthMiddleware())
	host.GET("/tasks/:id/git/*", server.handleRuntimeGitProxy)
	host.POST("/tasks/:id/git/*", server.handleRuntimeGitProxy)
	weave := httptest.NewServer(server.Echo)
	defer weave.Close()

	call := func(handler echo.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/", bytes.NewReader([]byte(body)))
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
	repository := upstream.URL + "/app.git"
	saved := call(server.handleSaveEnvironment, http.MethodPost, "", fmt.Sprintf(`{"name":"应用","repository_url":%q,"default_branch":"main","push_branches":true,"git_token":"s3cret-token"}`, repository))
	var environment adminEnvironment
	if err := json.Unmarshal(saved.Body.Bytes(), &environment); err != nil || !environment.GitCredential || strings.Contains(saved.Body.String(), "s3cret") {
		t.Fatalf("save = %d %s", saved.Code, saved.Body.String())
	}
	if rejected := call(server.handleSaveEnvironment, http.MethodPost, "", `{"name":"ssh","repository_url":"git@example.com:a/b.git","default_branch":"main","git_token":"x"}`); rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ssh token = %d", rejected.Code)
	}
	submitted := call(server.handleSubmitAdminTask, http.MethodPost, "", fmt.Sprintf(`{"team_id":"team","environment_id":%q,"task":"改代码","client_request_id":"00000000-0000-0000-0000-0000000000a1"}`, environment.ID))
	var run workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &run); err != nil || run.RunID == "" {
		t.Fatalf("submit = %d %s", submitted.Code, submitted.Body.String())
	}
	code, err := server.runCodeWorkspace(ctx, "ws", run.RunID)
	if err != nil || code == nil || !code.Proxy || code.PushBranch == "" {
		t.Fatalf("code workspace = %+v %v", code, err)
	}

	node, token, err := server.Runtimes.Create(ctx, "ws", "mac-studio")
	if err != nil {
		t.Fatal(err)
	}
	subject := execution.Subject{WorkspaceID: "ws", UserID: "user"}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,actor_subject,runtime_id,worker_id,claim_epoch,lease_expires_at)
		VALUES('git-task','ws','worker','lead',1,'api','running','engine_exec','agent',2,'team_free_collab',$1,'{"node_id":"code","engine":"claude"}'::jsonb,'{"workspace_id":"ws","user_id":"user"}'::jsonb,$2,$3,1,$4)`,
		run.RunID, node.ID, runtimes.RuntimeWorkerID("ws", node.ID), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	headers := []string{"Authorization: Bearer " + token, runtimeprotocol.HeaderVersion + ": " + runtimeprotocol.ProtocolVersion, "X-Weave-Task-Epoch: 1", "X-Weave-Task-Subject: " + subject.Digest()}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(headers))}
	for index, header := range headers {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.extraHeader", index), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, header))
	}
	proxy := weave.URL + "/v1/runtime/tasks/git-task/git"

	checkout := filepath.Join(t.TempDir(), "checkout")
	gitFixture(t, filepath.Dir(checkout), env, "clone", "--", proxy, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, checkout, nil, "add", ".")
	gitFixture(t, checkout, nil, "commit", "-m", "fix")
	gitFixture(t, checkout, env, "push", "--", proxy, "HEAD:refs/heads/"+code.PushBranch)
	if head := gitFixture(t, bare, nil, "rev-parse", "refs/heads/"+code.PushBranch); head != gitFixture(t, checkout, nil, "rev-parse", "HEAD") {
		t.Fatalf("pushed head = %s", head)
	}

	refused := exec.Command("git", "push", "--", proxy, "HEAD:refs/heads/main")
	refused.Dir = checkout
	refused.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	if output, err := refused.CombinedOutput(); err == nil {
		t.Fatalf("push to main through the proxy succeeded: %s", output)
	}
	if main := gitFixture(t, bare, nil, "rev-parse", "refs/heads/main"); main == gitFixture(t, checkout, nil, "rev-parse", "HEAD") {
		t.Fatal("main moved")
	}

	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='completed' WHERE id='git-task'`); err != nil {
		t.Fatal(err)
	}
	stale := exec.Command("git", "ls-remote", "--", proxy)
	stale.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	if output, err := stale.CombinedOutput(); err == nil {
		t.Fatalf("finished task still reached the repository: %s", output)
	}
}
