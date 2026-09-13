package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/labstack/echo/v4"
)

type mcpClaimClock struct{ at time.Time }

func (c mcpClaimClock) Now() time.Time { return c.at }

func TestFrozenCLIMCPClaimAuthorityAndEffectsRealPG(t *testing.T) {
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", hex.EncodeToString(key))
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
 INSERT INTO weave_teams(id,workspace_id,name) VALUES('team','ws','team');`); err != nil {
		t.Fatal(err)
	}
	_, err := snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
		RunID: "snapshot", WorkspaceID: "ws", TeamID: "team", SourceRef: "fixture", SnapshotSchemaVersion: 2, Mode: "free_collab", LeadAvatarID: "agent", LeadAvatarVersion: 1,
		WorkerVersions: json.RawMessage(`{}`), TeamWorkerSnapshot: json.RawMessage(`[]`), InlineDependencies: json.RawMessage(`{}`), RuntimeAssignment: json.RawMessage(`{}`),
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-05T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`), TriggerSourceV2: json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	seedPool := pool
	config := pool.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	productionPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer productionPool.Close()
	pool = productionPool
	var effects atomic.Int64
	var drift atomic.Bool
	var cancelDuringList atomic.Bool
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-upstream-private" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "claim-fixture", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			selected := schema
			if drift.Load() {
				selected = json.RawMessage(`{"type":"object","required":["changed"]}`)
			}
			result = map[string]any{"tools": []any{map[string]any{"name": "calculate", "description": "Calculate", "inputSchema": selected, "annotations": map[string]any{"readOnlyHint": true}}}}
			if cancelDuringList.Load() {
				if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='cancel_requested' WHERE id='task'`); err != nil {
					t.Error(err)
				}
			}
		case "tools/call":
			effects.Add(1)
			if !strings.Contains(string(request.Params), "9007199254740993") {
				t.Error("argument precision changed before upstream")
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "accepted"}}, "isError": false}
		default:
			http.Error(w, "unknown", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer upstream.Close()
	mcp := mcpregistry.New(pool, key)
	registered, err := mcp.Create(ctx, "ws", "test", mcpregistry.UpsertServerRequest{Slug: "claim-fixture", DisplayName: "Claim fixture", Transport: mcpregistry.TransportStreamableHTTP, URL: upstream.URL, Enabled: true, Headers: map[string]string{"Authorization": "Bearer fixture-upstream-private"}})
	if err != nil {
		t.Fatal(err)
	}
	readOnly := true
	if _, err := mcp.RecordProbeSuccess(ctx, "ws", registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", Description: "Calculate", InputSchema: schema, ReadOnlyHint: &readOnly}}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := mcpregistry.ResolveCurrentMCPToolsTx(ctx, tx, "ws", registered.ID, mcpregistry.MCPAgentPolicy{})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	runtimeStore := runtimes.NewStore(pool)
	owner, runtimeToken, err := runtimeStore.Create(ctx, "ws", "owner")
	if err != nil {
		t.Fatal(err)
	}
	clock := mcpClaimClock{time.Now().UTC().Truncate(time.Microsecond)}
	tasks := taskqueue.New(pool, clock, time.Minute)
	payload := runtimes.EngineExecRequest{Subject: execution.Subject{WorkspaceID: "ws", UserID: "user"}, Engine: "codex", BoundMCP: true, Record: &registry.AgentRecord{WorkspaceID: "ws", ID: "agent", Name: "worker", Version: 1, Engine: "codex", MCPServers: []registry.MCPServerConfig{{ServerID: registered.ID}}}, FrozenMCP: &execspec.FrozenMCPInvocation{WorkspaceID: "ws", AgentID: "agent", AgentVersion: 1, RunSnapshotID: "snapshot", FactoryKey: compiler.StandardFrozenCLIToolsKey(), Bindings: []frozen.FrozenMCPBinding{binding}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{ID: "task", WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamWorkerLeaf, RunSnapshotID: "snapshot", Kind: "engine_exec", Source: "dispatch", RuntimeID: owner.ID, Payload: encoded}
	if err := tasks.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	claim := func() *taskqueue.Task {
		t.Helper()
		claimed, err := tasks.Claim(ctx, runtimes.RuntimeWorkerID("ws", owner.ID), taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: owner.ID, IdentityKind: taskqueue.IdentityAgent})
		if err != nil || claimed == nil {
			t.Fatalf("claim failed: %v", err)
		}
		return claimed
	}
	first := claim()
	server := &Server{Pool: pool, Tasks: tasks, Runtimes: runtimeStore, MCPRegistry: mcp}
	mint := func(task *taskqueue.Task) string {
		t.Helper()
		raw, err := server.redactRuntimeClaim(task)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), upstream.URL) || strings.Contains(string(raw), "fixture-upstream-private") || strings.Contains(string(raw), "access_ref") || strings.Contains(string(raw), "frozen_mcp") {
			t.Fatal("claim leaked frozen connection authority")
		}
		var p runtimes.EngineExecRequest
		if json.Unmarshal(raw, &p) != nil || len(p.TaskMCP) != 1 || len(p.Record.MCPServers) != 0 {
			t.Fatal("claim missing task endpoints")
		}
		return p.TaskMCP[0].Token
	}
	token := mint(first)
	e := echo.New()
	e.Any("/v1/runtime/tasks/:id/mcp/:idx", server.handleRuntimeTaskMCP, server.taskMCPAuthMiddleware())
	call := func(token, path, args string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"calculate","arguments":%s}}`, args)
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	path := "/v1/runtime/tasks/task/mcp/0"
	validArgs := `{"n":9007199254740993}`
	wantEffects := int64(1)
	if status, body := call(token, path, validArgs); status != 200 || !strings.Contains(body, "accepted") || effects.Load() != wantEffects {
		t.Fatalf("valid claim not executed: %d %s effects=%d", status, body, effects.Load())
	}
	if err := tasks.Heartbeat(ctx, first.ID, first.WorkerID); err != nil {
		t.Fatal(err)
	}
	renewed, err := tasks.Get(ctx, "ws", first.ID)
	if err != nil || renewed.ClaimEpoch != first.ClaimEpoch || !renewed.StartedAt.Equal(*first.StartedAt) {
		t.Fatal("renew changed claim identity")
	}
	wantEffects++
	if status, _ := call(token, path, validArgs); status != 200 || effects.Load() != wantEffects {
		t.Fatal("renew invalidated current token")
	}
	claims, err := secret.VerifyTaskMCPToken(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*secret.TaskMCPClaims){
		func(c *secret.TaskMCPClaims) { c.WorkspaceID = "other" },
		func(c *secret.TaskMCPClaims) { c.RuntimeID = "other-runtime" },
		func(c *secret.TaskMCPClaims) { c.BindingDigest = "changed" },
		func(c *secret.TaskMCPClaims) { c.ClaimEpoch = 0 },
	} {
		changed := claims
		mutate(&changed)
		bad, err := secret.SignTaskMCPToken(changed)
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := call(bad, path, validArgs); status < 400 || effects.Load() != wantEffects {
			t.Fatal("altered signed claim executed")
		}
	}
	for _, args := range []string{`{}`, `{"n":"PRIVATE_VALUE"}`, `{"n":1,"n":2}`, `{"n":1,"extra":true}`} {
		status, body := call(token, path, args)
		if status != 200 || !strings.Contains(body, "mcp_arguments_") || strings.Contains(body, "PRIVATE_VALUE") || effects.Load() != wantEffects {
			t.Fatalf("invalid arguments escaped: %d %s effects=%d", status, body, effects.Load())
		}
	}
	for _, invalid := range []struct{ token, path string }{{runtimeToken, path}, {secret.BoundaryToken("ws", "worker", 0), path}, {token, "/v1/runtime/tasks/other/mcp/0"}, {token, "/v1/runtime/tasks/task/mcp/1"}} {
		if status, _ := call(invalid.token, invalid.path, validArgs); status < 400 || effects.Load() != wantEffects {
			t.Fatal("cross-scope credential executed")
		}
	}
	drift.Store(true)
	if status, _ := call(token, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("schema drift executed")
	}
	drift.Store(false)
	for _, state := range []string{"queued", "cancel_requested", "cancelled", "completed", "failed"} {
		if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status=$1 WHERE id='task'`, state); err != nil {
			t.Fatal(err)
		}
		if status, _ := call(token, path, validArgs); status < 400 || effects.Load() != wantEffects {
			t.Fatalf("state %s executed", state)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='running',lease_expires_at=NOW()-INTERVAL '1 second' WHERE id='task'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(token, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("expired lease executed")
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='queued',worker_id=NULL,started_at=NULL,lease_expires_at=NULL WHERE id='task'`); err != nil {
		t.Fatal(err)
	}
	second := claim()
	if second.ClaimEpoch != first.ClaimEpoch+1 || !second.StartedAt.Equal(*first.StartedAt) {
		t.Fatal("claim epoch did not distinguish identical timestamps")
	}
	if status, _ := call(token, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("old claim token survived reclaim")
	}
	current := mint(second)
	wantEffects++
	if status, _ := call(current, path, validArgs); status != 200 || effects.Load() != wantEffects {
		t.Fatal("new claim cannot execute")
	}
	cancelDuringList.Store(true)
	if status, _ := call(current, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("cancel during catalog check passed final effect gate")
	}
	cancelDuringList.Store(false)
	if _, err := seedPool.Exec(ctx, `UPDATE weave_task_queue SET status='running' WHERE id='task'; UPDATE weave_mcp_servers SET enabled=false WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(current, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("revoked MCP access executed")
	}
	closedPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	closedPool.Close()
	server.Runtimes = runtimes.NewStore(closedPool)
	if status, _ := call(current, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("unavailable database allowed MCP execution")
	}

	server.Runtimes = runtimeStore
	if _, err := seedPool.Exec(ctx, `UPDATE weave_mcp_servers SET enabled=true WHERE workspace_id='ws'; UPDATE weave_runtimes SET enabled=false WHERE workspace_id='ws'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(current, path, validArgs); status < 400 || effects.Load() != wantEffects {
		t.Fatal("revoked runtime executed")
	}

}
