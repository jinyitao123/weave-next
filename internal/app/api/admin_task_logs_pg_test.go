package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/labstack/echo/v4"
)

func TestTaskLogsAreReadableAndSearchableWithinRunVisibilityRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	call := func(handler echo.HandlerFunc, method, id, query, body string, user string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/?"+query, bytes.NewReader([]byte(body)))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: user}))
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.SetParamNames("id")
		c.SetParamValues(id)
		c.Set("tenant", "ws")
		c.Set("user_id", user)
		c.Set("roles", []string{"member"})
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	submitted := call(server.handleSubmitAdminTask, http.MethodPost, "", "", `{"team_id":"team","task":"跑测试","client_request_id":"00000000-0000-0000-0000-0000000000e1"}`, "user")
	var run workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,actor_subject,created_at)
		VALUES('stage-code','ws','worker','lead',1,'api','running','engine_exec','agent',2,'team_free_collab',$1,'{"node_id":"code"}'::jsonb,'{"workspace_id":"ws","user_id":"user"}'::jsonb,$2)`, run.RunID, now); err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for seq, text := range []string{"$ go test ./...", "--- FAIL: TestAdd (0.00s)", "ok  	example/calc", "FAIL"} {
		lines = append(lines, `('ws','stage-code','command-1',`+string(rune('1'+seq))+`,now(),'`+strings.ReplaceAll(text, "'", "''")+`')`)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_logs(workspace_id,task_id,stream,seq,occurred_at,text) VALUES `+strings.Join(lines, ",")); err != nil {
		t.Fatal(err)
	}
	read := func(query, user string) []adminLogLine {
		t.Helper()
		recorder := call(server.handleGetAdminTaskLogs, http.MethodGet, run.RunID, query, "", user)
		if recorder.Code != http.StatusOK {
			t.Fatalf("logs %q = %d %s", query, recorder.Code, recorder.Body.String())
		}
		var body struct {
			Lines []adminLogLine `json:"lines"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return body.Lines
	}
	if all := read("", "user"); len(all) != 4 || all[0].NodeID != "code" || all[3].Seq != 4 {
		t.Fatalf("all lines = %+v", all)
	}
	if failed := read("q=fail", "user"); len(failed) != 2 {
		t.Fatalf("search = %+v", failed)
	}
	if literal := read("q=%25", "user"); len(literal) != 0 {
		t.Fatalf("wildcard was not escaped: %+v", literal)
	}
	if page := read("after=2&stream=command-1&node=code", "user"); len(page) != 2 || page[0].Seq != 3 {
		t.Fatalf("page = %+v", page)
	}
	summary := call(server.handleGetAdminTaskLogs, http.MethodGet, run.RunID, "summary=1", "", "user")
	if !strings.Contains(summary.Body.String(), `"stream":"command-1","lines":4,"last_seq":4`) {
		t.Fatalf("summary = %s", summary.Body.String())
	}
	if other := call(server.handleGetAdminTaskLogs, http.MethodGet, run.RunID, "", "", "user-b"); other.Code != http.StatusNotFound {
		t.Fatalf("another member read the logs: %d", other.Code)
	}

	// The stream ends once the run has finished.
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at,terminal_at)
		VALUES('ws',$1,'succeeded','team','flow',1,$1,'manual',$2,$3,$4,$4,$4)`, run.RunID, run.TaskID, "establish:"+run.RunID, now); err != nil {
		t.Fatal(err)
	}
	streamed := call(server.handleAdminTaskStream, http.MethodGet, run.RunID, "", "", "user")
	scanner := bufio.NewScanner(strings.NewReader(streamed.Body.String()))
	events := []string{}
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: ") {
			events = append(events, strings.TrimPrefix(scanner.Text(), "event: "))
		}
	}
	if strings.Join(events, ",") != "update,end" || streamed.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream events = %v (%s)", events, streamed.Header().Get("Content-Type"))
	}
}

func TestTaskLogLineValidation(t *testing.T) {
	valid := runtimeprotocol.TaskLogLine{Stream: "command-1", Seq: 1, OccurredAt: time.Now(), Text: "ok"}
	if runtimeprotocol.ValidateTaskLogLine(valid) != nil {
		t.Fatal("valid line refused")
	}
	for name, mutate := range map[string]func(*runtimeprotocol.TaskLogLine){
		"zero seq":     func(line *runtimeprotocol.TaskLogLine) { line.Seq = 0 },
		"bad stream":   func(line *runtimeprotocol.TaskLogLine) { line.Stream = "Command 1" },
		"empty stream": func(line *runtimeprotocol.TaskLogLine) { line.Stream = "" },
		"too long": func(line *runtimeprotocol.TaskLogLine) {
			line.Text = strings.Repeat("x", runtimeprotocol.TaskLogLineBytes+1)
		},
		"no timestamp":  func(line *runtimeprotocol.TaskLogLine) { line.OccurredAt = time.Time{} },
		"invalid utf-8": func(line *runtimeprotocol.TaskLogLine) { line.Text = string([]byte{0xff}) },
	} {
		line := valid
		mutate(&line)
		if runtimeprotocol.ValidateTaskLogLine(line) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
