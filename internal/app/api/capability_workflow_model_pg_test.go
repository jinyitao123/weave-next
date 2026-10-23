package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/app/deliveryverify"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

const capabilityWorkflowModelName = "review-model"

type capabilityWorkflowModelCase struct {
	name       string
	wantCalls  int32
	wantOutput string
}

type capabilityWorkflowModelTrace struct {
	Nodes []struct {
		NodeID  string          `json:"node_id"`
		AgentID string          `json:"agent_id"`
		Model   string          `json:"model"`
		Kind    string          `json:"kind"`
		Status  string          `json:"status"`
		Output  json.RawMessage `json:"output"`
	} `json:"nodes"`
}

// capabilityWorkflowModelPromptTokens is the input each controlled member
// reports; members differ so a missing or doubled member changes the total.
var capabilityWorkflowModelPromptTokens = map[string]int{
	"fixture-role-completeness": 100, "fixture-role-policy": 200, "fixture-role-accuracy": 400,
	"fixture-role-final": 800, "fixture-role-single": 50,
}

func TestCapabilityWorkflowModelSingleMemberRunsThroughPublishedLoomRealPG(t *testing.T) {
	runCapabilityWorkflowModelAcceptance(t, false)
}

func TestCapabilityWorkflowModelParallelReviewJoinsBeforeFinalMemberRealPG(t *testing.T) {
	runCapabilityWorkflowModelAcceptance(t, true)
}

func TestCapabilityWorkflowModelSingleMemberWorksWithOneConnectionRealPG(t *testing.T) {
	runCapabilityWorkflowModelAcceptance(t, false, 1)
}

func TestCapabilityWorkflowModelParallelReviewWorksWithOneConnectionRealPG(t *testing.T) {
	runCapabilityWorkflowModelAcceptance(t, true, 1)
}

func runCapabilityWorkflowModelAcceptance(t *testing.T, parallel bool, maxConns ...int32) {
	t.Helper()
	runCapabilityWorkflowModelScenario(t, parallel, maxConns, false)
}

func TestCapabilityWorkflowModelProductionCancellationPersistsLiveProgressRealPG(t *testing.T) {
	runCapabilityWorkflowModelScenario(t, false, nil, true)
}

func runCapabilityWorkflowModelScenario(t *testing.T, parallel bool, maxConns []int32, cancelRunning bool) {
	t.Helper()
	ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if len(maxConns) != 0 {
		config := pool.Config()
		config.MaxConns = maxConns[0]
		config.MinConns = 0
		limitedPool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatalf("create invocation pool with max_conns=%d: %v", maxConns[0], err)
		}
		t.Cleanup(limitedPool.Close)
		pool = limitedPool
		if pool.Config().MaxConns != maxConns[0] {
			t.Fatalf("invocation pool max_conns=%d, want %d", pool.Config().MaxConns, maxConns[0])
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','Workspace');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('user','ws','user','unused','admin')`); err != nil {
		t.Fatal(err)
	}

	var modelCalls atomic.Int32
	var requestMu sync.Mutex
	var requests [][]byte
	modelStarted, modelStopped, releaseModel := make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read local model request body: %v", err)
		}
		requestMu.Lock()
		requests = append(requests, append([]byte(nil), body...))
		requestMu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer controlled-model-secret" {
			t.Errorf("unexpected model transport request: method=%s path=%s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode local model request: %v", err)
		}
		if request.Model != capabilityWorkflowModelName || len(request.Tools) != 0 {
			t.Errorf("pure review workflow sent an unexpected model/tool request: model=%q tools=%d", request.Model, len(request.Tools))
		}
		if cancelRunning {
			close(modelStarted)
			defer close(modelStopped)
			select {
			case <-r.Context().Done():
			case <-releaseModel:
			}
			return
		}
		content := "审核完成：资料已核对。"
		finalWorkerRequest := false
		promptTokens := 0
		for marker, response := range map[string]string{
			"fixture-role-completeness": "完整性复核：已检查所需资料。",
			"fixture-role-policy":       "规则复核：资料符合约定要求。",
			"fixture-role-accuracy":     "准确性复核：关键日期一致。",
			"fixture-role-final":        "综合结论：材料完整，内容符合要求。",
			"fixture-role-single":       "资料复核结论：材料完整，日期一致。",
		} {
			if strings.Contains(string(body), marker) {
				content = response
				finalWorkerRequest = marker == "fixture-role-final"
				promptTokens = capabilityWorkflowModelPromptTokens[marker]
				break
			}
		}
		if finalWorkerRequest {
			for _, review := range []string{"完整性复核：已检查所需资料。", "规则复核：资料符合约定要求。", "准确性复核：关键日期一致。"} {
				if !strings.Contains(string(body), review) {
					t.Errorf("final worker did not receive joined review output %q", review)
				}
			}
		}
		firstChunk, _ := json.Marshal(map[string]any{
			"id": "controlled-model-response", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}},
		})
		lastChunk := `{"id":"controlled-model-response","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", firstChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", lastChunk)
		// Each member reports its own spend, so the root total shows whether
		// every member, parallel legs included, was counted exactly once.
		usageChunk, _ := json.Marshal(map[string]any{
			"id": "controlled-model-response", "object": "chat.completion.chunk", "choices": []any{},
			"usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": promptTokens / 10, "total_tokens": promptTokens + promptTokens/10},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", usageChunk)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	defer close(releaseModel)

	key := []byte(strings.Repeat("k", 32))
	providers := credentials.New(pool, key)
	providerService := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", ServiceID: "provider:fixture"})
	if err := providers.Upsert(providerService, "ws", llmrouter.ProviderConfig{
		CredentialScope: frozen.CredentialScopeWorkspaceService, CredentialServiceID: "provider:fixture",
		ID: "fixture", Name: "Controlled local model", BaseURL: provider.URL, APIKey: "controlled-model-secret",
		Models: []string{capabilityWorkflowModelName},
	}); err != nil {
		t.Fatalf("install workspace model provider: %v", err)
	}

	caseData := capabilityWorkflowModelCase{name: "single review", wantCalls: 1, wantOutput: "资料复核结论：材料完整，日期一致。"}
	workerPrompts := map[string]string{}
	if parallel {
		caseData = capabilityWorkflowModelCase{name: "parallel review", wantCalls: 4, wantOutput: "综合结论：材料完整，内容符合要求。"}
		workerPrompts = map[string]string{
			"completeness": "你是资料完整性复核员，fixture-role-completeness。只检查材料是否齐全。",
			"policy":       "你是资料规则复核员，fixture-role-policy。只核对资料是否符合规则。",
			"accuracy":     "你是资料准确性复核员，fixture-role-accuracy。只核对关键日期。",
			"final":        "你是最终资料评审员，fixture-role-final。结合并行复核意见形成综合结论。",
		}
	} else {
		workerPrompts = map[string]string{"single": "你是资料复核员，fixture-role-single。核对所给资料并给出结论。"}
	}

	artifacts, flows, authority, leadID, workerIDs := publishCapabilityModelWorkflow(t, ctx, pool, key, providers, workerPrompts)
	deliveredWorkerID := workerIDs["single"]
	if parallel {
		deliveredWorkerID = workerIDs["final"]
	}
	// `publishCapabilityModelWorkflow` uses the same platform publication authority as a
	// normal publish. The API handler and background TeamRun worker below then own
	// invocation, credential authorization, task execution, terminal state and result.

	leaseDuration := 250 * time.Millisecond
	if cancelRunning {
		leaseDuration = 3 * temporaryTeamRunTaskHeartbeat
	}
	tasks := taskqueue.New(pool, nil, leaseDuration)
	snapshots := snapshot.NewStore(pool)
	access := capabilities.NewAccessStore(pool)
	capabilityStore := capabilities.NewPGStore(pool)
	server := &Server{
		Echo: echo.New(), Config: &config.Config{JWTSecret: "capability-model-test"},
		Pool: pool, Store: teamDispatchPoolStore{pool: pool}, UserStore: users.NewStore(pool),
		Registry: agentcatalog.New(pool), Descriptors: memberIntegrationDescriptors(t),
		Tasks: tasks, Snapshots: snapshots, WorkflowArtifacts: artifacts, Workflow: flows,
		Deliverables: deliveryverify.NewStore(pool), StoreExt: storeext.New(pool),
		Fanout: fanout.New(pool, nil), Runtimes: runtimes.NewStore(pool),
		Credentials: providers, MCPRegistry: mcpregistry.New(pool, key), DeliveryTargets: delivery.New(pool, key),
		Capabilities: capabilities.NewService(capabilityStore), CapabilityAccess: access,
		PublicationAuthority: authority,
	}
	server.KernelPublication = openAPIKernelPublication(t, ctx, pool, authority)
	server.Models = llmrouter.NewResolver(llmrouter.New(capabilityWorkflowModelName))
	server.Models.SetSource(providers)
	if err := server.ConfigureTeamRunWorkers(); err != nil {
		t.Fatal(err)
	}
	if server.teamRunWorkers == nil || server.teamRunCancel == nil || server.workflowFanoutReconciler == nil {
		t.Fatal("production TeamRun workers or cancellation service were not configured")
	}
	server.registerRoutes()

	inputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`)
	outputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	definition := capability.Definition{
		SchemaVersion: 1, CapabilityID: "materials_review", Name: "材料评审",
		InputSchema: inputSchema, OutputSchema: outputSchema,
		Workflow: &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 1},
	}
	if err := server.Capabilities.SaveDraft(ctx, capabilities.DraftRequest{WorkspaceID: "ws", Definition: definition}); err != nil {
		t.Fatalf("save workflow-backed capability: %v", err)
	}
	if _, err := server.Capabilities.Publish(ctx, "ws", definition.CapabilityID, 1); err != nil {
		t.Fatalf("publish workflow-backed capability: %v", err)
	}
	appAccess := &capabilityWorkflowContractFixture{server: server, pool: pool, access: access}
	app, appKey := appAccess.application(t, "materials review app")
	appAccess.grant(t, app.ID, definition.CapabilityID, true)

	server.teamRunWorkers.Start()
	t.Cleanup(server.teamRunWorkers.Stop)
	server.workflowFanoutReconciler.Start()
	t.Cleanup(server.workflowFanoutReconciler.Stop)
	call := appAccess.call
	if len(maxConns) != 0 {
		call = func(t *testing.T, key, method, path string, body any) capabilityWorkflowHTTPResult {
			timeout := 300 * time.Millisecond
			if method == http.MethodPost {
				timeout = 3 * time.Second
			}
			return capabilityWorkflowCallWithTimeout(t, appAccess, key, method, path, body, timeout)
		}
	}
	accepted := call(t, appKey, http.MethodPost, workflowInvocationPath(definition.CapabilityID, 1), map[string]any{
		"request_id": "acceptance-" + caseData.name,
		"input":      map[string]any{"document": "合同附件目录、双方名称及签署日期。"},
	})
	if accepted.status != http.StatusAccepted {
		t.Fatalf("capability app invocation status=%d body=%+v", accepted.status, accepted.body)
	}
	invocationID, runID := accepted.stringField(t, "invocation_id"), accepted.stringField(t, "run_id")
	if invocationID == "" || runID == "" || accepted.stringField(t, "status") == "" {
		t.Fatalf("accepted response lost invocation or run identity: %+v", accepted.body)
	}
	if cancelRunning {
		assertCapabilityWorkflowModelCancellation(t, appAccess, appKey, invocationID, runID, modelStarted, modelStopped)
		if got := modelCalls.Load(); got != 1 {
			t.Fatalf("cancellation dispatched extra model calls: %d", got)
		}
		return
	}

	deadlineDuration := 20 * time.Second
	if len(maxConns) != 0 {
		deadlineDuration = 5 * time.Second
		if parallel {
			deadlineDuration = 12 * time.Second
		}
	}
	deadline := time.Now().Add(deadlineDuration)
	var completed capabilityWorkflowHTTPResult
	for time.Now().Before(deadline) {
		completed = call(t, appKey, http.MethodGet, "/v1/invocations/"+invocationID, nil)
		if completed.status != http.StatusOK {
			if len(maxConns) == 0 {
				t.Fatalf("invocation GET status=%d body=%+v", completed.status, completed.body)
			}
			time.Sleep(25 * time.Millisecond)
			continue
		}
		status := completed.stringField(t, "status")
		if status == "completed" {
			break
		}
		if status == "failed" || status == "cancelled" {
			t.Fatalf("published workflow ended %q before completion: %+v; %s", status, completed.body,
				capabilityWorkflowModelRunDiagnostics(ctx, pool, tasks, runID))
		}
		time.Sleep(25 * time.Millisecond)
	}
	lastStatus := ""
	_ = json.Unmarshal(completed.body["status"], &lastStatus)
	if lastStatus != "completed" {
		if len(maxConns) != 0 {
			server.workflowFanoutReconciler.Stop()
			server.teamRunWorkers.Stop()
		}
		t.Fatalf("published workflow did not reach completed: %+v; model calls=%d; %s", completed.body, modelCalls.Load(),
			capabilityWorkflowModelRunDiagnostics(ctx, pool, tasks, runID))
	}
	if completed.stringField(t, "run_id") != runID || completed.stringField(t, "workflow_id") != "flow" || completed.intField(t, "workflow_version") != 1 {
		t.Fatalf("completed projection changed its published run identity: %+v", completed.body)
	}

	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	teamRun, err := runs.Get(ctx, "ws", runID)
	if err != nil || teamRun.Status != teamrun.StatusSucceeded {
		t.Fatalf("completed API projection is not backed by a succeeded TeamRun: status=%s err=%v", teamRun.Status, err)
	}
	var result struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(completed.body["result"], &result); err != nil || result.Summary != caseData.wantOutput {
		t.Fatalf("published output schema/result mismatch: result=%s err=%v result_state=%s error=%s; %s",
			completed.body["result"], err, completed.stringField(t, "result_state"), completed.stringField(t, "error"),
			capabilityWorkflowModelRunDiagnostics(ctx, pool, tasks, runID))
	}
	var physical struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	wantInput := capabilityWorkflowModelPromptTokens["fixture-role-single"]
	if parallel {
		wantInput = 0
		for _, marker := range []string{"fixture-role-completeness", "fixture-role-policy", "fixture-role-accuracy", "fixture-role-final"} {
			wantInput += capabilityWorkflowModelPromptTokens[marker]
		}
	}
	if err := json.Unmarshal(completed.body["physical_usage"], &physical); err != nil ||
		physical.InputTokens != wantInput || physical.OutputTokens != wantInput/10 {
		t.Fatalf("root usage = %s (err=%v), want input=%d output=%d: every member, parallel legs included, counted once",
			completed.body["physical_usage"], err, wantInput, wantInput/10)
	}
	// Every member reported tokens and none ran again, so nothing is unknown.
	var complete bool
	if err := json.Unmarshal(completed.body["usage_complete"], &complete); err != nil || !complete {
		t.Fatalf("usage_complete = %s (err=%v), want true", completed.body["usage_complete"], err)
	}
	if got := modelCalls.Load(); got != caseData.wantCalls {
		t.Fatalf("controlled provider calls=%d, want %d", got, caseData.wantCalls)
	}
	if completed.stringField(t, "agent_id") != deliveredWorkerID {
		t.Fatalf("result producer agent=%q, want delivered final worker %q (unexecuted team lead %q)",
			completed.stringField(t, "agent_id"), deliveredWorkerID, leadID)
	}
	if completed.stringField(t, "agent_id") == leadID || completed.stringField(t, "model") != capabilityWorkflowModelName || completed.stringField(t, "engine") != "loom" {
		t.Fatalf("completion did not project final Loom worker provenance: %+v", completed.body)
	}
	var trace capabilityWorkflowModelTrace
	if err := json.Unmarshal(completed.body["trace"], &trace); err != nil {
		t.Fatalf("decode completed worker trace: %v", err)
	}
	wantWorkerNodes := []string{"review"}
	if parallel {
		wantWorkerNodes = []string{"review_completeness", "review_policy", "review_accuracy", "final_review"}
	}
	seenNodes := make(map[string]bool, len(wantWorkerNodes))
	for _, node := range trace.Nodes {
		if node.Kind != "worker" {
			continue
		}
		seenNodes[node.NodeID] = true
		if node.Status != "completed" || node.AgentID == "" || node.Model != capabilityWorkflowModelName || len(node.Output) == 0 || string(node.Output) == "null" {
			t.Fatalf("worker trace omitted its executed result or provenance: %+v", node)
		}
	}
	for _, nodeID := range wantWorkerNodes {
		if !seenNodes[nodeID] {
			t.Fatalf("worker node %q is absent from completed trace: %+v", nodeID, trace.Nodes)
		}
	}
	if len(seenNodes) != len(wantWorkerNodes) {
		t.Fatalf("unexpected executed worker trace nodes: got=%v want=%v", seenNodes, wantWorkerNodes)
	}
	if parallel && traceAgentForNode(trace, "final_review") != workerIDs["final"] {
		t.Fatalf("final review node is not bound to the published final worker: trace=%+v", trace.Nodes)
	}
	// Every worker, fanout legs included, reports its model call on the
	// parent run under its own node and member, once per phase, without any
	// prompt, material or response text.
	rows, err := pool.Query(ctx, `SELECT COALESCE(node_id,''),COALESCE(member_id,''),kind,detail::text FROM weave_team_run_activity_events
		WHERE workspace_id='ws' AND run_id=$1 AND kind IN ('model_started','model_completed')`, runID)
	if err != nil {
		t.Fatal(err)
	}
	phases := map[string]int{}
	for rows.Next() {
		var nodeID, memberID, kind, detail string
		if err := rows.Scan(&nodeID, &memberID, &kind, &detail); err != nil {
			t.Fatal(err)
		}
		if memberID != traceAgentForNode(trace, nodeID) {
			t.Fatalf("model activity on node %q names member %q, trace has %q", nodeID, memberID, traceAgentForNode(trace, nodeID))
		}
		for _, private := range []string{"检查附件", "核实材料", "核对材料", caseData.wantOutput, "document"} {
			if strings.Contains(detail, private) {
				t.Fatalf("model activity detail carries %q: %s", private, detail)
			}
		}
		phases[nodeID+"/"+kind]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range wantWorkerNodes {
		for _, kind := range []string{"model_started", "model_completed"} {
			if phases[nodeID+"/"+kind] != 1 {
				t.Fatalf("worker %q recorded %d %s events, want 1: %v", nodeID, phases[nodeID+"/"+kind], kind, phases)
			}
		}
	}

	requestMu.Lock()
	defer requestMu.Unlock()
	if len(requests) != int(caseData.wantCalls) {
		t.Fatalf("captured HTTP model requests=%d, want %d", len(requests), caseData.wantCalls)
	}
	if strings.Contains(strings.ToLower(string(requests[0])), "tool_choice") {
		t.Fatal("pure computation invocation unexpectedly requested a model tool call")
	}
	// The member that froze a reasoning effort sends exactly it. Members that
	// left it unset send no reasoning_effort, so the provider default applies.
	for _, body := range requests {
		var wire struct {
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		finalMember := strings.Contains(string(body), "fixture-role-final")
		switch {
		case finalMember && (wire.ReasoningEffort == nil || *wire.ReasoningEffort != "high"):
			t.Fatalf("member with reasoning_effort high sent %v", wire.ReasoningEffort)
		case !finalMember && wire.ReasoningEffort != nil:
			t.Fatalf("member without a reasoning effort sent reasoning_effort %q", *wire.ReasoningEffort)
		}
	}
}

func assertCapabilityWorkflowModelCancellation(t *testing.T, fixture *capabilityWorkflowContractFixture, key, invocationID, runID string, modelStarted, modelStopped <-chan struct{}) {
	t.Helper()
	select {
	case <-modelStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production worker did not start the controlled model request")
	}
	var liveEvents int
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_activity_events
			WHERE workspace_id='ws' AND run_id=$1 AND kind IN ('model_started','model_request_written')`, runID).Scan(&liveEvents); err != nil {
			t.Fatal(err)
		}
		if liveEvents == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if liveEvents != 2 {
		t.Fatalf("in-flight request has no durable progress: count=%d", liveEvents)
	}
	started := time.Now()
	cancelled := fixture.call(t, key, http.MethodPost, "/v1/invocations/"+invocationID+"/cancel", nil)
	if cancelled.status != http.StatusOK {
		t.Fatalf("cancel request returned status=%d", cancelled.status)
	}
	deadline := started.Add(10 * time.Second)
	var status string
	var terminal capabilityWorkflowHTTPResult
	for time.Now().Before(deadline) {
		result := fixture.call(t, key, http.MethodGet, "/v1/invocations/"+invocationID, nil)
		status = result.stringField(t, "status")
		if status == "cancelled" {
			terminal = result
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "cancelled" {
		t.Fatalf("production cancellation did not settle within caller's window: status=%s", status)
	}
	var complete *bool
	if err := json.Unmarshal(terminal.body["usage_complete"], &complete); err != nil || complete == nil || *complete {
		t.Fatalf("unreturned model receipt was reported as complete usage: value=%s err=%v", terminal.body["usage_complete"], err)
	}
	select {
	case <-modelStopped:
	default:
		t.Fatal("task acknowledged cancellation before the HTTP request exited")
	}
	var phase string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT state FROM weave_run_attempt_leases WHERE workspace_id='ws' AND run_id=$1`, runID).Scan(&phase); err != nil || phase == "active" {
		t.Fatalf("cancelled attempt remains active: state=%s err=%v", phase, err)
	}
	var cancellationMarkers, endEvents int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_run_terminal_markers WHERE workspace_id='ws' AND run_id=$1 AND stop_reason='cancelled'`, runID).Scan(&cancellationMarkers); err != nil || cancellationMarkers != 1 {
		t.Fatalf("cancellation has no root terminal evidence: count=%d err=%v", cancellationMarkers, err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_run_activity_events WHERE workspace_id='ws' AND run_id=$1 AND kind='model_cancelled'`, runID).Scan(&endEvents); err != nil || endEvents != 1 {
		t.Fatalf("cancelled context swallowed model end evidence: count=%d err=%v", endEvents, err)
	}
}

func capabilityWorkflowModelRunDiagnostics(ctx context.Context, pool *pgxpool.Pool, tasks *taskqueue.Store, runID string) string {
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	run, runErr := runs.Get(ctx, "ws", runID)
	if runErr != nil {
		return fmt.Sprintf("TeamRun lookup error=%v", runErr)
	}
	task, taskErr := tasks.Get(ctx, "ws", run.SourceTaskID)
	code, cause := "", ""
	if run.ErrorCode != nil {
		code = string(*run.ErrorCode)
	}
	if run.CauseSummary != nil {
		cause = *run.CauseSummary
	}
	var sourceTaskStatus any
	if task != nil {
		sourceTaskStatus = task.Status
	}
	events, eventErr := (&teamrun.PGActivityStore{Transactions: pool}).List(ctx, "ws", runID, 100)
	activitySummary := make([]string, 0, len(events))
	for _, event := range events {
		activitySummary = append(activitySummary, fmt.Sprintf("%s/%s:%s", event.Kind, event.NodeID, event.Detail))
	}
	var runTasks []map[string]any
	rows, queryErr := pool.Query(ctx, `SELECT id,status,result,COALESCE(error,'') FROM weave_task_queue WHERE workspace_id='ws' AND run_id=$1 ORDER BY created_at,id`, runID)
	if queryErr == nil {
		defer rows.Close()
		for rows.Next() {
			var id, status, taskError string
			var result json.RawMessage
			if rows.Scan(&id, &status, &result, &taskError) == nil {
				runTasks = append(runTasks, map[string]any{"id": id, "status": status, "result": string(result), "error": taskError})
			}
		}
	}
	return fmt.Sprintf("TeamRun status=%s error_code=%q cause=%q task_status=%v task_error=%v run_tasks=%+v task_query_error=%v activity=%+v activity_error=%v",
		run.Status, code, cause, sourceTaskStatus, taskErr, runTasks, queryErr, activitySummary, eventErr)
}

func traceAgentForNode(trace capabilityWorkflowModelTrace, nodeID string) string {
	for _, node := range trace.Nodes {
		if node.NodeID == nodeID {
			return node.AgentID
		}
	}
	return ""
}

func capabilityWorkflowCallWithTimeout(
	t *testing.T,
	f *capabilityWorkflowContractFixture,
	key, method, path string,
	body any,
	timeout time.Duration,
) capabilityWorkflowHTTPResult {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal capability request: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	request := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.server.Echo.ServeHTTP(response, request)
	result := capabilityWorkflowHTTPResult{status: response.Code, body: map[string]json.RawMessage{}}
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &result.body); err != nil {
			t.Fatalf("decode timeout-bounded %s %s response %d: %s (%v)", method, path, response.Code, response.Body.String(), err)
		}
	}
	return result
}

func publishCapabilityModelWorkflow(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	key []byte,
	providers *credentials.Store,
	workerPrompts map[string]string,
) (*workflow.ArtifactStore, *workflowcatalog.Store, *teamconstruction.PublicationAuthority, string, map[string]string) {
	t.Helper()
	lead := &registry.AgentRecord{
		Name: "materials-lead", DisplayName: "未执行协调员", Role: "avatar", Engine: "loom",
		Model: capabilityWorkflowModelName, GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "不在此流程执行的协调员。"},
	}
	leadRegistry := agentcatalog.New(pool)
	if err := leadRegistry.Put(ctx, "ws", lead); err != nil {
		t.Fatal(err)
	}
	workerIDs := make(map[string]string, len(workerPrompts))
	workers := make([]*registry.AgentRecord, 0, len(workerPrompts))
	for role, prompt := range workerPrompts {
		record := &registry.AgentRecord{
			Name: "materials-" + role, DisplayName: "资料" + role, Role: "worker", Engine: "loom",
			Model: capabilityWorkflowModelName, GraphType: "standard", Permissions: registry.PermissionConfig{Deny: []string{"*"}},
			Spec: stdlib.AgentSpec{SystemPrompt: prompt},
		}
		if role == "final" {
			// Only this member chooses a reasoning effort; the others leave it unset.
			record.RequestProfile = &frozen.FrozenRequestProfile{ReasoningEffort: "high"}
		}
		if err := leadRegistry.Put(ctx, "ws", record); err != nil {
			t.Fatalf("create published Loom worker %q: %v", role, err)
		}
		workerIDs[role] = record.ID
		workers = append(workers, record)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','ws','Materials review','`+lead.ID+`','active')`); err != nil {
		t.Fatal(err)
	}
	workerRepo := agentcatalog.NewTeamWorkerRepository(pool)
	for _, worker := range workers {
		if _, err := workerRepo.Create(ctx, "ws", registry.TeamWorker{
			TeamID: "team", WorkerAgentID: worker.ID, Duty: "Review materials", AllowedKinds: []string{"consult", "dispatch"},
			DefaultKind: "consult", ResultRequirement: "核对指定资料并给出简洁结论。", Enabled: true,
		}); err != nil {
			t.Fatalf("authorize published worker %q: %v", worker.ID, err)
		}
	}

	graph := capabilityWorkflowModelGraph(workerIDs, len(workerIDs) > 1)
	artifacts := workflow.NewArtifactStore(pool, nil)
	flows := workflowcatalog.New(pool, nil, artifacts)
	trigger := json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: "ws", TeamID: "team", Name: "Published materials review"}, workflow.DraftInput{
		CreatedBy: "user", TriggerConfig: trigger, GraphDefinition: graph,
	})
	if err != nil {
		t.Fatalf("create published workflow draft: %v", err)
	}
	descriptors := memberIntegrationDescriptors(t)
	builder := workflowcatalog.NewCandidateBuilder(flows, leadRegistry, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), descriptors)
	authority, publications := openAPIProductPublication(t, ctx, pool, builder)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "ws", WorkflowID: "flow", WorkflowVersion: draft.Version})
	if err != nil || candidate == nil || report != nil && len(report.Issues) > 0 {
		_ = tx.Rollback(ctx)
		t.Fatalf("build published pure Loom candidate: candidate=%v report=%+v err=%v", candidate != nil, report, err)
	}
	command, err := teamconstruction.PublicationCommandForCandidate("materials-review-"+fmt.Sprint(time.Now().UnixNano()), candidate, teamconstruction.PublicationTarget{TeamID: "team"})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := publications.Publish(ctx, command); err != nil {
		t.Fatalf("publish candidate through platform publication service: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT lead_avatar_id FROM weave_teams WHERE workspace_id='ws' AND id='team'`).Scan(&lead.ID); err != nil {
		t.Fatal(err)
	}
	return artifacts, flows, authority, lead.ID, workerIDs
}

func capabilityWorkflowModelGraph(workerIDs map[string]string, parallel bool) json.RawMessage {
	inputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"document":{"type":"string"}},"required":["document"]}`)
	outputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	workerNode := func(id, agentID string, agentVersion int64, kind, prompt, inputNode string) map[string]any {
		var taskValue map[string]any
		var expected string
		if inputNode == "" {
			taskValue = map[string]any{"source": "run_input", "path": "/document"}
			expected = "text"
		} else {
			taskValue = map[string]any{"source": "node_output", "node_id": inputNode, "path": ""}
			expected = "json"
		}
		return map[string]any{
			"id": id, "type": "worker", "label": id,
			"config": map[string]any{"kind": kind, "agent_id": agentID, "agent_version": agentVersion, "result_requirement": prompt},
			"inputs": map[string]any{"task": map[string]any{"expected_type": expected, "value": taskValue}},
			"output": map[string]any{"type": "text"},
		}
	}
	ref := func(nodeID string) map[string]any {
		return map[string]any{"source": "node_output", "node_id": nodeID, "path": ""}
	}
	objectOutput := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"summary": map[string]any{"type": "string"}}, "required": []string{"summary"}}
	fields := map[string]any{"summary": ref("review")}
	nodes := make([]any, 0, 10)
	edges := make([]any, 0, 14)
	entry := "review"
	if parallel {
		entry = "fanout"
		nodes = append(nodes, map[string]any{"id": "fanout", "type": "parallel", "config": map[string]any{"join_node_id": "join"}})
		for _, item := range []struct{ id, role, requirement string }{
			{"review_completeness", "completeness", "检查附件与目录是否齐全。"},
			{"review_policy", "policy", "核实材料是否符合约定要求。"},
			{"review_accuracy", "accuracy", "核对材料中的关键日期。"},
		} {
			nodes = append(nodes, workerNode(item.id, workerIDs[item.role], 1, "dispatch", item.requirement, ""))
			edges = append(edges, map[string]any{"id": "branch-" + item.role, "from_node_id": "fanout", "to_node_id": item.id, "route": "branch"})
			edges = append(edges, map[string]any{"id": "join-" + item.role, "from_node_id": item.id, "to_node_id": "join", "route": "join"})
		}
		nodes = append(nodes, map[string]any{"id": "join", "type": "join", "config": map[string]any{"policy": "all_success"}})
		edges = append(edges, map[string]any{"id": "join-final", "from_node_id": "join", "to_node_id": "final_review", "route": "success"})
		nodes = append(nodes, workerNode("final_review", workerIDs["final"], 1, "consult", "结合各项复核结果形成综合评审结论。", "join"))
		fields = map[string]any{"summary": ref("final_review")}
	} else {
		nodes = append(nodes, workerNode("review", workerIDs["single"], 1, "consult", "核对资料完整性和关键信息并给出结论。", ""))
	}
	nodes = append(nodes,
		map[string]any{"id": "final_output", "type": "transform", "config": map[string]any{"operation": "object", "fields": fields}, "output": map[string]any{"type": "json", "schema": objectOutput}},
		map[string]any{"id": "deliver", "type": "deliver", "config": map[string]any{"result": ref("final_output")}},
	)
	finalWorkerNode := "review"
	if parallel {
		finalWorkerNode = "final_review"
	}
	edges = append(edges,
		map[string]any{"id": "worker-output", "from_node_id": finalWorkerNode, "to_node_id": "final_output", "route": "success"},
		map[string]any{"id": "output-deliver", "from_node_id": "final_output", "to_node_id": "deliver", "route": "success"},
	)
	graph := map[string]any{
		"schema_version": 1, "entry_node_id": entry,
		"input_contract":  map[string]any{"type": "json", "schema": inputSchema},
		"output_contract": map[string]any{"type": "json", "schema": outputSchema},
		"nodes":           nodes, "edges": edges,
	}
	raw, _ := json.Marshal(graph)
	return raw
}
