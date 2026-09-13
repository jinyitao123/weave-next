package runtimes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestRuntimePoolDoesNotReplayWorkAfterDisconnectedInvocationRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
`); err != nil {
		t.Fatal(err)
	}
	// Use the production parameter protocol for the runtime/task stores;
	// the fixture pool uses simple protocol for its multi-statement seeds.
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	runtimePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	store := NewStore(runtimePool)
	var ids []string
	for _, name := range []string{"First", "Backup"} {
		runtime, _, err := store.Create(ctx, "ws", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture", ProtocolVersion: "1", AuthMode: AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_runtimes SET pool_id='pool',functional_revision=functional_revision+1 WHERE id=$1`, runtime.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, runtime.ID)
	}
	queue := taskqueue.New(runtimePool, nil, time.Minute)
	workerDone := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			for _, id := range ids {
				workerID := RuntimeWorkerID("ws", id)
				task, err := queue.Claim(ctx, workerID, taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: id})
				if err != nil {
					workerDone <- err
					return
				}
				if task == nil {
					continue
				}
				// This invocation already performed a tool action. A subsequent
				// connection loss must preserve this exact failed attempt.
				result, _ := json.Marshal(CLIEngineExecResult(engine.RunResult{Status: "failed", Err: "stream disconnected", Events: []engine.Event{{Kind: "tool_result", Tool: "write", CallID: "one", Status: "ok", Output: "saved"}}}))
				workerDone <- queue.CompleteClaimed(ctx, task.ID, workerID, result, "")
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Millisecond):
			}
		}
		workerDone <- ctx.Err()
	}()
	record := &registry.AgentRecord{ID: "agent", Name: "worker", WorkspaceID: "ws", Version: 1, Engine: engine.Claude,
		RuntimeID: ids[0], RuntimePolicyMode: "engine_pool", RuntimePoolID: "pool"}
	result, err := NewExecutor(queue, store, "", "").ExecRemote(ctx, "ws", record,
		execution.AgentExecutionStamp{AgentID: "agent", AgentVersion: 1, ExecutionScope: execution.ScopeLegacyOrchestrator}, "perform one action", nil)
	if workerErr := <-workerDone; workerErr != nil {
		t.Fatal(workerErr)
	}
	if err == nil || !strings.Contains(err.Error(), "stream disconnected") || len(result.Attempts) != 1 || len(result.Events) != 1 {
		t.Fatalf("invocation was lost or repeated: result=%+v err=%v", result, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("physical tasks=%d want=1 error=%v", count, err)
	}
}

func TestSchedulerRestartReattachesCompletedPhysicalInvocationRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
`); err != nil {
		t.Fatal(err)
	}
	// Use the production parameter protocol for the runtime/task stores;
	// the fixture pool uses simple protocol for its multi-statement seeds.
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	runtimePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	store := NewStore(runtimePool)
	var ids []string
	for _, name := range []string{"First", "Backup"} {
		runtime, _, err := store.Create(ctx, "ws", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture", ProtocolVersion: "1", AuthMode: AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_runtimes SET pool_id='pool',functional_revision=functional_revision+1 WHERE id=$1`, runtime.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, runtime.ID)
	}
	queue := taskqueue.New(runtimePool, nil, time.Minute)

	record := &registry.AgentRecord{ID: "agent", Name: "worker", WorkspaceID: "ws", Version: 1, Engine: engine.Claude, RuntimeID: ids[0]}
	stamp := execution.AgentExecutionStamp{AgentID: "agent", AgentVersion: 1, ExecutionScope: execution.ScopeLegacyOrchestrator}
	invocation := execution.WithInvocationID(ctx, "run/node/generation-0")
	firstCtx, stopScheduler := context.WithCancel(invocation)
	firstDone := make(chan error, 1)
	go func() {
		_, err := NewExecutor(queue, store, "", "").ExecRemote(firstCtx, "ws", record, stamp, "one action", nil)
		firstDone <- err
	}()
	var physical *taskqueue.Task
	for physical == nil && ctx.Err() == nil {
		physical, err = queue.Claim(ctx, RuntimeWorkerID("ws", ids[0]), taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: ids[0]})
		if err != nil {
			t.Fatal(err)
		}
		if physical == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if physical == nil {
		t.Fatal("physical invocation never claimed")
	}
	stopScheduler()
	if err := <-firstDone; err == nil {
		t.Fatal("scheduler did not detach")
	}
	task, err := queue.Get(ctx, "ws", physical.ID)
	if err != nil || task.Status != taskqueue.StatusRunning {
		t.Fatalf("shutdown cancelled physical work: %+v %v", task, err)
	}
	result, _ := json.Marshal(CLIEngineExecResult(engine.RunResult{Status: "completed", Output: "one durable result"}))
	if err := queue.CompleteClaimed(ctx, physical.ID, RuntimeWorkerID("ws", ids[0]), result, ""); err != nil {
		t.Fatal(err)
	}
	// A completed receipt is usable even when the original runtime is offline.
	if _, err := pool.Exec(ctx, `UPDATE weave_runtimes SET last_heartbeat_at=NULL WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	got, err := NewExecutor(queue, store, "", "").ExecRemote(invocation, "ws", record, stamp, "one action", nil)
	if err != nil || got.Output != "one durable result" || len(got.Attempts) != 1 || got.Attempts[0].AttemptID != physical.ID {
		t.Fatalf("resume=%+v err=%v", got, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("physical tasks=%d err=%v", count, err)
	}
	if logicalEngineTaskID(execution.WithInvocationID(ctx, "run/node/generation-1"), "ws") == physical.ID {
		t.Fatal("explicit retry reused previous generation")
	}
}
