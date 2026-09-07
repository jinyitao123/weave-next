package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
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
	calls   atomic.Int64
	exhaust bool
}

func (m *memberTestModel) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	m.calls.Add(1)
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
	file            string
	calls           atomic.Int64
	failAfterEffect bool
	artifactContent string
}

func (*memberTestTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "append", Description: "Append a value", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`), ReadOnly: true}}, nil
}
func (tools *memberTestTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
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
	if _, err := runner.Run(t.Context(), next); !errors.Is(err, ErrMemberOutcomeUnknown) {
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
