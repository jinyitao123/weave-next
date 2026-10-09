package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

func TestEnvironmentSubmissionFreezesTheRunCodeContextRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
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

	for _, invalid := range []string{
		`{"name":"x","repository_url":"https://user:secret@example.com/repo.git","default_branch":"main"}`,
		`{"name":"x","repository_url":"not a url","default_branch":"main"}`,
		`{"name":"x","repository_url":"https://example.com/repo.git","default_branch":"../main"}`,
		`{"name":"x","repository_url":"https://example.com/repo.git","default_branch":"main","verify_commands":["go test\nrm -rf /"]}`,
	} {
		if recorder := call(server.handleSaveEnvironment, http.MethodPost, "", invalid); recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid environment accepted (%d): %s", recorder.Code, invalid)
		}
	}
	created := call(server.handleSaveEnvironment, http.MethodPost, "", `{"name":"支付服务","repository_url":"git@example.com:team/payments.git","default_branch":"main","setup_script":"go mod download","verify_commands":["go test ./...","","go vet ./..."],"push_branches":true,"default_team_id":"team"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create environment = %d %s", created.Code, created.Body.String())
	}
	var environment adminEnvironment
	if err := json.Unmarshal(created.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	if len(environment.VerifyCommands) != 2 || environment.DefaultTeamID != "team" {
		t.Fatalf("environment = %+v", environment)
	}
	if duplicate := call(server.handleSaveEnvironment, http.MethodPost, "", `{"name":"支付服务","repository_url":"https://example.com/x.git","default_branch":"main"}`); duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d", duplicate.Code)
	}
	listed := call(server.handleListEnvironments, http.MethodGet, "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "支付服务") {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}

	submission := `{"team_id":"team","environment_id":"` + environment.ID + `","ref":"feature/cursor","task":"为导出接口增加分页","client_request_id":"00000000-0000-0000-0000-0000000000c1"}`
	first := call(server.handleSubmitAdminTask, http.MethodPost, "", submission)
	if first.Code != http.StatusCreated {
		t.Fatalf("submit = %d %s", first.Code, first.Body.String())
	}
	var receipt workflowManualRunResponse
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	code, err := server.runCodeWorkspace(ctx, "ws", receipt.RunID)
	if err != nil || code == nil {
		t.Fatalf("run code context = %+v %v", code, err)
	}
	if code.Repository != "git@example.com:team/payments.git" || code.Ref != "feature/cursor" || code.SetupScript != "go mod download" ||
		len(code.VerifyCommands) != 2 || !strings.HasPrefix(code.PushBranch, "weave/") {
		t.Fatalf("frozen code context = %+v", code)
	}
	// An identical replay returns the same run and keeps the context.
	if replay := call(server.handleSubmitAdminTask, http.MethodPost, "", submission); replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), receipt.RunID) {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	// Reusing the request id with another branch is refused.
	changed := strings.Replace(submission, "feature/cursor", "main", 1)
	if conflict := call(server.handleSubmitAdminTask, http.MethodPost, "", changed); conflict.Code != http.StatusConflict {
		t.Fatalf("changed replay = %d %s", conflict.Code, conflict.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_run_code_contexts SET ref='main' WHERE workspace_id='ws' AND run_id=$1`, receipt.RunID); err == nil {
		t.Fatal("run code context was mutable")
	}
	// Editing the environment does not change a submitted run.
	if updated := call(server.handleSaveEnvironment, http.MethodPut, environment.ID, `{"name":"支付服务","repository_url":"https://example.com/other.git","default_branch":"main"}`); updated.Code != http.StatusOK {
		t.Fatalf("update = %d %s", updated.Code, updated.Body.String())
	}
	if again, _ := server.runCodeWorkspace(ctx, "ws", receipt.RunID); again.Repository != code.Repository {
		t.Fatalf("submitted run followed the edited environment: %+v", again)
	}
	// A task without an environment carries no code context.
	plain := call(server.handleSubmitAdminTask, http.MethodPost, "", `{"team_id":"team","task":"写周报","client_request_id":"00000000-0000-0000-0000-0000000000c2"}`)
	var plainReceipt workflowManualRunResponse
	if err := json.Unmarshal(plain.Body.Bytes(), &plainReceipt); err != nil || plain.Code != http.StatusCreated {
		t.Fatalf("plain submit = %d %s", plain.Code, plain.Body.String())
	}
	if none, err := server.runCodeWorkspace(ctx, "ws", plainReceipt.RunID); err != nil || none != nil {
		t.Fatalf("plain task gained code context: %+v %v", none, err)
	}
	if archived := call(server.handleArchiveEnvironment, http.MethodDelete, environment.ID, ""); archived.Code != http.StatusNoContent {
		t.Fatalf("archive = %d", archived.Code)
	}
	if missing := call(server.handleSubmitAdminTask, http.MethodPost, "", strings.Replace(submission, "0000000000c1", "0000000000c3", 1)); missing.Code != http.StatusNotFound {
		t.Fatalf("archived environment accepted: %d", missing.Code)
	}
}
