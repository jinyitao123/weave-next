package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func TestTaskQueueExplainsWaitingAndMetricsMeasureFirstOutputRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	server.Runtimes = runtimes.NewStore(pool)
	call := func(handler echo.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/", bytes.NewReader([]byte(body)))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.SetParamNames("id")
		c.SetParamValues(id)
		c.Set("tenant", "ws")
		c.Set("user_id", "user")
		c.Set("roles", []string{"admin"})
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	wait := func(runID string) adminTaskWait {
		t.Helper()
		var body adminTaskWait
		recorder := call(server.handleGetAdminTaskQueue, http.MethodGet, runID, "")
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("queue = %d %s", recorder.Code, recorder.Body.String())
		}
		return body
	}
	submitted := call(server.handleSubmitAdminTask, http.MethodPost, "", `{"team_id":"team","task":"跑测试","client_request_id":"00000000-0000-0000-0000-0000000000f1"}`)
	var run workflowManualRunResponse
	if err := json.Unmarshal(submitted.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if got := wait(run.RunID); !got.Waiting || got.Reason != "team_pickup" {
		t.Fatalf("before pickup = %+v", got)
	}
	node, _, err := server.Runtimes.Create(ctx, "ws", "mac-studio")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insert := func(id, snapshot string, created time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,actor_subject,created_at,runtime_id)
			VALUES($1,'ws','worker','lead',1,'api','queued','engine_exec','agent',2,'team_free_collab',$2,'{"node_id":"code","engine":"claude"}'::jsonb,'{"workspace_id":"ws","user_id":"user"}'::jsonb,$3,$4)`, id, snapshot, created, node.ID); err != nil {
			t.Fatal(err)
		}
	}
	otherSubmitted := call(server.handleSubmitAdminTask, http.MethodPost, "", `{"team_id":"team","task":"另一个任务","client_request_id":"00000000-0000-0000-0000-0000000000f2"}`)
	var other workflowManualRunResponse
	if err := json.Unmarshal(otherSubmitted.Body.Bytes(), &other); err != nil {
		t.Fatal(err)
	}
	insert("earlier", other.RunID, now.Add(-time.Minute))
	insert("mine", run.RunID, now)
	if got := wait(run.RunID); !got.Waiting || got.Position != 2 || got.NodeName != "mac-studio" || got.Reason != "runtime_offline" || got.Engine != "claude" {
		t.Fatalf("queued behind another task on an offline node = %+v", got)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_logs(workspace_id,task_id,stream,seq,occurred_at,text)
		SELECT 'ws','mine','agent',1,created_at + interval '6 seconds','first' FROM weave_workflow_admission_requests WHERE workspace_id='ws' AND request->>'run_id'=$1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	metrics := call(server.handleGetAdminMetrics, http.MethodGet, "", "")
	var body struct {
		Median *float64 `json:"first_output_median_seconds"`
		Sample int      `json:"sample"`
	}
	if err := json.Unmarshal(metrics.Body.Bytes(), &body); err != nil || body.Sample != 1 || body.Median == nil || *body.Median < 5.9 || *body.Median > 6.1 {
		t.Fatalf("metrics = %s", metrics.Body.String())
	}
	single := call(server.handleListAdminTasks, http.MethodGet, run.RunID, "")
	var task adminTask
	if err := json.Unmarshal(single.Body.Bytes(), &task); err != nil || task.FirstOutputAt == nil {
		t.Fatalf("task first output = %s", single.Body.String())
	}
}
