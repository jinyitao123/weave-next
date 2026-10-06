package businessaction_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

const confirmationHTTPToolSchema = `{"type":"object","properties":{"actionName":{"type":"string"},"objectName":{"type":"string"},"recordId":{"type":"string"},"params":{"type":"object"},"confirm":{"type":"boolean"}},"required":["actionName"],"additionalProperties":false}`

type confirmationHTTPFixture struct {
	t           *testing.T
	server      *httptest.Server
	pool        *pgxpool.Pool
	mu          sync.Mutex
	grant       businessaction.TaskDelegationGrant
	schema      string
	required    bool
	files       bool
	hidden      bool
	mode        string
	reads       atomic.Int64
	effects     atomic.Int64
	lists       atomic.Int64
	wires       []json.RawMessage
	onDiscovery func()
}

func (f *confirmationHTTPFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
		f.t.Error("request did not use the restricted task credential")
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == businessaction.TaskDelegationPath+"/current" {
		f.reads.Add(1)
		f.mu.Lock()
		grant := f.grant
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(grant)
		return
	}
	if r.URL.Path == businessaction.TaskDelegationPath+"/objects/sales_quote" {
		params := `{"name":"line_id","type":"text","required":true},{"name":"idempotency_key","type":"text","required":true}`
		if f.files {
			params += `,{"name":"material_file_id","type":"file","required":true}`
		}
		_, _ = w.Write([]byte(`{"type":"object","name":"sales_quote","item":{"name":"sales_quote","actions":[{"name":"AdjustPrice","params":[` + params + `]}]}}`))
		return
	}
	if r.URL.Path != businessaction.TaskDelegationPath+"/mcp" {
		f.t.Error("business request left the task-only MCP endpoint")
		w.WriteHeader(404)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		w.WriteHeader(400)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "isolated-native-forge", "version": "17.5"}}
	case "notifications/initialized":
		w.WriteHeader(202)
		return
	case "tools/list":
		if f.lists.Add(1) == 2 && f.onDiscovery != nil {
			f.onDiscovery()
		}
		f.mu.Lock()
		schema := f.schema
		f.mu.Unlock()
		result = map[string]any{"tools": []map[string]any{{"name": "list_actions", "inputSchema": json.RawMessage(`{"type":"object"}`)}, {"name": "run_action", "inputSchema": json.RawMessage(schema)}}}
	case "tools/call":
		content, isError := "", false
		if req.Params.Name == "list_actions" {
			if f.hidden {
				content = `{"actions":[]}`
			} else {
				params := `{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}`
				if f.files {
					params += `,{"name":"material_file_id","type":"string","required":true}`
				}
				content = fmt.Sprintf(`{"actions":[{"name":"AdjustPrice","objectName":"sales_quote","requiresRecord":true,"requiresConfirmation":%t,"params":[%s]}]}`, f.required, params)
			}
		} else if req.Params.Name == "run_action" {
			f.mu.Lock()
			f.wires = append(f.wires, append(json.RawMessage(nil), req.Params.Arguments...))
			f.mu.Unlock()
			var body map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &body)
			if f.required && body["confirm"] != true {
				f.t.Error("required native confirmation was not system-projected")
			}
			if !f.required {
				if _, exists := body["confirm"]; exists {
					f.t.Error("confirmation changed the old native request")
				}
			}
			if body["recordId"] != "record-a" || body["actionName"] != "AdjustPrice" || body["objectName"] != "sales_quote" {
				f.t.Error("confirmation broadened the frozen target")
			}
			params, _ := body["params"].(map[string]any)
			if f.files && params["material_file_id"] != "file-a" {
				f.t.Error("confirmation broadened the frozen material selection")
			}
			if _, exists := params["confirm"]; exists {
				f.t.Error("confirmation entered business params")
			}
			if f.mode == "http428" {
				w.WriteHeader(428)
				_, _ = w.Write([]byte(`{"error":{"code":"ACTION_CONFIRMATION_REQUIRED"}}`))
				return
			}
			if f.mode == "rejected" {
				content, isError = `{"error":{"code":"ACTION_CONFIRMATION_REQUIRED","message":"Native confirmation refused","status":428}}`, true
			} else {
				f.effects.Add(1)
				if f.mode == "unknown" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				content = `{"ok":true,"data":{"receipt":"original-native-receipt"}}`
			}
		} else {
			f.t.Error("unexpected native tool")
			w.WriteHeader(400)
			return
		}
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": content}}, "isError": isError}
	default:
		f.t.Error("unexpected MCP method")
		w.WriteHeader(400)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func confirmationTaskContext(t *testing.T, pool *pgxpool.Pool) context.Context {
	t.Helper()
	subject := execution.Subject{WorkspaceID: "workspace", UserID: "employee"}
	ctx, err := execution.WithCurrentTask(execution.WithSubject(t.Context(), subject), execution.CurrentTask{ID: "task", WorkspaceID: "workspace", Subject: subject, WorkerID: "worker", ClaimEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx = execution.WithOperationID(execution.WithInvocationID(execution.WithNodeID(ctx, "member"), "snapshot/0/member"), "member/segment/000000000001")
	activities := &teamrun.PGActivityStore{Transactions: pool}
	ctx = businessaction.WithActionOutcomeGuard(ctx, func(callCtx context.Context, event businessaction.ActionOutcomeEvent) (businessaction.ActionOutcomeReplay, error) {
		decision, err := activities.CheckBusinessActionReplay(callCtx, teamrun.BusinessActionReplayCheck{WorkspaceID: "workspace", RunID: "parent", NodeID: "member", InvocationID: event.InvocationID, CallID: event.CallID, OperationID: event.OperationID, InputRevisionID: event.InputRevisionID, CapabilityID: event.CapabilityID, RecordID: event.RecordID, ParamsSHA256: event.ParamsSHA256})
		return businessaction.ActionOutcomeReplay{Blocked: decision.Blocked, Status: decision.Status, SameOperation: decision.SameOperation, Result: decision.Result}, err
	})
	return businessaction.WithActionOutcomeRecorder(ctx, func(callCtx context.Context, event businessaction.ActionOutcomeEvent) error {
		raw, _ := json.Marshal(event)
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(event.OperationID+"/"+event.CallID+"/"+event.Phase)).String()
		return activities.RecordBusinessActionEvent(callCtx, teamrun.ActivityEvent{WorkspaceID: "workspace", RunID: "parent", NodeID: "member", MemberID: "agent", MemberVersion: 1, EventID: id, Kind: "business_action_" + event.Phase, Detail: raw, OccurredAt: time.Now().UTC()})
	})
}

func TestNativeConfirmationTaskHTTPAndScopeRealPG(t *testing.T) {
	// Captured through the public registerActionTools API in @objectstack/mcp
	// 17.3.0. Its closed native tool has no confirmation transport field.
	oldSchema, err := os.ReadFile("testdata/objectstack-17.3-run-action.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"confirmed", "old", "new-unconfirmed", "read-only-scope", "missing-protocol", "invalid-protocol", "changed-protocol", "native-schema-denied", "missing-authority", "wrong-actor", "wrong-record", "model-confirm", "param-confirm", "expired", "cancelled", "terminal", "revoked", "superseded", "replaced-task", "employee-only", "files-confirmed", "files-out-of-scope", "files-missing", "unknown", "http428", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			pool := testutil.PostgresPool(t)
			if err := db.Migrate(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
			f := &confirmationHTTPFixture{t: t, pool: pool, schema: confirmationHTTPToolSchema, required: true, mode: mode}
			if mode == "old" {
				f.required = false
				f.schema = string(oldSchema)
			}
			if mode == "new-unconfirmed" {
				f.required = false
			}
			if mode == "missing-protocol" {
				f.schema = string(oldSchema)
			}
			if mode == "invalid-protocol" {
				f.schema = strings.ReplaceAll(confirmationHTTPToolSchema, `"confirm":{"type":"boolean"}`, `"confirm":{"type":"string"}`)
			}
			if mode == "native-schema-denied" {
				f.schema = strings.ReplaceAll(confirmationHTTPToolSchema, `"confirm":{"type":"boolean"}`, `"confirm":{"type":"boolean","const":false}`)
			}
			f.hidden = mode == "employee-only"
			f.files = strings.HasPrefix(mode, "files-")
			f.server = httptest.NewServer(http.HandlerFunc(f.serve))
			t.Cleanup(f.server.Close)
			f.grant = memberTaskGrant(f.server.URL)
			if mode == "read-only-scope" {
				f.grant.Scope.AllowedActions = []string{}
				raw, _ := json.Marshal(f.grant.Scope)
				f.grant.ScopeSHA256, _ = frozen.HashCanonicalJSON(raw)
			}
			if f.files && mode != "files-missing" {
				f.grant.Scope.Resources = []businessaction.TaskDelegationResource{{Type: "forge-file", ID: "file-a", Name: "Synthetic.txt", Bytes: 3, SHA256: strings.Repeat("a", 64)}}
				raw, _ := json.Marshal(f.grant.Scope)
				f.grant.ScopeSHA256, _ = frozen.HashCanonicalJSON(raw)
			}
			key := []byte(strings.Repeat("k", 32))
			seedMemberActionRuntime(t, pool, f.server.URL, key, f.grant)
			ctx := confirmationTaskContext(t, pool)
			if mode == "missing-authority" {
				_, err := pool.Exec(ctx, `UPDATE weave_task_business_delegations SET revoked_at=now(),revocation_reason='employee_cancel'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong-actor" {
				subject := execution.Subject{WorkspaceID: "workspace", UserID: "another-employee"}
				var err error
				ctx, err = execution.WithCurrentTask(execution.WithSubject(ctx, subject), execution.CurrentTask{ID: "task", WorkspaceID: "workspace", Subject: subject, WorkerID: "worker", ClaimEpoch: 1})
				if err != nil {
					t.Fatal(err)
				}
			}
			factory := businessaction.Factory{Inner: workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
				return compiler.FrozenBuildOpts{Tools: memberEmptyTools{}}, nil, nil
			}), Store: businessaction.NewStore(pool, taskqueue.New(pool, nil, time.Minute), key)}
			bundle := frozen.FrozenExecutionBundle{Agent: frozen.FrozenAgentRecord{WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 1, BusinessCapabilityIDs: []string{memberActionCapability}}}
			opts, _, err := factory.Build(ctx, bundle, nil)
			if mode == "read-only-scope" {
				if err != nil {
					t.Fatal(err)
				}
				tools, err := opts.Tools.ListTools(ctx)
				if err != nil || len(tools) != 0 || len(f.wires) != 0 || f.effects.Load() != 0 {
					t.Fatal("read-only authorization acquired a native action")
				}
				return
			}
			buildDenied := mode == "missing-protocol" || mode == "invalid-protocol" || mode == "missing-authority" || mode == "wrong-actor" || mode == "employee-only" || mode == "files-missing"
			if buildDenied {
				if err == nil {
					t.Fatal("unsupported or unauthorized confirmation was offered")
				}
				if len(f.wires) != 0 || f.effects.Load() != 0 {
					t.Fatal("build refusal wrote business state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tools, err := opts.Tools.ListTools(ctx)
			if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "confirm") || strings.Contains(string(tools[0].InputSchema), "idempotency_key") {
				t.Fatal("system fields were exposed to the model", err)
			}
			args := `{"params":{"line_id":"same-line"}}`
			if mode == "files-confirmed" {
				args = `{"params":{"line_id":"same-line","material_file_id":"file-a"}}`
			}
			if mode == "files-out-of-scope" {
				args = `{"params":{"line_id":"same-line","material_file_id":"file-b"}}`
			}
			if mode == "wrong-record" {
				args = `{"recordId":"record-b","params":{"line_id":"same-line"}}`
			}
			if mode == "model-confirm" {
				args = `{"confirm":true,"params":{"line_id":"same-line"}}`
			}
			if mode == "param-confirm" {
				args = `{"params":{"line_id":"same-line","confirm":true}}`
			}
			if mode == "changed-protocol" {
				f.mu.Lock()
				f.schema = strings.ReplaceAll(confirmationHTTPToolSchema, `"required":["actionName"]`, `"required":["actionName","params"]`)
				f.mu.Unlock()
			}
			if mode == "expired" {
				f.onDiscovery = func() { f.mu.Lock(); f.grant.ExpiresAt = time.Now().Add(-time.Second); f.mu.Unlock() }
			}
			if mode == "superseded" {
				f.onDiscovery = func() { f.mu.Lock(); f.grant.Generation++; f.mu.Unlock() }
			}
			if mode == "cancelled" {
				if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='cancelled'`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "terminal" || mode == "replaced-task" {
				status := "completed"
				if mode == "replaced-task" {
					status = "superseded"
				}
				if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status=$1`, status); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "revoked" {
				if _, err := pool.Exec(ctx, `UPDATE weave_task_business_delegations SET revoked_at=now(),revocation_reason='employee_cancel'`); err != nil {
					t.Fatal(err)
				}
			}
			call := contract.ToolCall{ID: "first-call", Name: tools[0].Name, Args: args}
			result, err := opts.Tools.Dispatch(ctx, call)
			preDenied := mode == "changed-protocol" || mode == "native-schema-denied" || mode == "wrong-record" || mode == "model-confirm" || mode == "param-confirm" || mode == "expired" || mode == "cancelled" || mode == "terminal" || mode == "revoked" || mode == "superseded" || mode == "replaced-task" || mode == "files-out-of-scope"
			if preDenied {
				if err == nil && (result == nil || !result.IsError) {
					t.Fatal("invalid confirmation was not refused")
				}
				var events int
				if queryErr := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_activity_events WHERE kind LIKE 'business_action_%'`).Scan(&events); queryErr != nil {
					t.Fatal(queryErr)
				}
				if len(f.wires) != 0 || f.effects.Load() != 0 || events != 0 {
					t.Fatal("pre-dispatch refusal became an effect or unresolved operation")
				}
				return
			}
			if err != nil || result == nil || len(f.wires) != 1 {
				t.Fatal("native confirmation did not produce its single actual response", err)
			}
			wantUnknown := mode == "unknown" || mode == "http428"
			if wantUnknown && !result.StopLoop {
				t.Fatal("post-send uncertainty lost the stop/reconcile requirement")
			}
			if mode == "rejected" && (!result.IsError || !strings.Contains(result.Content, "ACTION_CONFIRMATION_REQUIRED")) {
				t.Fatal("original native confirmation refusal was discarded")
			}
			if (mode == "confirmed" || mode == "old" || mode == "new-unconfirmed" || mode == "files-confirmed") && result.IsError {
				t.Fatal("valid action was refused")
			}
			var original map[string]any
			_ = json.Unmarshal(f.wires[0], &original)
			delete(original, "confirm")
			raw, _ := json.Marshal(original)
			digest, _ := frozen.HashCanonicalJSON(raw)
			var storedDigest string
			if err := pool.QueryRow(ctx, `SELECT detail->>'params_sha256' FROM weave_team_run_activity_events WHERE kind='business_action_started'`).Scan(&storedDigest); err != nil || storedDigest != digest {
				t.Fatal("native transport confirmation changed the business-operation digest", err)
			}
			if mode == "old" && string(f.wires[0]) != string(raw) {
				t.Fatal("old native request bytes changed")
			}
			reads, lists := f.reads.Load(), f.lists.Load()
			call.ID = "recovered-call"
			replay, replayErr := opts.Tools.Dispatch(ctx, call)
			if replayErr != nil || replay == nil || len(f.wires) != 1 || f.reads.Load() != reads || f.lists.Load() != lists {
				t.Fatal("original operation was redispatched during receipt recovery", replayErr)
			}
			if !wantUnknown && replay.Content != businessaction.SanitizeActionOutcomeResult(result).Content {
				t.Fatal("recovery replaced the original native receipt")
			}
			if wantUnknown {
				next := execution.WithOperationID(ctx, "member/segment/000000000002")
				blocked, err := opts.Tools.Dispatch(next, call)
				if err != nil || blocked == nil || !blocked.StopLoop || len(f.wires) != 1 {
					t.Fatal("unknown action was replayed under another operation", err)
				}
			}
		})
	}
}
