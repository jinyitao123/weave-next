package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/capabilityruntime"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

type capabilityToolUnusedRemote struct{ calls atomic.Int64 }

func (r *capabilityToolUnusedRemote) ExecRemote(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, []execenv.Attachment) (engine.RunResult, error) {
	r.calls.Add(1)
	return engine.RunResult{}, errors.New("tool step was delegated to the model runtime")
}

func TestCapabilityPublicationToDirectToolExecutionRealPG(t *testing.T) {
	ctx := t.Context()
	seed := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, seed); err != nil {
		t.Fatal(err)
	}
	cfg := seed.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws-direct','ws-direct','Direct tool workspace')`); err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int64
	inputSchema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer direct-fixture" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.NewDecoder(request.Body).Decode(&rpc) != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "direct", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": inputSchema, "annotations": map[string]bool{"readOnlyHint": true}}}}
		case "tools/call":
			if rpc.Params.Name != "calculate" || string(rpc.Params.Arguments) != `{"count":3}` {
				t.Errorf("unexpected tool call %q %s", rpc.Params.Name, rpc.Params.Arguments)
			}
			effects.Add(1)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{"total":42}`}}}
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer upstream.Close()

	key := []byte(strings.Repeat("k", 32))
	mcp := mcpregistry.New(pool, key)
	registered, err := mcp.Create(ctx, "ws-direct", "user", mcpregistry.UpsertServerRequest{Slug: "direct", DisplayName: "Direct", Transport: mcpregistry.TransportStreamableHTTP,
		URL: upstream.URL, Headers: map[string]string{"Authorization": "Bearer direct-fixture"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	readOnly := true
	if _, err := mcp.RecordProbeSuccess(ctx, "ws-direct", registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "calculate", InputSchema: inputSchema, ReadOnlyHint: &readOnly}}); err != nil {
		t.Fatal(err)
	}
	store := appcapabilities.NewPGStore(pool)
	service := appcapabilities.NewService(store, store)
	definition := capability.Definition{SchemaVersion: 1, CapabilityID: "cap-direct", Name: "Direct calculation",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Roles: []capability.Role{{ID: "runner", Name: "Runner"}},
		Steps: []capability.Step{{ID: "calculate", Name: "Calculate", RoleID: "runner", Kind: capability.StepTool, ToolID: "calculate",
			InputBindings: map[string]capability.ValueRef{"count": {Source: "input", Path: "/count"}}, OutputSchema: json.RawMessage(`{"type":"object","required":["total"]}`)}},
		Runtime: capability.RuntimeRequirement{Engine: "codex"}, Resources: capability.ResourceRequirement{Tools: []capability.ToolReference{{MCPServerID: registered.ID, ToolName: "calculate"}}}}
	if err := service.SaveDraft(ctx, appcapabilities.DraftRequest{WorkspaceID: "ws-direct", Definition: definition}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, "ws-direct", "cap-direct", 1); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := service.Invoke(ctx, appcapabilities.InvokeRequest{WorkspaceID: "ws-direct", ApplicationID: "app", InvocationID: "inv-direct", RequestID: "req-direct",
		CapabilityID: "cap-direct", Revision: 1, Input: json.RawMessage(`{"count":3}`)})
	if err != nil {
		t.Fatal(err)
	}
	unusedRemote := &capabilityToolUnusedRemote{}
	runner := &capabilityruntime.Runner{Remote: unusedRemote, ListRuntimes: func(context.Context, string) ([]runtimes.Runtime, error) {
		return []runtimes.Runtime{{ID: "runtime", Enabled: true, Online: true, Engines: []string{"codex"}, TotalSlots: 1}}, nil
	}}
	server := &Server{Pool: pool, MCPRegistry: mcp}
	processed, err := appcapabilities.RunOne(ctx, store, appcapabilities.RuntimeTaskExecutor{Runner: runner, Store: store, DirectTools: server.capabilityDirectTools(store)})
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	completed, err := service.GetInvocation(ctx, "ws-direct", "app", invocation.InvocationID)
	var delivered struct {
		Calculate struct {
			Total int `json:"total"`
		} `json:"calculate"`
	}
	decodeErr := json.Unmarshal(completed.Result, &delivered)
	if err != nil || decodeErr != nil || completed.Status != "completed" || delivered.Calculate.Total != 42 || effects.Load() != 1 || unusedRemote.calls.Load() != 0 {
		t.Fatalf("completed=%+v effects=%d remote=%d err=%v", completed, effects.Load(), unusedRemote.calls.Load(), err)
	}
}
