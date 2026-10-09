package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/labstack/echo/v4"
)

func TestAdminHTTPRolesAndCodeReplayRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.Config, s.UserStore = echo.New(), &config.Config{JWTSecret: "console-role-test"}, users.NewStore(pool)
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()
	call := func(role, method, path string, body any) (int, []byte) {
		t.Helper()
		token, err := s.signJWTWithPermissionSets("ws", "user", []string{role}, "forge", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, host.URL+path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	// Real route authentication must refuse the member before handlers write or
	// expose environment credentials, task logs, code, or pull-request actions.
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/environments"}, {"POST", "/v1/environments"}, {"PUT", "/v1/environments/missing"}, {"DELETE", "/v1/environments/missing"},
		{"POST", "/v1/admin/tasks"}, {"GET", "/v1/admin/tasks"}, {"GET", "/v1/admin/tasks/missing/code"},
		{"GET", "/v1/admin/tasks/missing/evidence"}, {"GET", "/v1/admin/tasks/missing/logs"}, {"GET", "/v1/admin/tasks/missing/stream"},
		{"POST", "/v1/admin/tasks/missing/follow-ups"}, {"POST", "/v1/admin/tasks/missing/pull-request"},
	} {
		if status, body := call("member", route.method, route.path, map[string]string{}); status != 403 {
			t.Fatalf("member %s %s: %d %s", route.method, route.path, status, body)
		}
	}
	counts := func() string {
		t.Helper()
		var environments, contexts, admissions, tasks int
		err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_environments),(SELECT count(*) FROM weave_run_code_contexts),(SELECT count(*) FROM weave_workflow_admission_requests),(SELECT count(*) FROM weave_task_queue)`).Scan(&environments, &contexts, &admissions, &tasks)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(environments, contexts, admissions, tasks)
	}
	if counts() != "0 0 0 0" {
		t.Fatalf("member wrote data: %s", counts())
	}
	envs := []string{}
	for _, role := range []string{"developer", "admin"} {
		status, body := call(role, "POST", "/v1/environments", map[string]any{"name": role, "repository_url": "https://example.com/repo.git", "default_branch": "main", "verify_commands": []string{"true"}, "push_branches": true})
		if status != 200 {
			t.Fatalf("%s environment: %d %s", role, status, body)
		}
		var env adminEnvironment
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatal(err)
		}
		envs = append(envs, env.ID)
	}
	submit := func(environment, ref, id string) map[string]string {
		return map[string]string{"team_id": "team", "task": "检查代码", "environment_id": environment, "ref": ref, "client_request_id": id}
	}
	for _, scenario := range []struct{ name, firstEnv, firstRef, nextEnv, nextRef string }{
		{"add environment", "", "", envs[0], ""}, {"remove environment", envs[0], "", "", ""},
		{"change environment", envs[0], "", envs[1], ""}, {"change ref", envs[0], "main", envs[0], "feature"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			id := uuid.NewString()
			request := submit(scenario.firstEnv, scenario.firstRef, id)
			if status, body := call("developer", "POST", "/v1/admin/tasks", request); status != 201 {
				t.Fatalf("submit %d %s", status, body)
			}
			before := counts()
			if status, body := call("developer", "POST", "/v1/admin/tasks", submit(scenario.nextEnv, scenario.nextRef, id)); status != 409 {
				t.Fatalf("conflict %d %s", status, body)
			}
			if counts() != before {
				t.Fatal("conflict appended code or execution facts")
			}
			if status, body := call("developer", "POST", "/v1/admin/tasks", request); status != 200 {
				t.Fatalf("replay %d %s", status, body)
			}
			if counts() != before {
				t.Fatal("replay appended code or execution facts")
			}
		})
	}
	// Even after environment edits, the same omitted ref replays its frozen input.
	id := uuid.NewString()
	request := submit(envs[0], "", id)
	if status, body := call("admin", "POST", "/v1/admin/tasks", request); status != 201 {
		t.Fatalf("submit %d %s", status, body)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_environments SET default_branch='other',repository_url='https://example.com/other.git' WHERE id=$1`, envs[0]); err != nil {
		t.Fatal(err)
	}
	before := counts()
	if status, body := call("admin", "POST", "/v1/admin/tasks", request); status != 200 {
		t.Fatalf("frozen replay %d %s", status, body)
	}
	if counts() != before {
		t.Fatal("edited environment changed the frozen request")
	}
}
