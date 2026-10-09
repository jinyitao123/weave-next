package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

func TestTaskCodeKeepsTheLatestPassOfALoopRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	call := func(handler echo.HandlerFunc, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		c.Set("roles", []string{"admin"})
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	created := call(server.handleSaveEnvironment, `{"name":"示例","repository_url":"https://example.com/app.git","default_branch":"main","verify_commands":["go test ./..."]}`)
	var environment adminEnvironment
	if err := json.Unmarshal(created.Body.Bytes(), &environment); err != nil {
		t.Fatal(err)
	}
	submitted := call(server.handleSubmitAdminTask, `{"team_id":"team","environment_id":"`+environment.ID+`","task":"修复","client_request_id":"00000000-0000-0000-0000-0000000000e1"}`)
	var run workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &run); err != nil || run.RunID == "" {
		t.Fatalf("submit = %d %s", submitted.Code, submitted.Body.String())
	}
	start := time.Now().UTC().Add(-time.Hour)
	stage := func(index int, node, head string, exitCode int) {
		t.Helper()
		version := fmt.Sprintf(`{"schema_version":1,"repository":"https://example.com/app.git","ref":"main","base_sha":"base","parent_sha":"base","head_sha":%q,"tree_sha":"tree","node_id":%q,"changed":true,"files":[],"patch":"none"}`, head, node)
		evidence := fmt.Sprintf(`{"schema_version":1,"tree":"tree","tree_unchanged":true,"commit":%q,"node_id":%q,"commands":[{"command":"go test ./...","exit_code":%d,"started_at":"2026-10-07T00:00:00Z","duration_ms":1,"output_sha256":"x","output_bytes":0,"output_tail":""}]}`, head, node, exitCode)
		artifacts, _ := json.Marshal([]map[string]string{
			{"path": "code/version.json", "content_type": "application/json", "content": version},
			{"path": "code/evidence.json", "content_type": "application/json", "content": evidence},
		})
		result, _ := json.Marshal(map[string]any{"artifacts": json.RawMessage(artifacts)})
		if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,result,completed_at,actor_subject)
			VALUES($1,'ws','worker','lead',1,'api','completed','engine_exec','agent',2,'team_free_collab',$2,$3::jsonb,$4::jsonb,$5,'{"workspace_id":"ws","user_id":"user"}'::jsonb)`,
			fmt.Sprintf("pass-%d", index), run.RunID, fmt.Sprintf(`{"node_id":%q}`, node), string(result), start.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Verification failed once, the work went back to coding, then passed.
	stage(1, "code", "head1", 1)
	stage(2, "verify", "head1", 1)
	stage(3, "code", "head2", 0)
	stage(4, "verify", "head2", 0)
	view, ok, err := loadTaskCode(ctx, pool, "ws", run.RunID, "succeeded")
	if err != nil || !ok {
		t.Fatalf("load = %v %v", ok, err)
	}
	if len(view.Stages) != 2 || view.Stages[0].Passes != 2 || view.Stages[1].Passes != 2 || view.Final == nil || view.Final.NodeID != "verify" || view.Final.Version.HeadSHA != "head2" {
		t.Fatalf("stages = %+v final = %+v", view.Stages, view.Final)
	}
	if view.Verdict.Status != "passed" || view.Verdict.Commit != "head2" {
		t.Fatalf("verdict = %+v", view.Verdict)
	}
	// A third coding pass that never reached verification is the final version.
	stage(5, "code", "head3", 0)
	view, _, _ = loadTaskCode(ctx, pool, "ws", run.RunID, "failed")
	if view.Final == nil || view.Final.NodeID != "code" || view.Final.Version.HeadSHA != "head3" || view.Stages[0].Passes != 3 {
		t.Fatalf("final after unverified pass = %+v", view.Final)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(request, recorder)
	c.SetParamNames("id")
	c.SetParamValues(run.RunID)
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	c.Set("roles", []string{"admin"})
	if err := server.handleExportAdminTaskEvidence(c); err != nil {
		t.Fatal(err)
	}
	var export struct {
		Format        string          `json:"format"`
		Content       json.RawMessage `json:"content"`
		ContentSHA256 string          `json:"content_sha256"`
	}
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Disposition"), "attachment;") || json.Unmarshal(recorder.Body.Bytes(), &export) != nil || export.Format != "weave-task-evidence" {
		t.Fatalf("export = %d %s", recorder.Code, recorder.Body.String())
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, export.Content); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(compact.Bytes())
	if hex.EncodeToString(sum[:]) != export.ContentSHA256 || !strings.Contains(compact.String(), `"head_sha":"head3"`) || !strings.Contains(compact.String(), `"title":"修复"`) {
		t.Fatalf("export content = %s", compact.String())
	}
}
