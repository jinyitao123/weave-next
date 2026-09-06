package runtimes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestModelFallbackReconcilesAttemptsInsteadOfReplayingAfterRestartRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	timeout := 10 * time.Second
	liveCLI := os.Getenv("WEAVE_LIVE_FALLBACK_CLI_PATH")
	fallbackModel := "native-supported"
	if liveCLI != "" {
		timeout = 3 * time.Minute
		fallbackModel = os.Getenv("WEAVE_LIVE_FALLBACK_MODEL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
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

	record := &registry.AgentRecord{ID: "agent", Name: "worker", WorkspaceID: "ws", Version: 1, Engine: engine.Claude, RuntimeID: ids[0], RuntimePolicyMode: "strict_pin", Model: "missing-model", FallbackModels: []string{fallbackModel}, FallbackRetries: 2}
	stamp := execution.AgentExecutionStamp{AgentID: "agent", AgentVersion: 1, ExecutionScope: execution.ScopeLegacyOrchestrator}
	callCtx := execution.WithInvocationID(ctx, "run/worker/generation-0")
	workerDone := make(chan error, 1)
	go func() {
		previous := ""
		for count := 0; count < 2 && ctx.Err() == nil; {
			workerID := RuntimeWorkerID("ws", ids[0])
			task, err := queue.Claim(ctx, workerID, taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: ids[0]})
			if err != nil {
				workerDone <- err
				return
			}
			if task == nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			var request EngineExecRequest
			if err := json.Unmarshal(task.Payload, &request); err != nil {
				workerDone <- err
				return
			}
			result := engine.RunResult{Status: "failed", Err: "The model missing-model is not supported", RetrySafeBeforeExecution: true}
			if count == 0 {
				if request.Model != "missing-model" {
					workerDone <- fmt.Errorf("wrong primary model %s", request.Model)
					return
				}
				previous = task.ID
			} else {
				if request.Model != fallbackModel || task.ParentTaskID != previous || request.LogicalInvocationID != execution.EngineTaskID("ws", "run/worker/generation-0") {
					workerDone <- fmt.Errorf("lost fallback lineage model=%s parent=%s", request.Model, task.ParentTaskID)
					return
				}
				result = engine.RunResult{Status: "completed", Output: "fallback completed"}
				if liveCLI != "" {
					backend, err := engine.New(engine.Claude, liveCLI)
					if err != nil {
						workerDone <- err
						return
					}
					result, err = backend.Run(ctx, engine.RunSpec{WorkDir: t.TempDir(), Prompt: "Reply exactly fallback completed. Do not use tools or access files.", Model: request.Model, Env: map[string]string{"WEAVE_CLAUDE_AUTH_MODE": "oauth"}, Timeout: 2 * time.Minute})
					if err != nil {
						workerDone <- err
						return
					}
					t.Logf("injected primary rejection; real Claude fallback status=%s reported_models=%v output=%q", result.Status, result.ReportedModels, result.Output)
				}
			}
			raw, _ := json.Marshal(CLIEngineExecResult(result))
			if err := queue.CompleteClaimed(ctx, task.ID, workerID, raw, ""); err != nil {
				workerDone <- err
				return
			}
			count++
		}
		workerDone <- nil
	}()
	result, err := NewExecutor(queue, store, "", "").ExecRemote(callCtx, "ws", record, stamp, "one action", nil)
	if err != nil || result.Output != "fallback completed" || len(result.Attempts) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	resumed, err := NewExecutor(queue, store, "", "").ExecRemote(callCtx, "ws", record, stamp, "one action", nil)
	if err != nil || resumed.Output != result.Output || len(resumed.Attempts) != 2 {
		t.Fatalf("restart=%+v err=%v", resumed, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE kind='engine_exec'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("physical tasks=%d err=%v", count, err)
	}
	if safeModelRetryResult(engine.RunResult{RetrySafeBeforeExecution: true, Events: []engine.Event{{Kind: "tool_result", Tool: "Write", CallID: "one", Status: "ok"}}}) {
		t.Fatal("tool execution proof was ignored")
	}
	if !strings.HasPrefix(logicalEngineTaskID(callCtx, "ws"), "task-") {
		t.Fatal("missing stable identity")
	}
}
