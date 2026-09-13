package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func TestRuntimeStopAcknowledgementAuthenticatesOwnerAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');`); err != nil {
		t.Fatal(err)
	}
	rs := runtimes.NewStore(pool)
	owner, _, err := rs.Create(ctx, "ws", "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := rs.Create(ctx, "ws", "other")
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	task := &taskqueue.Task{ID: "remote", WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
		Kind: "engine_exec", Source: "dispatch", RuntimeID: owner.ID, Payload: json.RawMessage(`{}`)}
	if err := tasks.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='cancel_requested',worker_id=$2,lease_expires_at=NOW()+INTERVAL '1 minute' WHERE id=$1`, task.ID, runtimes.RuntimeWorkerID("ws", owner.ID)); err != nil {
		t.Fatal(err)
	}
	server := &Server{Tasks: tasks}
	ack := func(runtime *runtimes.Runtime) int {
		t.Helper()
		rec := httptest.NewRecorder()
		e := echo.New()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/runtime/tasks/remote/stopped", nil), rec)
		c.SetParamNames("id")
		c.SetParamValues(task.ID)
		c.Set(runtimeContextKey, runtime)
		if err := server.handleRuntimeTaskStopped(c); err != nil {
			e.HTTPErrorHandler(err, c)
		}
		return rec.Code
	}
	if got := ack(other); got != http.StatusNotFound {
		t.Fatalf("other runtime acknowledged stop: %d", got)
	}
	for range 2 {
		if got := ack(owner); got != http.StatusNoContent {
			t.Fatalf("owner acknowledgement/replay: %d", got)
		}
	}
	stored, err := tasks.Get(ctx, "ws", task.ID)
	if err != nil || stored.Status != taskqueue.StatusCancelled || stored.WorkerID != "" {
		t.Fatalf("stop not recorded: %+v %v", stored, err)
	}
	// A completed result remains completed when the response was lost and the
	// daemon subsequently reports its already-exited process.
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='completed',result='{"output":"saved"}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if got := ack(owner); got != http.StatusNoContent {
		t.Fatal(got)
	}
	stored, err = tasks.Get(ctx, "ws", task.ID)
	if err != nil || stored.Status != taskqueue.StatusCompleted || string(stored.Result) != `{"output": "saved"}` {
		t.Fatalf("ack overwrote saved result: %+v %v", stored, err)
	}
}
