package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

func TestFollowUpStartsFromThePreviousDeliveredCommitRealPG(t *testing.T) {
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
	created := call(server.handleSaveEnvironment, http.MethodPost, "", `{"name":"示例","repository_url":"https://example.com/demo.git","default_branch":"main","verify_commands":["go test ./..."],"push_branches":true}`)
	var environment adminEnvironment
	if err := json.Unmarshal(created.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	submitted := call(server.handleSubmitAdminTask, http.MethodPost, "", `{"team_id":"team","environment_id":"`+environment.ID+`","task":"修复加法","client_request_id":"00000000-0000-0000-0000-0000000000d1"}`)
	var parent workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &parent); err != nil || submitted.Code != http.StatusCreated {
		t.Fatalf("submit = %d %s", submitted.Code, submitted.Body.String())
	}
	followUp := `{"task":"再补一个减法函数","client_request_id":"00000000-0000-0000-0000-0000000000d2"}`
	if refused := call(server.handleSubmitFollowUp, http.MethodPost, parent.RunID, followUp); refused.Code != http.StatusConflict {
		t.Fatalf("follow-up of a running task = %d %s", refused.Code, refused.Body.String())
	}

	// The run finished and its last stage handed over a Host-written version.
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at,terminal_at)
		VALUES('ws',$1,'succeeded','team','flow',1,$1,'manual',$2,$3,$4,$4,$4)`, parent.RunID, parent.TaskID, "establish:"+parent.RunID, now); err != nil {
		t.Fatal(err)
	}
	version := `{"schema_version":1,"repository":"https://example.com/demo.git","ref":"main","base_sha":"base1","parent_sha":"base1","head_sha":"head1","tree_sha":"tree1","node_id":"verify","changed":true,"files":[{"path":"calc.py","added":1,"deleted":1}],"patch":"complete"}`
	patch := "diff --git a/calc.py b/calc.py\n"
	artifacts, _ := json.Marshal([]map[string]string{
		{"path": "code/version.json", "content_type": "application/json", "content": version},
		{"path": "code/changes.patch", "content_type": "text/x-diff", "content": patch},
	})
	result, _ := json.Marshal(map[string]any{"artifacts": json.RawMessage(artifacts)})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,result,completed_at,actor_subject)
		VALUES('stage-verify','ws','worker','lead',1,'api','completed','engine_exec','agent',2,'team_free_collab',$1,'{"node_id":"verify"}'::jsonb,$2::jsonb,$3,'{"workspace_id":"ws","user_id":"user"}'::jsonb)`, parent.RunID, string(result), now); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}

	accepted := call(server.handleSubmitFollowUp, http.MethodPost, parent.RunID, followUp)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("follow-up = %d %s", accepted.Code, accepted.Body.String())
	}
	var next workflowManualRunResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	code, err := server.runCodeWorkspace(ctx, "ws", next.RunID)
	if err != nil || code == nil || code.Seed == nil {
		t.Fatalf("follow-up code context = %+v %v", code, err)
	}
	if code.Ref != "base1" || code.Seed.Version != version || code.Seed.Patch != patch || len(code.VerifyCommands) != 1 {
		t.Fatalf("follow-up did not freeze the delivered version: %+v %+v", code, code.Seed)
	}
	parentCode, _ := server.runCodeWorkspace(ctx, "ws", parent.RunID)
	if code.PushBranch == "" || code.PushBranch != parentCode.PushBranch {
		t.Fatalf("follow-up pushes to %q, task branch is %q", code.PushBranch, parentCode.PushBranch)
	}
	if replay := call(server.handleSubmitFollowUp, http.MethodPost, parent.RunID, followUp); replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), next.RunID) {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
	tasks := call(server.handleListAdminTasks, http.MethodGet, next.RunID, "")
	if !strings.Contains(tasks.Body.String(), `"title":"跟进：再补一个减法函数"`) {
		t.Fatalf("follow-up title = %s", tasks.Body.String())
	}

	thread := call(server.handleGetTaskThread, http.MethodGet, next.RunID, "")
	var body struct {
		Runs []adminTask `json:"runs"`
	}
	if err := json.Unmarshal(thread.Body.Bytes(), &body); err != nil || len(body.Runs) != 2 || body.Runs[0].RunID != parent.RunID || body.Runs[1].RunID != next.RunID {
		t.Fatalf("thread = %d %s", thread.Code, thread.Body.String())
	}

	plain := call(server.handleSubmitAdminTask, http.MethodPost, "", `{"team_id":"team","task":"写周报","client_request_id":"00000000-0000-0000-0000-0000000000d3"}`)
	var plainRun workflowManualRunResponse
	_ = json.Unmarshal(plain.Body.Bytes(), &plainRun)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at,terminal_at)
		VALUES('ws',$1,'succeeded','team','flow',1,$1,'manual',$2,$3,$4,$4,$4)`, plainRun.RunID, plainRun.TaskID, "establish:"+plainRun.RunID, now); err != nil {
		t.Fatal(err)
	}
	if refused := call(server.handleSubmitFollowUp, http.MethodPost, plainRun.RunID, `{"task":"继续","client_request_id":"00000000-0000-0000-0000-0000000000d4"}`); refused.Code != http.StatusConflict {
		t.Fatalf("follow-up without code = %d %s", refused.Code, refused.Body.String())
	}
}

func TestFollowUpInputLeadsWithTheFollowUp(t *testing.T) {
	input := followUpInput("补一个减法", "修复加法")
	if !strings.HasPrefix(input, "跟进：补一个减法\n") || !strings.Contains(input, "原始要求：\n修复加法") {
		t.Fatalf("input = %q", input)
	}
}
