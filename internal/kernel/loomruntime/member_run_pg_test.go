package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

type memberPGHarness struct {
	pool    *pgxpool.Pool
	records *memberFaultStore
	model   *memberTestModel
	tools   *memberTestTools
	request MemberRequest
}

func newMemberPGHarness(t *testing.T) *memberPGHarness {
	t.Helper()
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
	pool = production
	if _, err := pool.Exec(t.Context(), `CREATE TABLE member_test_parent(epoch bigint NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO member_test_parent VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	return memberPGFixture(t, pool, t.TempDir())
}

func memberPGFixture(t *testing.T, pool *pgxpool.Pool, directory string) *memberPGHarness {
	t.Helper()
	h := &memberPGHarness{pool: pool, records: &memberFaultStore{PGExt: storeext.New(pool)}, model: &memberTestModel{}, tools: &memberTestTools{file: filepath.Join(directory, "effects.txt")}}
	team, workflow, snapshot, parent := "team", "workflow", "snapshot", "parent"
	version, seq := 1, int64(1)
	attribution, err := NewTerminalAttribution(TerminalAttributionInput{
		Scope: TerminalAttributionFixedWorkflow, WorkspaceID: "workspace", TeamID: &team, WorkflowID: &workflow,
		WorkflowVersion: &version, RunSnapshotID: &snapshot, ParentRunID: &parent, ParentSeq: &seq, AggregationParentRunID: &parent,
	}, &TerminalSnapshotEvidence{WorkspaceID: "workspace", RunID: snapshot, TeamID: team, Mode: "fixed_workflow", WorkflowID: &workflow, WorkflowVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	h.request = MemberRequest{WorkspaceID: "workspace", ParentRunID: parent, RunSnapshotID: snapshot, NodeID: "worker", CallID: "call-0", ParentGeneration: 1,
		ArtifactHash: strings.Repeat("a", 64), Bundle: frozen.FrozenExecutionBundle{FactoryKey: compiler.StandardFrozenToolsKey(), Agent: frozen.FrozenAgentRecord{WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 1, Name: "member"}},
		Input: loom.State{"messages": []contract.Message{{Role: "user", Content: "Write two entries then finish."}}}, Attribution: attribution,
	}
	h.request.ParentGuard = func(ctx context.Context, tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT epoch FROM member_test_parent FOR UPDATE`).Scan(&epoch); err != nil {
			return err
		}
		if epoch != 1 {
			return ErrAttemptLeaseOwnerConflict
		}
		return nil
	}
	h.request.Graph = h.graph()
	return h
}

func (h *memberPGHarness) graph() *loom.Graph {
	opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: h.model, Tools: h.tools}))
	llm := opts.ExecutionLLMWrapper(opts.LLM)
	g := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
	g.AddStep("chat", stdlib.NewToolLoopStep(llm, opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
	return g
}

func (h *memberPGHarness) nextEpoch(t *testing.T) MemberRequest {
	t.Helper()
	next := h.request
	next.ParentGeneration++
	if _, err := h.pool.Exec(t.Context(), `UPDATE member_test_parent SET epoch=$1`, next.ParentGeneration); err != nil {
		t.Fatal(err)
	}
	next.ParentGuard = func(ctx context.Context, tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT epoch FROM member_test_parent FOR UPDATE`).Scan(&epoch); err != nil {
			return err
		}
		if epoch != next.ParentGeneration {
			return ErrAttemptLeaseOwnerConflict
		}
		return nil
	}
	next.Graph = h.graph()
	return next
}

type memberTestModel struct {
	calls               atomic.Int64
	exhaust             bool
	cancelAfterResponse context.CancelFunc
}

func (m *memberTestModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls.Add(1)
	if m.cancelAfterResponse != nil {
		defer m.cancelAfterResponse()
	}
	response := &contract.ChatResponse{Content: "done", Usage: contract.Usage{InputTokens: 3, OutputTokens: 4, CostUSD: .01}}
	for _, msg := range request.Messages {
		if msg.Role == "tool" && !m.exhaust {
			return response, nil
		}
	}
	if m.exhaust && len(request.Tools) == 0 {
		return response, nil
	}
	response.Content = ""
	response.ToolCalls = []contract.ToolCall{{ID: "call-a", Name: "append", Args: `{"value":"a"}`}, {ID: "call-b", Name: "append", Args: `{"value":"b"}`}}
	if m.exhaust {
		for i := range response.ToolCalls {
			response.ToolCalls[i].ID = fmt.Sprintf("round-%d-%d", len(request.Messages), i)
			response.ToolCalls[i].Args = fmt.Sprintf(`{"value":"%d-%d"}`, len(request.Messages), i)
		}
	}
	return response, nil
}
func (*memberTestModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused")
}

type memberTestTools struct {
	file             string
	calls            atomic.Int64
	failAfterEffect  bool
	artifactContent  string
	observeOperation func(context.Context, contract.ToolCall)
}

func (*memberTestTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "append", Description: "Append a value", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ReadOnly: true}}, nil
}
func (tools *memberTestTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if tools.observeOperation != nil {
		tools.observeOperation(ctx, call)
	}
	tools.calls.Add(1)
	file, err := os.OpenFile(tools.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintln(file, call.ID)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if tools.failAfterEffect {
		return nil, errors.New("connection reset after effect")
	}
	content := "written"
	if call.ID == "call-a" && tools.artifactContent != "" {
		content = tools.artifactContent
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: content}, nil
}

type memberFaultStore struct {
	*storeext.PGExt
	afterToolReceipt context.CancelFunc
	failHistory      bool
}
type memberFaultTx struct {
	pgx.Tx
	afterCommit context.CancelFunc
}

func (tx *memberFaultTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if err == nil && tx.afterCommit != nil {
		tx.afterCommit()
	}
	return err
}
func (store *memberFaultStore) BeginTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := store.PGExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	return &memberFaultTx{Tx: tx}, nil
}
func (store *memberFaultStore) PutValueTx(ctx context.Context, tx pgx.Tx, ns, key string, raw []byte) error {
	if store.failHistory && strings.HasPrefix(ns, "checkpoint:") && strings.Contains(key, "/") {
		return errors.New("injected immutable history save failure")
	}
	if store.afterToolReceipt != nil && strings.HasPrefix(ns, "member-operation:") {
		var op memberOperation
		if json.Unmarshal(raw, &op) == nil && op.Kind == "tool" && len(op.Response) > 0 {
			tx.(*memberFaultTx).afterCommit = store.afterToolReceipt
			store.afterToolReceipt = nil
		}
	}
	return store.PGExt.PutValueTx(ctx, tx, ns, key, raw)
}

func TestMemberResumeAfterReceiptDoesNotRepeatEffectRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	_, err = runner.Run(ctx, h.request)
	if err == nil {
		t.Fatal("expected interruption")
	}
	if h.tools.calls.Load() != 1 || h.model.calls.Load() != 1 {
		t.Fatalf("before restart tools=%d models=%d err=%v", h.tools.calls.Load(), h.model.calls.Load(), err)
	}
	next := h.nextEpoch(t)
	// Fresh runner, freshly compiled graph, new attempt; only PostgreSQL survives.
	restarted, _ := NewMemberRunner(storeext.New(h.pool))
	result, err := restarted.Run(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != loom.StopCompleted || h.tools.calls.Load() != 2 || h.model.calls.Load() != 2 {
		t.Fatalf("result=%+v tools=%d models=%d", result, h.tools.calls.Load(), h.model.calls.Load())
	}
	effects, err := os.ReadFile(h.tools.file)
	if err != nil || string(effects) != "call-a\ncall-b\n" {
		t.Fatalf("effects=%q err=%v", effects, err)
	}
	usage, err := LoadUsageAccumulator(result.State)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Totals().InputTokens != 6 || usage.Totals().ToolCalls != 2 {
		t.Fatalf("usage=%+v", usage.Totals())
	}
	again, err := restarted.Run(t.Context(), next)
	if err != nil || again.RunID != result.RunID || h.tools.calls.Load() != 2 || h.model.calls.Load() != 2 {
		t.Fatalf("completed reuse=%+v err=%v", again, err)
	}
	changed := next
	changed.Input = loom.State{"messages": []contract.Message{{Role: "user", Content: "different"}}}
	if _, err := restarted.Run(t.Context(), changed); !errors.Is(err, ErrMemberIdentityConflict) {
		t.Fatalf("identity=%v", err)
	}
	var generation, seq int64
	if err := h.pool.QueryRow(t.Context(), `SELECT attempt_generation FROM weave_run_attempt_leases WHERE workspace_id=$1 AND run_id=$2`, next.WorkspaceID, result.RunID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("attempt generation=%d", generation)
	}
	if err := h.pool.QueryRow(t.Context(), `SELECT checkpoint_seq FROM weave_workflow_member_runs WHERE workspace_id=$1 AND member_run_id=$2`, next.WorkspaceID, result.RunID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	var historyCount int64
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store WHERE namespace=$1 AND key LIKE $2`, "checkpoint:"+next.Graph.Name, result.RunID+"/%").Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != seq {
		t.Fatalf("atomic history=%d latest seq=%d", historyCount, seq)
	}
}

func TestMemberCancellationBeforeResponseTransactionDoesNotPanicRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.model.cancelAfterResponse = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("cancelled response transaction succeeded")
	}
	if h.model.calls.Load() != 1 || h.tools.calls.Load() != 0 {
		t.Fatalf("unexpected effects models=%d tools=%d", h.model.calls.Load(), h.tools.calls.Load())
	}
	// The pre-call intent remains durable, but a cancelled owner cannot append
	// the response. A failed second BeginTx must not replace a deferred tx receiver.
	var pending int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store WHERE namespace='member-operation:workspace' AND NOT (convert_from(value,'UTF8')::jsonb ? 'response')`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending intent=%d err=%v", pending, err)
	}
}

func TestMemberUnknownToolEffectIsNotReplayedRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.tools.failAfterEffect = true
	runner, _ := NewMemberRunner(h.records)
	if _, err := runner.Run(t.Context(), h.request); err == nil {
		t.Fatal("unknown tool effect accepted")
	}
	if h.tools.calls.Load() != 1 || h.model.calls.Load() != 1 {
		t.Fatal("execution continued after transport lost the effect receipt")
	}
	next := h.nextEpoch(t)
	if _, err := runner.Run(t.Context(), next); !errors.Is(err, ErrMemberOutcomeUnknown) || !errors.Is(err, stdlib.ErrJournalOutcomeUnknown) {
		t.Fatalf("expected reconciliation, got %v", err)
	}
	if h.tools.calls.Load() != 1 || h.model.calls.Load() != 1 {
		t.Fatal("unknown effect was automatically replayed")
	}
}

func TestMemberAdmissionAndCheckpointFenceRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	runner, _ := NewMemberRunner(h.records)
	old, _, err := runner.admit(t.Context(), h.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.admit(t.Context(), h.request); !errors.Is(err, ErrMemberBusy) {
		t.Fatalf("duplicate admission=%v", err)
	}
	next := h.nextEpoch(t)
	newer, _, err := runner.admit(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if old.runID != newer.runID || newer.lease.AttemptGeneration != 2 {
		t.Fatal("takeover changed logical identity")
	}
	if err := old.check(t.Context()); !errors.Is(err, ErrAttemptLeaseOwnerConflict) {
		t.Fatalf("old owner accepted: %v", err)
	}
	if err := newer.check(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMemberAtomicHistoryFailureStopsBeforeEffectsRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.records.failHistory = true
	runner, _ := NewMemberRunner(h.records)
	if _, err := runner.Run(t.Context(), h.request); err == nil {
		t.Fatal("failed durable boundary accepted")
	}
	if h.tools.calls.Load() != 0 || h.model.calls.Load() != 0 {
		t.Fatal("effects ran after checkpoint failure")
	}
	var seq int64
	if err := h.pool.QueryRow(t.Context(), `SELECT checkpoint_seq FROM weave_workflow_member_runs`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("partial index saved: %d", seq)
	}
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM loom_store WHERE namespace LIKE 'checkpoint:%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("latest survived failed history transaction")
	}
}

func TestMemberResumePreservesOriginalToolLoopLimitRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.model.exhaust = true
	runner, _ := NewMemberRunner(h.records)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("expected interruption")
	}
	next := h.nextEpoch(t)
	if _, err := runner.Run(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if h.tools.calls.Load() != 6 || h.model.calls.Load() != 4 {
		t.Fatalf("loop budget reset: tools=%d model=%d", h.tools.calls.Load(), h.model.calls.Load())
	}
}

func TestMemberArtifactExportSurvivesReceiptRecoveryRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	h.tools.artifactContent = `{"weave_member_artifacts_v1":[{"path":"model/result.json","content_type":"application/json","content":"{\"value\":42}"}]}`
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	if _, err := runner.Run(ctx, h.request); err == nil {
		t.Fatal("interruption did not stop member")
	}
	h.tools.artifactContent = "must not replace acknowledged export"
	resumed, err := runner.Run(t.Context(), h.nextEpoch(t))
	if err != nil {
		t.Fatal(err)
	}
	files, err := fileartifact.MemberFiles(resumed.State)
	if err != nil || len(files) != 1 || files[0].Content != `{"value":42}` || h.tools.calls.Load() != 2 {
		t.Fatalf("files=%+v tools=%d err=%v", files, h.tools.calls.Load(), err)
	}
	cached, err := runner.Run(t.Context(), h.nextEpoch(t))
	if err != nil {
		t.Fatal(err)
	}
	files, err = fileartifact.MemberFiles(cached.State)
	if err != nil || len(files) != 1 || h.tools.calls.Load() != 2 {
		t.Fatal("completed receipt did not retain files")
	}
}

func TestMemberDurableToolSlotsSurviveRecoveryRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	var slots []string
	h.tools.observeOperation = func(ctx context.Context, _ contract.ToolCall) {
		slot := execution.OperationID(ctx)
		if slot == "" {
			t.Fatal("external effect had no durable operation identity")
		}
		slots = append(slots, slot)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.records.afterToolReceipt = cancel
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Run(ctx, h.request); err == nil {
		t.Fatal("expected interruption after durable receipt")
	}
	if len(slots) != 1 {
		t.Fatalf("effects before recovery: %v", slots)
	}
	firstSlot := slots[0]
	next := h.nextEpoch(t)
	restarted, _ := NewMemberRunner(storeext.New(h.pool))
	result, err := restarted.Run(t.Context(), next)
	if err != nil || result.StopReason != loom.StopCompleted {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	if len(slots) != 2 || slots[0] != firstSlot || slots[0] == slots[1] {
		t.Fatalf("each intentional tool position needs its own identity, cached first effect must not repeat: %v", slots)
	}
	rows, err := h.pool.Query(t.Context(), `SELECT key FROM loom_store WHERE namespace=$1 AND key=$2`, "member-operation:workspace", firstSlot)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("effect identity does not match its persisted journal slot")
	}
}

func protocolProbeGraph(model contract.LLM, tools contract.ToolDispatcher) *loom.Graph {
	opts := InstallFrozenMemberJournal(InstallFrozenUsageTracking(compiler.FrozenBuildOpts{LLM: model, Tools: tools}))
	graph := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	graph.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
	graph.AddStep("chat", stdlib.NewToolLoopStep(opts.ExecutionLLMWrapper(opts.LLM), opts.Tools, stdlib.ToolLoopOpts{Model: "fixture", MaxIterations: 3}), loom.End())
	return graph
}

func protocolProbeFixture(t *testing.T, max int) (*memberPGHarness, *MemberRunner, *ModelProtocolProbePolicy) {
	t.Helper()
	h := newMemberPGHarness(t)
	runner, err := NewMemberRunner(h.records)
	if err != nil {
		t.Fatal(err)
	}
	policy := &ModelProtocolProbePolicy{WorkspaceID: "workspace", WorkflowID: "workflow", ExpiresAt: time.Now().Add(time.Hour), MaxModelCalls: max}
	runner.ProtocolProbe = policy
	return h, runner, policy
}

func TestProtocolProbeProviderJournalNodeIdentityAndReplayRealPG(t *testing.T) {
	h, runner, _ := protocolProbeFixture(t, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []contract.Message `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid fixture request")
			return
		}
		value := "A"
		hasTool := false
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "sample-B") {
				value = "B"
			}
			hasTool = hasTool || message.Role == "tool"
		}
		message := map[string]any{"content": "private-response-marker", "reasoning_content": "private-reasoning-marker"}
		finish := "stop"
		if !hasTool {
			finish = "tool_calls"
			message["tool_calls"] = []any{map[string]any{"id": "same-call", "type": "function", "function": map[string]any{"name": "append", "arguments": `{ "value" : "` + value + `" }`}}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": message}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	defer server.Close()
	client := llmrouter.NewProviderClient(llmrouter.ProviderConfig{BaseURL: server.URL, APIKey: "fixture-private-key-marker", Models: []string{"fixture"}})
	var requests []MemberRequest
	var samples int
	for _, node := range []string{"A", "B"} {
		request := h.request
		request.NodeID, request.CallID = "node-"+node, "node-call-"+node
		request.Input = loom.State{"messages": []contract.Message{{Role: "user", Content: "sample-" + node + " private-request-marker"}}}
		request.Graph = protocolProbeGraph(client, h.tools)
		if result, err := runner.Run(t.Context(), request); err != nil || result.StopReason != loom.StopCompleted {
			t.Fatal("probed member did not retain its ordinary completion")
		}
		requests = append(requests, request)
		memberID := MemberRunID(request.WorkspaceID, request.ParentRunID, request.RunSnapshotID, request.NodeID, request.CallID)
		entries, err := ReadMemberJournal(t.Context(), h.pool, "workspace", memberID)
		if err != nil {
			t.Fatal(err)
		}
		var emitted *ModelProtocolProbeSample
		for _, entry := range entries {
			if hash, err := frozen.HashCanonicalJSON(entry.Input); err != nil || hash != entry.InputHash {
				t.Fatal("probe changed original operation input hash")
			}
			if entry.Kind == "model" {
				if len(entry.ProtocolProbe) != 1 {
					t.Fatal("model attempt did not retain exactly one sample")
				}
				sample := entry.ProtocolProbe[0]
				samples++
				if sample.State != "observed" || len(sample.Observed) != 1 || !sample.Observed[0].RequestSent || !sample.Observed[0].Complete || sample.NormalizedToolCount == nil {
					t.Fatal("complete physical/model boundary was not retained")
				}
				raw, _ := json.Marshal(sample)
				for _, marker := range []string{"private-request-marker", "private-response-marker", "private-reasoning-marker", "fixture-private-key-marker"} {
					if bytes.Contains(raw, []byte(marker)) {
						t.Fatal("private data entered journal probe metadata")
					}
				}
				if *sample.NormalizedToolCount == 1 {
					emitted = &sample
				}
			}
			if entry.Kind == "tool" {
				var call contract.ToolCall
				if json.Unmarshal(entry.Input, &call) != nil || call.ID != "same-call" || call.Args != `{"value":"`+node+`"}` || emitted == nil {
					t.Fatal("same tool call ID was attributed to the wrong node")
				}
				if emitted.NormalizedTools[0].CallIDSHA256 != protocolDigest([]byte(call.ID)) || emitted.NormalizedTools[0].CanonicalArgumentsSHA256 != protocolDigest([]byte(call.Args)) {
					t.Fatal("standard/tool journal identity linkage changed")
				}
			}
		}
	}
	if samples != 4 {
		t.Fatal("unexpected sampled model attempt count")
	}
	before, err := ReadMemberJournal(t.Context(), h.pool, "workspace", MemberRunID("workspace", "parent", "snapshot", "node-A", "node-call-A"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewMemberRunner(storeext.New(h.pool))
	restarted.ProtocolProbe = runner.ProtocolProbe
	if _, err := restarted.Run(t.Context(), requests[0]); err != nil {
		t.Fatal(err)
	}
	after, _ := ReadMemberJournal(t.Context(), h.pool, "workspace", MemberRunID("workspace", "parent", "snapshot", "node-A", "node-call-A"))
	beforeBytes, _ := json.Marshal(before)
	afterBytes, _ := json.Marshal(after)
	if !bytes.Equal(beforeBytes, afterBytes) || h.tools.calls.Load() != 2 {
		t.Fatal("replay generated probe samples or another tool effect")
	}
	// A fresh node remains executable after the cross-node/restart quota is full.
	next := requests[0]
	next.NodeID, next.CallID = "node-C", "node-call-C"
	if _, err := restarted.Run(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	entries, _ := ReadMemberJournal(t.Context(), h.pool, "workspace", MemberRunID("workspace", "parent", "snapshot", "node-C", "node-call-C"))
	for _, entry := range entries {
		if len(entry.ProtocolProbe) != 0 {
			t.Fatal("restart reset probe quota")
		}
	}
}

func TestProtocolProbeDisabledExpiredWrongScopeAndUnavailableRealPG(t *testing.T) {
	for _, mode := range []string{"disabled", "expired", "wrong-run", "wrong-workflow", "diagnostic-read-failure", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, policy := protocolProbeFixture(t, 1)
			switch mode {
			case "disabled":
				runner.ProtocolProbe = nil
			case "expired":
				policy.ExpiresAt = time.Now().Add(-time.Second)
			case "wrong-run":
				policy.RunID = "different-run"
			case "wrong-workflow":
				policy.WorkflowID = "different-workflow"
			case "diagnostic-read-failure":
				if _, err := h.pool.Exec(t.Context(), `INSERT INTO loom_store(namespace,key,value) VALUES('member-operation:workspace','invalid-probe-record',convert_to('{"kind":"model","protocol_probe":{}}','UTF8'))`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := runner.Run(t.Context(), h.request); err != nil {
				t.Fatal(err)
			}
			entries, err := ReadMemberJournal(t.Context(), h.pool, "workspace", MemberRunID("workspace", "parent", "snapshot", "worker", "call-0"))
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, entry := range entries {
				for _, sample := range entry.ProtocolProbe {
					count++
					if mode != "unavailable" || sample.State != "unavailable" || len(sample.Observed) != 0 || sample.NormalizedToolCount == nil || *sample.NormalizedToolCount != 2 {
						t.Fatal("unobserved provider boundary was called complete or zero")
					}
				}
			}
			expected := 0
			if mode == "unavailable" {
				expected = 1
			}
			if count != expected || h.model.calls.Load() != 2 || h.tools.calls.Load() != 2 {
				t.Fatal("probe policy changed ordinary model/tool execution")
			}
		})
	}
}

func TestProtocolProbePhysicalFailureAndCancelledJournalPreservesErrorRealPG(t *testing.T) {
	for _, mode := range []string{"http-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, _ := protocolProbeFixture(t, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "cancel" {
					cancel()
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.WriteHeader(http.StatusBadGateway)
				io.WriteString(w, "fixture-provider-failure-marker")
			}))
			defer server.Close()
			client := llmrouter.NewProviderClient(llmrouter.ProviderConfig{BaseURL: server.URL, APIKey: "fixture-private-key-marker", Models: []string{"fixture"}})
			h.request.Graph = protocolProbeGraph(client, h.tools)
			_, err := runner.Run(ctx, h.request)
			if err == nil || mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "http-error" && !strings.Contains(err.Error(), "fixture-provider-failure-marker") {
				t.Fatal("probe swallowed or rewrote provider error")
			}
			entries, readErr := ReadMemberJournal(t.Context(), h.pool, "workspace", MemberRunID("workspace", "parent", "snapshot", "worker", "call-0"))
			if readErr != nil || len(entries) != 1 || len(entries[0].Response) != 0 || len(entries[0].ProtocolProbe) != 1 {
				t.Fatal("failure probe changed original response/slot or disappeared")
			}
			sample := entries[0].ProtocolProbe[0]
			if sample.State != "incomplete" || !sample.NormalizedError || sample.NormalizedToolCount != nil || len(sample.Observed) != 1 || !sample.Observed[0].RequestSent {
				t.Fatal("failed physical attempt was not retained as incomplete")
			}
		})
	}
}

func TestProtocolProbeLateGenerationObservationSurvivesCurrentReceiptRealPG(t *testing.T) {
	h, runner, policy := protocolProbeFixture(t, 2)
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	var firstRelease, secondRelease sync.Once
	var calls atomic.Int64
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	secondCtx, cancelSecond := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		} else {
			close(secondStarted)
			<-releaseSecond
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"done"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	t.Cleanup(func() {
		cancelFirst()
		cancelSecond()
		firstRelease.Do(func() { close(releaseFirst) })
		secondRelease.Do(func() { close(releaseSecond) })
		server.Close()
	})
	client := llmrouter.NewProviderClient(llmrouter.ProviderConfig{BaseURL: server.URL, APIKey: "fixture", Models: []string{"fixture"}})
	h.request.Graph = protocolProbeGraph(client, h.tools)
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := runner.Run(firstCtx, h.request); firstDone <- err }()
	wait := func(signal <-chan struct{}) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal("controlled generation did not reach its model boundary")
		}
	}
	wait(firstStarted)
	next := h.nextEpoch(t)
	next.Graph = protocolProbeGraph(client, h.tools)
	current, _ := NewMemberRunner(storeext.New(h.pool))
	current.ProtocolProbe = policy
	go func() { _, err := current.Run(secondCtx, next); secondDone <- err }()
	wait(secondStarted)
	// Generation two has now loaded gen one's pending sample and reserved its own.
	cancelFirst()
	firstRelease.Do(func() { close(releaseFirst) })
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("stale generation committed execution")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale generation did not stop")
	}
	memberID := MemberRunID("workspace", "parent", "snapshot", "worker", "call-0")
	before, err := ReadMemberJournal(t.Context(), h.pool, "workspace", memberID)
	if err != nil || len(before) != 1 || len(before[0].ProtocolProbe) != 2 || len(before[0].ProtocolProbe[0].Observed) != 1 {
		t.Fatal("late old observation was not saved before current response")
	}
	secondRelease.Do(func() { close(releaseSecond) })
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("current generation did not finish")
	}
	after, err := ReadMemberJournal(t.Context(), h.pool, "workspace", memberID)
	if err != nil || len(after) != 1 || len(after[0].Response) == 0 || len(after[0].ProtocolProbe) != 2 || len(after[0].ProtocolProbe[0].Observed) != 1 || after[0].ProtocolProbe[1].State != "observed" {
		t.Fatal("current journal receipt erased older attempt's observed failure facts")
	}
	if h.tools.calls.Load() != 0 {
		t.Fatal("diagnostic recovery caused a tool effect")
	}
}
