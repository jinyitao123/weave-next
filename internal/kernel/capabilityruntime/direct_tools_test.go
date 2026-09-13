package capabilityruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const directToolSchema = `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`

type directToolAudit struct{ calls atomic.Int64 }

func (a *directToolAudit) Record(context.Context, string, string, string, string, string) error {
	a.calls.Add(1)
	return nil
}
func (a *directToolAudit) RecordServer(context.Context, string, string, string, string, string, string) error {
	a.calls.Add(1)
	return nil
}

type directToolFixture struct {
	config   DirectToolConfig
	audit    *directToolAudit
	calls    atomic.Int64
	mu       sync.Mutex
	received string
	tool     string
	schema   string
	output   string
}

func newDirectToolFixture(t *testing.T) *directToolFixture {
	t.Helper()
	f := &directToolFixture{schema: directToolSchema, output: `{"total":42}`, audit: &directToolAudit{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			f.mu.Lock()
			schema := f.schema
			f.mu.Unlock()
			result = map[string]any{"tools": []map[string]any{{"name": "calculate", "inputSchema": json.RawMessage(schema), "annotations": map[string]bool{"readOnlyHint": true}}}}
		case "tools/call":
			f.calls.Add(1)
			f.mu.Lock()
			f.received, f.tool = string(request.Params.Arguments), request.Params.Name
			output := f.output
			f.mu.Unlock()
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": output}}}
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	binding := frozen.FrozenMCPBinding{
		SchemaVersion: 1, WorkspaceID: "ws", ServerID: "mcp", ServerRevision: 1,
		Transport: "http", URL: server.URL, Filter: []string{"calculate"},
		Tools:     []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(directToolSchema), ReadOnly: true}},
		AccessRef: frozen.CredentialReference{
			SchemaVersion: 1, WorkspaceID: "ws", ResourceID: "mcp",
			Kind: frozen.CredentialMCPServerAccess, Slot: "access",
			Scope: frozen.CredentialScopeWorkspaceService, ServiceID: "mcp:mcp",
		},
	}
	f.config = DirectToolConfig{WorkspaceID: "ws", Agent: &registry.AgentRecord{WorkspaceID: "ws", ID: "agent", Version: 1, Name: "worker"},
		ToolIDs: []string{"calculate"}, Bindings: []frozen.FrozenMCPBinding{binding},
		AccessFactory:  mcphost.NewMCPAccessFactory(nil, f.audit),
		ResolveAccess:  func(context.Context, frozen.CredentialReference) (map[string]string, error) { return nil, nil },
		ValidateAccess: func(context.Context, frozen.CredentialReference) error { return nil },
		ValidateClaim:  func(context.Context) error { return nil },
	}
	return f
}

func directStep() capability.PlanStep {
	return capability.PlanStep{ID: "compute", Kind: capability.StepTool, ToolID: "calculate", OutputSchema: json.RawMessage(`{"type":"object","required":["total"]}`)}
}

func TestDirectToolExecutesExactCallThroughFrozenGateway(t *testing.T) {
	f := newDirectToolFixture(t)
	executor, err := NewDirectToolExecutor(f.config)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's binding after admission cannot change the execution.
	f.config.Bindings[0].Tools[0].Name = "unapproved"
	f.config.Agent.WorkspaceID = "other"
	input := json.RawMessage(`{"count":9007199254740993}`)
	output, err := executor.ExecuteTool(execution.WithInvocationID(t.Context(), "invocation/step/1"), directStep(), input)
	if err != nil || string(output) != `{"total":42}` || f.calls.Load() != 1 || f.audit.calls.Load() != 1 {
		t.Fatalf("output=%s err=%v calls=%d audits=%d", output, err, f.calls.Load(), f.audit.calls.Load())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tool != "calculate" || f.received != string(input) {
		t.Fatalf("tool or arguments changed: %q %s", f.tool, f.received)
	}
}

func TestDirectToolRejectsUnboundCrossWorkspaceAndAmbiguousTools(t *testing.T) {
	for _, name := range []string{"unbound", "workspace", "ambiguous", "missing_guard"} {
		t.Run(name, func(t *testing.T) {
			f := newDirectToolFixture(t)
			switch name {
			case "unbound":
				f.config.ToolIDs = []string{"unknown"}
			case "workspace":
				f.config.Bindings[0].WorkspaceID = "other"
			case "ambiguous":
				f.config.Bindings = append(f.config.Bindings, f.config.Bindings[0])
			case "missing_guard":
				f.config.ValidateClaim = nil
			}
			if _, err := NewDirectToolExecutor(f.config); err == nil || f.calls.Load() != 0 {
				t.Fatalf("invalid tool configuration accepted: %v", err)
			}
		})
	}
}

func TestDirectToolDoesNotDispatchWithoutCurrentAuthorityOrValidArguments(t *testing.T) {
	for _, name := range []string{"undeclared", "missing_identity", "invalid_args", "schema_drift", "revoked_after_resolution", "cancelled", "declared_write"} {
		t.Run(name, func(t *testing.T) {
			f := newDirectToolFixture(t)
			step, input := directStep(), json.RawMessage(`{"count":1}`)
			ctx := execution.WithInvocationID(t.Context(), "invocation/step/1")
			switch name {
			case "undeclared":
				step.ToolID = "unknown"
			case "missing_identity":
				ctx = t.Context()
			case "invalid_args":
				input = json.RawMessage(`{"count":"1"}`)
			case "schema_drift":
				f.schema = `{"type":"object","required":["unexpected"]}`
			case "revoked_after_resolution":
				var revoked atomic.Bool
				f.config.ResolveAccess = func(context.Context, frozen.CredentialReference) (map[string]string, error) {
					revoked.Store(true)
					return nil, nil
				}
				f.config.ValidateAccess = func(context.Context, frozen.CredentialReference) error {
					if revoked.Load() {
						return errors.New("revoked")
					}
					return nil
				}
			case "cancelled":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			case "declared_write":
				f.config.Bindings[0].WriteTools = []string{"calculate"}
			}
			executor, err := NewDirectToolExecutor(f.config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.ExecuteTool(ctx, step, input); err == nil || f.calls.Load() != 0 {
				t.Fatalf("invalid call reached upstream: err=%v calls=%d", err, f.calls.Load())
			}
		})
	}
}

func TestDirectToolDoesNotRetryOrRepairInvalidOutput(t *testing.T) {
	for _, output := range []string{"not-json", `{"wrong":42}`} {
		t.Run(strings.ReplaceAll(output, "/", "_"), func(t *testing.T) {
			f := newDirectToolFixture(t)
			f.output = output
			executor, err := NewDirectToolExecutor(f.config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.ExecuteTool(execution.WithInvocationID(t.Context(), "invocation/step/1"), directStep(), json.RawMessage(`{"count":1}`)); err == nil || f.calls.Load() != 1 {
				t.Fatalf("invalid output repaired or retried: err=%v calls=%d", err, f.calls.Load())
			}
		})
	}
}
