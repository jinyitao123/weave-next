package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

func TestRuntimeCompletionPreservesWorkWhenUsageIsInvalidRealPG(t *testing.T) {
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	pool := testutil.PostgresPool(t)
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
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	productionPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer productionPool.Close()
	runtimeStore := runtimes.NewStore(productionPool)
	runtime, _, err := runtimeStore.Create(ctx, "ws", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	queue := taskqueue.New(productionPool, nil, time.Minute)
	payload, _ := json.Marshal(runtimes.EngineExecRequest{Engine: engine.Claude, EngineVersion: "fixture 1"})
	task := &taskqueue.Task{ID: "task", WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
		Kind: "engine_exec", RuntimeID: runtime.ID, Payload: payload}
	if err := queue.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, runtimes.RuntimeWorkerID("ws", runtime.ID), taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: runtime.ID})
	if err != nil || claimed == nil {
		t.Fatalf("claim: task=%v err=%v", claimed, err)
	}
	answer := "# 日冕物理校核\n" + strings.Repeat("正文应完整保留。", 1000)
	result := runtimes.EngineExecResult{Status: "completed", Output: answer,
		Artifacts:    []engine.Artifact{{Path: "physics/review.md", ContentType: "text/markdown", Content: answer}},
		UsageReceipt: &engine.UsageReceipt{Source: engine.UsageSourceCLIReported, Scope: engine.UsageScopeInvocation, EngineVersion: "fixture 1", HasTokens: true, InputTokens: 100, RawSummary: strings.Repeat("界", 5000)}}
	wire, _ := json.Marshal(runtimeReceiptForTask(claimed, result))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime/tasks/task/complete", bytes.NewReader(wire))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	setRuntimeTaskProof(request, claimed)
	c := echo.New().NewContext(request, recorder)
	c.Set(runtimeContextKey, runtime)
	c.SetPath("/v1/runtime/tasks/:id/complete")
	c.SetParamNames("id")
	c.SetParamValues(task.ID)
	server := &Server{Tasks: queue}
	if err := server.handleRuntimeTaskComplete(c); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("completion rejected: %d %s", recorder.Code, recorder.Body.String())
	}
	// Lost acknowledgements and process restarts resend the original packet.
	// The same normalized work is accepted; another body cannot overwrite it.
	for _, changed := range []bool{false, true} {
		replay := result
		if changed {
			replay.Output = "different answer"
		}
		data, _ := json.Marshal(runtimeReceiptForTask(claimed, replay))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/runtime/tasks/task/complete", bytes.NewReader(data))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		setRuntimeTaskProof(req, claimed)
		cc := echo.New().NewContext(req, rec)
		cc.Set(runtimeContextKey, runtime)
		cc.SetParamNames("id")
		cc.SetParamValues(task.ID)
		if err := server.handleRuntimeTaskComplete(cc); err != nil {
			t.Fatal(err)
		}
		want := http.StatusNoContent
		if changed {
			want = http.StatusConflict
		}
		if rec.Code != want {
			t.Fatalf("replayed changed=%v status=%d body=%s", changed, rec.Code, rec.Body.String())
		}
	}
	saved, err := queue.Get(ctx, "ws", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got runtimes.EngineExecResult
	if err := json.Unmarshal(saved.Result, &got); err != nil {
		t.Fatal(err)
	}
	if saved.Status != taskqueue.StatusCompleted || got.Status != "completed" || got.Output != answer || len(got.Artifacts) != 1 || got.Artifacts[0].Content != answer {
		t.Fatal("optional statistics damaged completed work")
	}
	if got.UsageReceipt != nil || len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "usage_invalid" {
		t.Fatal("invalid accounting was accepted or discarded without notice")
	}
}
