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

func TestAdminHTTPRolesAndRetiredRepositoriesRealPG(t *testing.T) {
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

	for _, path := range []string{"/v1/environments", "/v1/admin/tasks/missing/code", "/v1/admin/tasks/missing/thread", "/v1/admin/tasks/missing/follow-ups", "/v1/admin/tasks/missing/pull-request"} {
		for _, role := range []string{"member", "developer", "admin"} {
			method := http.MethodGet
			if strings.HasSuffix(path, "follow-ups") || strings.HasSuffix(path, "pull-request") {
				method = http.MethodPost
			}
			if status, body := call(role, method, path, map[string]string{}); status != http.StatusNotFound {
				t.Fatalf("retired route %s: %d %s", path, status, body)
			}
		}
	}
	if status, _ := call("member", http.MethodPost, "/v1/admin/tasks", map[string]string{}); status != http.StatusForbidden {
		t.Fatalf("member submission: %d", status)
	}
	counts := func() string {
		t.Helper()
		var environments, contexts, admissions, tasks int
		if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_environments),(SELECT count(*) FROM weave_run_code_contexts),(SELECT count(*) FROM weave_workflow_admission_requests),(SELECT count(*) FROM weave_task_queue)`).Scan(&environments, &contexts, &admissions, &tasks); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(environments, contexts, admissions, tasks)
	}
	request := map[string]string{"team_id": "team", "task": "inspect the supplied report", "client_request_id": uuid.NewString()}
	if status, body := call("developer", http.MethodPost, "/v1/admin/tasks", request); status != http.StatusCreated {
		t.Fatalf("plain submission: %d %s", status, body)
	}
	before := counts()
	if status, body := call("developer", http.MethodPost, "/v1/admin/tasks", request); status != http.StatusOK {
		t.Fatalf("plain replay: %d %s", status, body)
	}
	if counts() != before {
		t.Fatal("replay appended execution facts")
	}
	request["task"] = "different input"
	if status, _ := call("developer", http.MethodPost, "/v1/admin/tasks", request); status != http.StatusConflict {
		t.Fatalf("changed input: %d", status)
	}
	request["environment_id"] = "retired"
	if status, _ := call("developer", http.MethodPost, "/v1/admin/tasks", request); status < 400 {
		t.Fatalf("retired repository input accepted: %d", status)
	}
}
