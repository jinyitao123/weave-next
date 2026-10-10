package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

func TestAdminTaskListShowsReadableTasksWithinRunVisibilityRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	dispatch := func(user, clientRequestID, task string) workflowManualRunResponse {
		t.Helper()
		body, _ := json.Marshal(teamDispatchRequest{Task: task, ClientRequestID: clientRequestID})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/teams/team/dispatch", bytes.NewReader(body))
		request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: user}))
		c := echo.New().NewContext(request, recorder)
		c.SetParamNames("id")
		c.SetParamValues("team")
		c.Set("tenant", "ws")
		c.Set("user_id", user)
		if err := server.handleDispatchTeam(c); err != nil || recorder.Code != http.StatusCreated {
			t.Fatalf("dispatch = %d %s %v", recorder.Code, recorder.Body.String(), err)
		}
		var result workflowManualRunResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	list := func(user, role, query string) (int, []adminTask) {
		t.Helper()
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/admin/tasks"+query, nil), recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", user)
		c.Set("roles", []string{role})
		if err := server.handleListAdminTasks(c); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Tasks []adminTask `json:"tasks"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body.Tasks
	}

	mine := dispatch("user", "00000000-0000-0000-0000-0000000000a1", "为导出接口增加游标分页\n保持向后兼容")
	theirs := dispatch("user-a", "00000000-0000-0000-0000-0000000000a2", "修复时区解析")

	code, tasks := list("user", "admin", "")
	if code != http.StatusOK || len(tasks) != 2 {
		t.Fatalf("admin list = %d %#v", code, tasks)
	}
	byRun := map[string]adminTask{}
	for _, task := range tasks {
		byRun[task.RunID] = task
	}
	if got := byRun[mine.RunID]; got.Title != "为导出接口增加游标分页" || got.TeamName != "team" || got.WorkflowID != "flow" || got.WorkflowVersion != 1 || got.Status == "" {
		t.Fatalf("own task = %#v", got)
	}
	if _, member := list("user-b", "member", ""); len(member) != 0 {
		t.Fatalf("member sees others' tasks: %#v", member)
	}
	if _, own := list("user-a", "member", ""); len(own) != 1 || own[0].RunID != theirs.RunID {
		t.Fatalf("submitter list = %#v", own)
	}

	// Administrators read every run of the workspace, including the one an
	// employee started from the desktop; a developer still reads only their own.
	// Admission records are immutable, so the employee desktop submission is
	// its own record carrying the desktop input revision.
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,actor_subject,request_digest,request,target)
		VALUES('ws','desktop-request','{"workspace_id":"ws","user_id":"user-a"}',repeat('b',64),
		'{"run_id":"desktop-run","input":"员工桌面任务","revision":{"workflow_id":"flow","workflow_version":1}}',
		'{"team_id":"team","input_revision_id":"desktop-input"}')`); err != nil {
		t.Fatal(err)
	}
	if _, admin := list("user", "admin", ""); len(admin) != 3 || admin[0].RunID != "desktop-run" || admin[0].Source != "desktop" || admin[0].Mine {
		t.Fatalf("admin list after desktop submission = %#v", admin)
	}
	if _, owner := list("user", "owner", "?source=desktop"); len(owner) != 1 || owner[0].RunID != "desktop-run" {
		t.Fatalf("owner desktop filter = %#v", owner)
	}
	if _, console := list("user", "admin", "?source=console&team=team"); len(console) != 2 || console[0].Source != "console" {
		t.Fatalf("console filter = %#v", console)
	}
	if _, none := list("user", "admin", "?team=another-team"); len(none) != 0 {
		t.Fatalf("team filter = %#v", none)
	}
	if code, _ := list("user", "admin", "?source=elsewhere"); code != http.StatusBadRequest {
		t.Fatalf("unknown source = %d", code)
	}
	if _, developer := list("user-b", "developer", ""); len(developer) != 0 {
		t.Fatalf("developer sees runs they did not submit: %#v", developer)
	}
	if _, own := list("user-a", "member", ""); len(own) != 2 || own[0].RunID != "desktop-run" || own[0].Status != "queued" || own[0].Title != "员工桌面任务" {
		t.Fatalf("employee desktop run = %#v", own)
	}

	read := func(user, role, runID string) int {
		t.Helper()
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/admin/tasks/"+runID, nil), recorder)
		c.SetParamNames("id")
		c.SetParamValues(runID)
		c.Set("tenant", "ws")
		c.Set("user_id", user)
		c.Set("roles", []string{role})
		if err := server.handleListAdminTasks(c); err != nil {
			t.Fatal(err)
		}
		return recorder.Code
	}
	if read("user", "admin", mine.RunID) != http.StatusOK || read("user-b", "member", theirs.RunID) != http.StatusNotFound || read("user-a", "member", theirs.RunID) != http.StatusOK ||
		read("user", "admin", "desktop-run") != http.StatusOK || read("user-b", "developer", "desktop-run") != http.StatusNotFound || read("user-a", "member", "desktop-run") != http.StatusOK {
		t.Fatal("single task read does not follow list visibility")
	}
	if code, _ := list("user", "admin", "?filter=unknown"); code != http.StatusBadRequest {
		t.Fatalf("unknown filter = %d", code)
	}
	if _, done := list("user", "admin", "?filter=done"); len(done) != 0 {
		t.Fatalf("queued task listed as done: %#v", done)
	}
	if _, active := list("user", "admin", "?filter=active"); len(active) != 3 {
		t.Fatalf("active filter = %#v", active)
	}
	if code, _ := list("user", "admin", "?before=yesterday"); code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d", code)
	}
}
