package businessaction_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

const memberActionCapability = "forge:action:sales_quote.AdjustPrice"

// TestTrackedForgeActionMemberResumeRealPG assembles public production entry
// points, including delegation resolution and HTTP MCP. Only inference, Forge
// and the cancellation trigger are fixtures. It interrupts after the action
// receipt commits but before the member tool journal response commits, forcing
// the fresh member to consult the action ledger rather than its tool cache.
func TestTrackedForgeActionMemberResumeRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	config := pool.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	production, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(production.Close)

	var effects atomic.Int64
	var currentReads atomic.Int64
	var keysMu sync.Mutex
	var forgeKeys []string
	grant := memberTaskGrant("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == businessaction.TaskDelegationPath+"/current" {
			currentReads.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(grant)
			return
		}
		if r.URL.Path == businessaction.TaskDelegationPath+"/objects/sales_quote" {
			if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"object","name":"sales_quote","item":{"name":"sales_quote","actions":[{"name":"AdjustPrice","params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}}`))
			return
		}
		if r.URL.Path != businessaction.TaskDelegationPath+"/mcp" {
			t.Errorf("unexpected Forge endpoint %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
			t.Error("task-scoped Forge authorization not carried by HTTP host")
			w.WriteHeader(401)
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
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture-forge", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "list_actions", "inputSchema": json.RawMessage(`{"type":"object"}`)}, {"name": "run_action", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		case "tools/call":
			content := ""
			if req.Params.Name == "list_actions" {
				content = `{"actions":[{"name":"AdjustPrice","objectName":"sales_quote","label":"调整报价单价","requiresRecord":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}`
			} else if req.Params.Name == "run_action" {
				var action struct {
					Action string            `json:"actionName"`
					Object string            `json:"objectName"`
					Record string            `json:"recordId"`
					Params map[string]string `json:"params"`
				}
				if json.Unmarshal(req.Params.Arguments, &action) != nil || action.Action != "AdjustPrice" || action.Object != "sales_quote" || action.Record != "record-a" || action.Params["line_id"] != "same-line" || !strings.HasPrefix(action.Params["idempotency_key"], "weave-op-") {
					t.Error("Forge received an unfrozen action or missing system key")
					w.WriteHeader(400)
					return
				}
				// Assert that the side effect's identity already exists in the
				// actual MemberRunner journal, before the HTTP effect happens.
				var count int
				if err := production.QueryRow(r.Context(), `SELECT count(*) FROM loom_store WHERE namespace='member-operation:workspace' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'`).Scan(&count); err != nil || count == 0 {
					t.Errorf("effect preceded member tool intent: count=%d err=%v", count, err)
				}
				keysMu.Lock()
				forgeKeys = append(forgeKeys, action.Params["idempotency_key"])
				keysMu.Unlock()
				index := effects.Add(1)
				content = fmt.Sprintf(`{"ok":true,"data":{"business_receipt":"receipt-%d"}}`, index)
			} else {
				t.Errorf("unexpected MCP tool %s", req.Params.Name)
				w.WriteHeader(400)
				return
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": content}}}
		default:
			t.Errorf("unexpected MCP method %s", req.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	grant = memberTaskGrant(server.URL)
	key := []byte(strings.Repeat("k", 32))
	seedMemberActionRuntime(t, pool, server.URL, key, grant)

	subject := execution.Subject{WorkspaceID: "workspace", UserID: "employee"}
	ctx := execution.WithSubject(t.Context(), subject)
	ctx, err = execution.WithCurrentTask(ctx, execution.CurrentTask{ID: "task", WorkspaceID: "workspace", Subject: subject, WorkerID: "worker", ClaimEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx = execution.WithInvocationID(execution.WithNodeID(ctx, "member"), "snapshot/0/member")
	activities := &teamrun.PGActivityStore{Transactions: production}
	var interrupted atomic.Bool
	var actionDispatches atomic.Int64
	var slotsMu sync.Mutex
	var observedSlots []string
	var recoveredSlots []string
	interruptedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	withActivities := func(base context.Context) context.Context {
		base = execution.WithOperationReconciler(base, func(reconcileCtx context.Context, slot string, input json.RawMessage) (json.RawMessage, bool, error) {
			var call contract.ToolCall
			if err := json.Unmarshal(input, &call); err != nil {
				return nil, false, err
			}
			decision, err := activities.ReconcileBusinessActionOperation(reconcileCtx, teamrun.BusinessActionOperationReconcileCheck{
				WorkspaceID: "workspace", RunID: "parent", NodeID: "member", MemberID: "agent", InvocationID: execution.InvocationID(reconcileCtx), OperationSlot: slot,
			})
			if err != nil || !decision.SameOperation || decision.Result == nil {
				return nil, false, err
			}
			if decision.Result.CallID != call.ID || decision.Result.ToolName != call.Name {
				return nil, false, errors.New("journal input differs from trusted original action receipt")
			}
			slotsMu.Lock()
			recoveredSlots = append(recoveredSlots, slot)
			slotsMu.Unlock()
			receipt := *decision.Result
			receipt.CallID, receipt.ToolName = call.ID, call.Name
			encoded, err := json.Marshal(receipt)
			return encoded, err == nil, err
		})
		base = businessaction.WithActionOutcomeGuard(base, func(callCtx context.Context, event businessaction.ActionOutcomeEvent) (businessaction.ActionOutcomeReplay, error) {
			decision, err := activities.CheckBusinessActionReplay(callCtx, teamrun.BusinessActionReplayCheck{WorkspaceID: "workspace", RunID: "parent", NodeID: "member", InvocationID: event.InvocationID, CallID: event.CallID, OperationID: event.OperationID, InputRevisionID: event.InputRevisionID, CapabilityID: event.CapabilityID, RecordID: event.RecordID, ParamsSHA256: event.ParamsSHA256})
			return businessaction.ActionOutcomeReplay{Blocked: decision.Blocked, Status: decision.Status, SameOperation: decision.SameOperation, Result: decision.Result}, err
		})
		return businessaction.WithActionOutcomeRecorder(base, func(callCtx context.Context, event businessaction.ActionOutcomeEvent) error {
			raw, _ := json.Marshal(event)
			id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(event.OperationID+"/"+event.CallID+"/"+event.Phase)).String()
			err := activities.RecordBusinessActionEvent(callCtx, teamrun.ActivityEvent{WorkspaceID: "workspace", RunID: "parent", NodeID: "member", MemberID: "agent", MemberVersion: 1, EventID: id, Kind: "business_action_" + event.Phase, Detail: raw, OccurredAt: time.Now().UTC()})
			if err == nil && event.Phase == "result" && interrupted.CompareAndSwap(false, true) {
				cancel()
			}
			return err
		})
	}
	model := &memberActionModel{}
	bundle := frozen.FrozenExecutionBundle{FactoryKey: compiler.StandardFrozenToolsKey(), Agent: frozen.FrozenAgentRecord{WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 1, Name: "member", BusinessCapabilityIDs: []string{memberActionCapability}}}
	graph := func(buildCtx context.Context) *loom.Graph {
		factory := businessaction.Factory{Inner: workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
			return compiler.FrozenBuildOpts{LLM: model, Tools: memberEmptyTools{}}, nil, nil
		}), Store: businessaction.NewStore(production, taskqueue.New(production, nil, time.Minute), key)}
		opts, closer, err := factory.Build(buildCtx, bundle, nil)
		if err != nil {
			t.Fatal(err)
		}
		if closer != nil {
			t.Cleanup(func() { _ = closer.Close() })
		}
		tools, err := opts.Tools.ListTools(buildCtx)
		if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "idempotency_key") {
			t.Fatalf("actual frozen tool schema not protected: tools=%+v err=%v", tools, err)
		}
		model.tool = tools[0].Name
		opts.Tools = &memberActionObserver{inner: opts.Tools, onDispatch: func(callCtx context.Context) {
			actionDispatches.Add(1)
			slot := execution.OperationID(callCtx)
			slotsMu.Lock()
			observedSlots = append(observedSlots, slot)
			slotsMu.Unlock()
			var raw []byte
			if err := production.QueryRow(callCtx, `SELECT value FROM loom_store WHERE namespace='member-operation:workspace' AND key=$1`, slot).Scan(&raw); err != nil {
				t.Errorf("actual dispatcher slot not persisted: %v", err)
			}
			var op struct {
				Kind string `json:"kind"`
			}
			_ = json.Unmarshal(raw, &op)
			if op.Kind != "tool" {
				t.Errorf("dispatcher identity is not a durable tool intent: %s", raw)
			}
		}}
		opts = loomruntime.InstallFrozenMemberJournal(loomruntime.InstallFrozenUsageTracking(opts))
		g := loom.NewGraph("workspace:business-action-member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
		g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
		g.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
		return g
	}
	team, flow, snap, parent := "team", "workflow", "snapshot", "parent"
	version, seq := 1, int64(1)
	attribution, err := loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{Scope: loomruntime.TerminalAttributionFixedWorkflow, WorkspaceID: "workspace", TeamID: &team, WorkflowID: &flow, WorkflowVersion: &version, RunSnapshotID: &snap, ParentRunID: &parent, ParentSeq: &seq, AggregationParentRunID: &parent}, &loomruntime.TerminalSnapshotEvidence{WorkspaceID: "workspace", RunID: snap, TeamID: team, Mode: "fixed_workflow", WorkflowID: &flow, WorkflowVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	request := loomruntime.MemberRequest{WorkspaceID: "workspace", ParentRunID: "parent", RunSnapshotID: "snapshot", NodeID: "member", CallID: "member-invocation", ParentGeneration: 1, ArtifactHash: strings.Repeat("a", 64), Bundle: bundle, Input: loom.State{"messages": []contract.Message{{Role: "user", Content: "Perform two legitimate adjustments with the same line parameter."}}}, Attribution: attribution}
	parentGuard := func(generation int64) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			var current int64
			if err := tx.QueryRow(ctx, `SELECT team_run_generation FROM weave_team_runs WHERE workspace_id='workspace' AND run_id='parent' FOR UPDATE`).Scan(&current); err != nil {
				return err
			}
			if current != generation {
				return errors.New("parent epoch changed")
			}
			return nil
		}
	}
	request.ParentGuard = parentGuard(1)
	request.Graph = graph(ctx)
	runner, err := loomruntime.NewMemberRunner(storeext.New(production))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Run(withActivities(interruptedCtx), request); err == nil {
		t.Fatal("expected interruption between business receipt and member receipt")
	}
	if effects.Load() != 1 || actionDispatches.Load() != 1 {
		t.Fatalf("pre-recovery effects=%d dispatches=%d", effects.Load(), actionDispatches.Load())
	}
	var eventsBefore int
	if err := production.QueryRow(ctx, `SELECT count(*) FROM weave_team_run_activity_events WHERE workspace_id='workspace' AND run_id='parent' AND kind='business_action_result'`).Scan(&eventsBefore); err != nil || eventsBefore != 1 {
		t.Fatalf("business receipt was not committed before interruption: count=%d err=%v", eventsBefore, err)
	}
	var responsesBefore int
	if err := production.QueryRow(ctx, `SELECT count(*) FROM loom_store WHERE namespace='member-operation:workspace' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool' AND convert_from(value,'UTF8')::jsonb ? 'response'`).Scan(&responsesBefore); err != nil || responsesBefore != 0 {
		t.Fatalf("test did not interrupt before member tool receipt: count=%d err=%v", responsesBefore, err)
	}
	if _, err := production.Exec(ctx, `UPDATE weave_team_runs SET team_run_generation=2 WHERE workspace_id='workspace' AND run_id='parent'`); err != nil {
		t.Fatal(err)
	}
	request.ParentGeneration = 2
	request.ParentGuard = parentGuard(2)
	request.Graph = graph(ctx)
	fresh, err := loomruntime.NewMemberRunner(storeext.New(production))
	if err != nil {
		t.Fatal(err)
	}
	result, err := fresh.Run(withActivities(ctx), request)
	if err != nil || result == nil || result.StopReason != loom.StopCompleted {
		t.Fatalf("fresh recovery failed: result=%+v err=%v", result, err)
	}
	if effects.Load() != 2 || actionDispatches.Load() != 2 {
		t.Fatalf("same operation must read ledger without dispatch; next identical intent must send: effects=%d dispatches=%d", effects.Load(), actionDispatches.Load())
	}
	if currentReads.Load() < 2 {
		t.Fatalf("task-current authority was not checked online at build and dispatch: calls=%d", currentReads.Load())
	}

	if len(observedSlots) != 2 || len(recoveredSlots) != 1 || observedSlots[0] == "" || observedSlots[0] != recoveredSlots[0] || observedSlots[0] == observedSlots[1] {
		t.Fatalf("member tool slots changed incorrectly: dispatched=%v recovered=%v", observedSlots, recoveredSlots)
	}
	if result.State["__member_usage_incomplete"] != true {
		t.Fatalf("recovered receipt incorrectly claimed complete physical usage: %v", result.State["__member_usage_incomplete"])
	}
	var savedRecovery struct {
		Response        *contract.ToolResult `json:"response"`
		UsageIncomplete bool                 `json:"usage_incomplete"`
	}
	var savedRaw []byte
	if err := production.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace='member-operation:workspace' AND key=$1`, recoveredSlots[0]).Scan(&savedRaw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(savedRaw, &savedRecovery) != nil || savedRecovery.Response == nil || !savedRecovery.UsageIncomplete || !strings.Contains(savedRecovery.Response.Content, "receipt-1") {
		t.Fatalf("trusted receipt was not durably reconciled into the original journal slot: %s", savedRaw)
	}
	if len(forgeKeys) != 2 || forgeKeys[0] != execution.EngineOperationID("revision", "snapshot/0/member", observedSlots[0], memberActionCapability) || forgeKeys[1] != execution.EngineOperationID("revision", "snapshot/0/member", observedSlots[1], memberActionCapability) || forgeKeys[0] == forgeKeys[1] {
		t.Fatalf("HTTP keys not derived from durable operation slots: %v", forgeKeys)
	}
	events, err := activities.ListBusinessActionEvents(ctx, "workspace", "parent")
	if err != nil || len(events) != 4 {
		t.Fatalf("expected exactly two durable action outcomes: events=%d err=%v", len(events), err)
	}
	for index, event := range events {
		var outcome businessaction.ActionOutcomeEvent
		_ = json.Unmarshal(event.Detail, &outcome)
		if outcome.OperationID != forgeKeys[index/2] || (event.Kind == "business_action_result" && (outcome.Status != "succeeded" || outcome.Result == nil || !strings.Contains(outcome.Result.Content, fmt.Sprintf("receipt-%d", index/2+1)))) {
			t.Fatalf("native PG receipt lost operation linkage: %+v", outcome)
		}
	}
	if model.calls.Load() != 2 {
		t.Fatalf("recorded model request was not replayed: physical calls=%d", model.calls.Load())
	}
	if len(model.receipts) != 2 || !strings.Contains(model.receipts[0], "receipt-1") || !strings.Contains(model.receipts[1], "receipt-2") {
		t.Fatalf("member did not receive both original native business receipts: %v", model.receipts)
	}
}

func TestTrackedForgeOrganizationDenialIsNoEffectNotUnknownRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	config := pool.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	production, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(production.Close)

	var effects atomic.Int64
	var currentReads atomic.Int64
	var denied atomic.Bool
	grant := memberTaskGrant("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == businessaction.TaskDelegationPath+"/current" {
			currentReads.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture-task-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if denied.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"FORGE_TASK_ORGANIZATION_FORBIDDEN","no_effect":true,"phase":"authorization"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(grant)
			return
		}
		if r.URL.Path == businessaction.TaskDelegationPath+"/objects/sales_quote" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"object","name":"sales_quote","item":{"name":"sales_quote","actions":[{"name":"AdjustPrice","params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}}`))
			return
		}
		if r.URL.Path != businessaction.TaskDelegationPath+"/mcp" {
			t.Errorf("unexpected Forge endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture-forge", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "list_actions", "inputSchema": json.RawMessage(`{"type":"object"}`)}, {"name": "run_action", "inputSchema": json.RawMessage(`{"type":"object"}`)}}}
		case "tools/call":
			if req.Params.Name == "run_action" {
				effects.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if req.Params.Name != "list_actions" {
				t.Errorf("unexpected MCP tool %s", req.Params.Name)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{"actions":[{"name":"AdjustPrice","objectName":"sales_quote","label":"调整报价单价","requiresRecord":true,"params":[{"name":"line_id","type":"string","required":true},{"name":"idempotency_key","type":"string","required":true}]}]}`}}}
		default:
			t.Errorf("unexpected MCP method %s", req.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	grant = memberTaskGrant(server.URL)
	key := []byte(strings.Repeat("k", 32))
	seedMemberActionRuntime(t, pool, server.URL, key, grant)

	subject := execution.Subject{WorkspaceID: "workspace", UserID: "employee"}
	ctx := execution.WithSubject(t.Context(), subject)
	ctx, err = execution.WithCurrentTask(ctx, execution.CurrentTask{ID: "task", WorkspaceID: "workspace", Subject: subject, WorkerID: "worker", ClaimEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx = execution.WithInvocationID(execution.WithNodeID(ctx, "member"), "snapshot/0/member")
	model := &organizationDenialMemberModel{onFirstCall: func() { denied.Store(true) }}
	bundle := frozen.FrozenExecutionBundle{FactoryKey: compiler.StandardFrozenToolsKey(), Agent: frozen.FrozenAgentRecord{WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 1, Name: "member", BusinessCapabilityIDs: []string{memberActionCapability}}}
	factory := businessaction.Factory{Inner: workflow.RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		return compiler.FrozenBuildOpts{LLM: model, Tools: memberEmptyTools{}}, nil, nil
	}), Store: businessaction.NewStore(production, taskqueue.New(production, nil, time.Minute), key)}
	opts, closer, err := factory.Build(ctx, bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	tools, err := opts.Tools.ListTools(ctx)
	if err != nil || len(tools) != 1 {
		t.Fatalf("build task-scoped action schema: tools=%+v err=%v", tools, err)
	}
	model.tool = tools[0].Name
	opts = loomruntime.InstallFrozenMemberJournal(loomruntime.InstallFrozenUsageTracking(opts))
	g := loom.NewGraph("workspace:business-action-denial", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
	g.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
	team, flow, snap, parent := "team", "workflow", "snapshot", "parent"
	version, seq := 1, int64(1)
	attribution, err := loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{Scope: loomruntime.TerminalAttributionFixedWorkflow, WorkspaceID: "workspace", TeamID: &team, WorkflowID: &flow, WorkflowVersion: &version, RunSnapshotID: &snap, ParentRunID: &parent, ParentSeq: &seq, AggregationParentRunID: &parent}, &loomruntime.TerminalSnapshotEvidence{WorkspaceID: "workspace", RunID: snap, TeamID: team, Mode: "fixed_workflow", WorkflowID: &flow, WorkflowVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	request := loomruntime.MemberRequest{WorkspaceID: "workspace", ParentRunID: parent, RunSnapshotID: snap, NodeID: "member", CallID: "member-invocation", ParentGeneration: 1, ArtifactHash: strings.Repeat("a", 64), Bundle: bundle,
		Input: loom.State{"messages": []contract.Message{{Role: "user", Content: "Perform the published adjustment."}}}, Attribution: attribution, Graph: g,
		RetryableFailure: func(err error) bool {
			failure := teamrun.ClassifyFailure(err)
			return failure.Retryable || failure.AuthorizationRequired != nil
		},
		ParentGuard: func(guardCtx context.Context, tx pgx.Tx) error {
			var generation int64
			if err := tx.QueryRow(guardCtx, `SELECT team_run_generation FROM weave_team_runs WHERE workspace_id='workspace' AND run_id='parent' FOR UPDATE`).Scan(&generation); err != nil {
				return err
			}
			if generation != 1 {
				return errors.New("parent epoch changed")
			}
			return nil
		}}
	runner, err := loomruntime.NewMemberRunner(storeext.New(production))
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(ctx, request)
	proof, trusted := execution.AuthorizationRefusalFromError(err)
	if !trusted || proof.Code != execution.AuthorizationDeniedBeforeDispatch || proof.Renewable() ||
		errors.Is(err, loomruntime.ErrMemberOutcomeUnknown) || effects.Load() != 0 || model.calls.Load() != 1 {
		t.Fatalf("Forge's trusted organization denial was not a terminal no-effect failure: proof=%+v trusted=%v err=%v effects=%d model_calls=%d", proof, trusted, err, effects.Load(), model.calls.Load())
	}
	if currentReads.Load() < 3 {
		t.Fatalf("Factory, current authority and tool dispatch did not use the online task check: current reads=%d", currentReads.Load())
	}
	var operations int
	if err := production.QueryRow(ctx, `SELECT count(*) FROM loom_store WHERE namespace='member-operation:workspace' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'`).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("tool intent was not durably journaled exactly once: count=%d err=%v", operations, err)
	}
	var saved struct {
		AuthorizationRefusal *execution.AuthorizationRefusal `json:"authorization_refusal"`
		Response             json.RawMessage                 `json:"response"`
	}
	var raw []byte
	if err := production.QueryRow(ctx, `SELECT value FROM loom_store WHERE namespace='member-operation:workspace' AND convert_from(value,'UTF8')::jsonb->>'kind'='tool'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &saved) != nil || saved.AuthorizationRefusal == nil || !saved.AuthorizationRefusal.Valid() ||
		saved.AuthorizationRefusal.Renewable() || saved.AuthorizationRefusal.ReasonCode != "FORGE_TASK_ORGANIZATION_FORBIDDEN" || len(saved.Response) != 0 {
		t.Fatalf("trusted denial was not preserved at the original member intent: %s", raw)
	}
}

type organizationDenialMemberModel struct {
	tool        string
	calls       atomic.Int64
	onFirstCall func()
}

func (m *organizationDenialMemberModel) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	if m.calls.Add(1) == 1 {
		if m.onFirstCall != nil {
			m.onFirstCall()
		}
		return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "model-call-denied", Name: m.tool, Args: `{"params":{"line_id":"same-line"}}`}}}, nil
	}
	return &contract.ChatResponse{Content: "unexpected retry"}, nil
}

func (*organizationDenialMemberModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("stream unused")
}

type memberActionObserver struct {
	inner      contract.ToolDispatcher
	onDispatch func(context.Context)
}

func (o *memberActionObserver) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return o.inner.ListTools(ctx)
}
func (o *memberActionObserver) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	o.onDispatch(ctx)
	return o.inner.Dispatch(ctx, call)
}

type memberActionModel struct {
	tool     string
	calls    atomic.Int64
	receipts []string
}

func (m *memberActionModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls.Add(1)
	for _, message := range request.Messages {
		if message.Role == "tool" {
			m.receipts = append(m.receipts, message.Content)
		}
	}
	if len(m.receipts) > 0 {
		return &contract.ChatResponse{Content: "Two adjustments completed."}, nil
	}
	return &contract.ChatResponse{ToolCalls: []contract.ToolCall{{ID: "model-call-one", Name: m.tool, Args: `{"params":{"line_id":"same-line"}}`}, {ID: "model-call-two", Name: m.tool, Args: `{"params":{"line_id":"same-line"}}`}}}, nil
}
func (*memberActionModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("stream unused")
}

func memberTaskGrant(issuer string) businessaction.TaskDelegationGrant {
	scope := businessaction.TaskDelegationScope{
		InputRevisionID: "revision", RegistrationID: "registration", TaskSHA256: strings.Repeat("a", 64),
		WorkflowID: "workflow", WorkflowVersion: 1, AllowedActions: []string{memberActionCapability},
		Resources:      []businessaction.TaskDelegationResource{},
		BusinessRecord: &businessaction.TaskBusinessRecord{ObjectName: "sales_quote", RecordID: "record-a"},
	}
	raw, _ := json.Marshal(scope)
	scopeHash, _ := frozen.HashCanonicalJSON(raw)
	issuedAt := time.Now().UTC().Add(-time.Minute)
	grant := businessaction.TaskDelegationGrant{
		Version: "1", Active: true, TokenType: "forge_task", Issuer: issuer,
		GrantID: "task-grant-1", Generation: 1, IssuedAt: issuedAt,
		ExpiresAt: issuedAt.Add(20 * time.Minute), ScopeSHA256: scopeHash, Scope: scope,
	}
	grant.Subject.ID = "employee"
	grant.Subject.OrganizationID = "native-org"
	return grant
}

func seedMemberActionRuntime(t *testing.T, pool *pgxpool.Pool, issuer string, key []byte, grant businessaction.TaskDelegationGrant) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('workspace','workspace','Workspace'); INSERT INTO weave_teams(id,workspace_id,name) VALUES('team','workspace','Team')`); err != nil {
		t.Fatal(err)
	}
	trigger, definition := json.RawMessage(`{"schema_version":1}`), json.RawMessage(`{"schema_version":1}`)
	payload := frozen.ArtifactPayloadV1{SchemaVersion: frozen.ArtifactSchemaVersion, TriggerConfig: trigger, GraphDefinition: definition, Team: frozen.ArtifactTeamV1{WorkspaceID: "workspace", TeamID: "team", LeadAgentID: "agent"}, Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{}}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: frozen.ArtifactSchemaVersion, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: frozen.ArtifactCanonicalizationVersion, HashAlgorithm: frozen.ArtifactHashAlgorithm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := json.Marshal(frozen.ArtifactEnvelopeV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: frozen.ArtifactSchemaVersion, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: frozen.ArtifactCanonicalizationVersion, HashAlgorithm: frozen.ArtifactHashAlgorithm, ContentHash: hash, Payload: encoded})
	if _, err := pool.Exec(ctx, `
 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name) VALUES('workspace','workflow','team','Workflow');
 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by) VALUES('workspace','workflow',1,'draft',$1::jsonb,$2::jsonb,'fixture');
 UPDATE weave_team_workflow_versions SET status='published',published_at=now(),updated_at=now() WHERE workspace_id='workspace' AND workflow_id='workflow' AND version=1;
 INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload) VALUES('workspace','workflow',1,1,'rfc8785+jcs-preorder',1,'sha256',$3,$4::jsonb);
 INSERT INTO weave_team_workflow_candidates(workspace_id,workflow_id,workflow_version,content_hash,envelope_json,dependencies_json,expected_updated_at,created_by,created_at) VALUES('workspace','workflow',1,$3,$5::jsonb,'[]',now(),'fixture',now());`, string(trigger), string(definition), hash, string(encoded), string(envelope)); err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{Subject: execution.Subject{WorkspaceID: "workspace", UserID: "employee"}, RunID: "snapshot", WorkspaceID: "workspace", TeamID: "team", SnapshotSchemaVersion: 2, Mode: "fixed_workflow", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactWorkflowID: "workflow", ArtifactWorkflowVersion: 1, AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":true,"workers_enabled":true,"version_blocked":false,"decided_at":"2026-10-01T00:00:00Z"}`), RunAssociations: json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`), TriggerSourceV2: json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`), RuntimeAssignment: json.RawMessage(`{}`), SourceRef: "fixture", CandidateContentHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
 INSERT INTO weave_task_queue(id,workspace_id,identity_kind,identity_schema_version,workflow_id,workflow_version,status,worker_id,claim_epoch,lease_expires_at,payload,actor_subject,run_snapshot_id) VALUES('task','workspace','team_workflow',2,'workflow',1,'running','worker',1,now()+interval '1 hour','{}','{"workspace_id":"workspace","user_id":"employee"}','snapshot');
 INSERT INTO weave_team_runs(workspace_id,run_id,status,team_run_generation,current_executor_id,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,created_at,updated_at) VALUES('workspace','parent','running',1,'executor','team','workflow',1,'snapshot','api','task','fixture-parent',now(),now());
 INSERT INTO weave_dispatch_input_revisions(workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,source_messages,task,task_sha256,team_id,mode,workflow_id,workflow_version,client_request_id,execution_task,revision_kind,root_input_revision_id) VALUES('workspace','employee','session','revision','registration',repeat('a',64),'["fixture-message"]','Adjust price twice',repeat('a',64),'team','workflow','workflow',1,'client-request','Adjust price twice','initial','revision');
 INSERT INTO weave_run_delivery_state(workspace_id,run_snapshot_id,run_id,input_revision_id,workflow_id,workflow_version,published_digest,contract,contract_digest) VALUES('workspace','snapshot','parent','revision','workflow',1,$1,'null',repeat('a',64));`, hash); err != nil {
		t.Fatal(err)
	}
	credential := []byte("fixture-task-token")
	ciphertext, err := secret.Seal(key, credential)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(credential)
	recordRaw, _ := json.Marshal(map[string]string{"object_name": "sales_quote", "id": "record-a"})
	recordDigest := sha256.Sum256(recordRaw)
	resources, _ := json.Marshal([]map[string]string{{"type": "dispatch-input", "id": "revision", "sha256": strings.Repeat("a", 64)}, {"type": "forge-record", "id": "record-a", "object_name": "sales_quote", "sha256": hex.EncodeToString(recordDigest[:])}})
	if _, err := pool.Exec(ctx, `INSERT INTO weave_task_business_delegations(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,grant_id,scope_sha256,refresh_generation) VALUES('workspace','employee','revision',gen_random_uuid(),'fixture-ref',$1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,'workflow',1,$8,$9,$10,$11,1)`, issuer, grant.Subject.ID, grant.Subject.OrganizationID, ciphertext, hex.EncodeToString(digest[:]), `["forge:action:sales_quote.AdjustPrice"]`, string(resources), grant.IssuedAt, grant.ExpiresAt, grant.GrantID, grant.ScopeSHA256); err != nil {
		t.Fatal(err)
	}
}

// The standard host supplies a non-nil dispatcher even with no local tools.
type memberEmptyTools struct{}

func (memberEmptyTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (memberEmptyTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, errors.New("no local tools")
}
