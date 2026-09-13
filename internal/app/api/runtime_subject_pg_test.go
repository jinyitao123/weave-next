package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestRuntimeSubjectAndLateReceiptRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('shared','shared','shared');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','shared','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','shared',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	rs := runtimes.NewStore(pool)
	host, _, err := rs.Create(ctx, "shared", "host")
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	server := &Server{Tasks: tasks}
	alice := execution.Subject{WorkspaceID: "shared", UserID: "alice"}
	bob := alice
	bob.UserID = "bob"
	for _, subject := range []execution.Subject{alice, bob} {
		payload, _ := json.Marshal(runtimes.EngineExecRequest{Subject: subject, Engine: engine.OpenCode})
		task := &taskqueue.Task{ID: subject.UserID, WorkspaceID: "shared", Agent: "worker", AgentID: "agent", AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Kind: "engine_exec", RuntimeID: host.ID, Payload: payload}
		if err := tasks.Enqueue(execution.WithSubject(ctx, subject), task); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='running',worker_id=$2,claim_epoch=2,unreported_attempts=2,lease_expires_at=now()+interval '1 minute' WHERE id=$1`, task.ID, runtimes.RuntimeWorkerID("shared", host.ID)); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(id string, proof execution.Subject, epoch int64, result runtimes.EngineExecResult) int {
		body, _ := json.Marshal(runtimeReceiptForTask(&taskqueue.Task{ID: id, ClaimEpoch: result.ClaimEpoch, Subject: result.Subject}, result))
		e := echo.New()
		rec := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/v1/runtime/tasks/"+id+"/complete", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
		request.Header.Set("X-Weave-Task-Subject", proof.Digest())
		request.Header.Set("X-Weave-Task-Epoch", strconv.FormatInt(epoch, 10))
		c := e.NewContext(request, rec)
		c.SetParamNames("id")
		c.SetParamValues(id)
		c.Set(runtimeContextKey, host)
		if err := server.handleRuntimeTaskComplete(c); err != nil {
			e.HTTPErrorHandler(err, c)
		}
		return rec.Code
	}
	result := runtimes.EngineExecResult{Subject: alice, ClaimEpoch: 2, Status: "completed", Output: "Alice private result"}
	if got := complete("alice", bob, 2, result); got != http.StatusConflict {
		t.Fatalf("wrong actor proof=%d", got)
	}
	if got := complete("alice", alice, 1, result); got != http.StatusConflict {
		t.Fatalf("old attempt proof=%d", got)
	}
	forged := result
	forged.Subject = bob
	if got := complete("alice", alice, 2, forged); got != http.StatusBadRequest {
		t.Fatalf("forged receipt=%d", got)
	}
	forged = result
	forged.ClaimEpoch = 1
	if got := complete("alice", alice, 2, forged); got != http.StatusBadRequest {
		t.Fatalf("late receipt=%d", got)
	}
	for range 2 {
		if got := complete("alice", alice, 2, result); got != http.StatusNoContent {
			t.Fatalf("valid or replay receipt=%d", got)
		}
	}
	stored, err := tasks.Get(execution.WithSubject(ctx, bob), "shared", "bob")
	if err != nil || stored.Status != taskqueue.StatusRunning || len(stored.Result) > 0 {
		t.Fatalf("Bob task changed: %+v %v", stored, err)
	}
	if _, err := tasks.Get(execution.WithSubject(ctx, bob), "shared", "alice"); err == nil {
		t.Fatal("Bob read Alice result")
	}
}
