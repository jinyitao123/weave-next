package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type schemaTestLLM struct {
	responses []contract.ChatResponse
	requests  []contract.ChatRequest
}

func (l *schemaTestLLM) Chat(_ context.Context, r contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, r)
	if len(l.responses) == 0 {
		return nil, errors.New("unexpected model call")
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	return &response, nil
}
func (*schemaTestLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

var outputTestSchema = json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}`)

func schemaInput() loom.State {
	return loom.State{"messages": []contract.Message{{Role: "user", Content: "Produce the answer."}}}
}

func TestNodeOutputVerifierHardStopKeepsBoundedEvidence(t *testing.T) {
	bad := `{"answer":"private material","extra":"do not log this value"}`
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: bad}, {Content: bad}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 2})
	result, err := step(WithNodeOutputSchema(t.Context(), outputTestSchema), schemaInput())
	var violation *NodeOutputViolation
	if !errors.As(err, &violation) || !errors.Is(err, stdlib.ErrCompletionUnverified) {
		t.Fatalf("lost final verification failure: %v", err)
	}
	if len(llm.requests) != 2 || result["output"] != nil {
		t.Fatalf("hard stop published output or made extra calls: %#v", result)
	}
	encoded, _ := json.Marshal(violation)
	if violation.Path != "/extra" || len(violation.OutputSHA256) != 64 || violation.OutputBytes != len(bad) {
		t.Fatalf("diagnostic=%s", encoded)
	}
	if strings.Contains(string(encoded), "private material") || strings.Contains(err.Error(), "do not log") {
		t.Fatal("diagnostic leaked model content")
	}
}

func TestNodeOutputVerifierPreservesControlledRecoveryAndSchemaIdentity(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: `{"answer":"draft","extra":true}`}, {Content: `{"answer":"accepted"}`}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2}})
	graph := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", step, loom.End())
	store := loom.NewMemStore()
	ctx := WithNodeOutputSchema(t.Context(), outputTestSchema)
	paused, err := graph.Run(ctx, schemaInput(), store)
	if err != nil || !paused.Yielded {
		t.Fatalf("expected bounded pause: %#v %v", paused, err)
	}
	outcome, present, err := stdlib.ReadToolLoopOutcome(paused.State)
	if err != nil || !present || outcome.TotalRoundsUsed != 1 {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	raw, _ := json.Marshal(paused.State["__checkpoint_seq"])
	var seq int64
	_ = json.Unmarshal(raw, &seq)
	delta, err := stdlib.PrepareToolLoopResume(paused.State, stdlib.ToolLoopResumeGrant{ID: "resume", ExpectedRunID: paused.RunID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: paused.State["__yield_token"].(string), ExpectedSlice: outcome.Slice, AuthorizedTotalRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	// A different schema cannot consume a checkpoint under the same node.
	changed := WithNodeOutputSchema(t.Context(), json.RawMessage(`{"type":"object"}`))
	if _, err := graph.Resume(changed, paused.RunID, delta, store); err == nil {
		t.Fatal("changed schema resumed old policy")
	}
	if len(llm.requests) != 1 {
		t.Fatal("changed schema reached model")
	}
	completed, err := graph.Resume(ctx, paused.RunID, delta, store)
	if err != nil || completed.State["output"] != `{"answer":"accepted"}` {
		t.Fatalf("resume=%#v %v", completed, err)
	}
	if len(llm.requests) != 2 || !strings.Contains(llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content, "/extra") {
		t.Fatal("resume lost correction observation")
	}
	// Build a real previous-policy pause with the same schema, system text and
	// round limits. Only the old verifier identity differs from this compiler.
	digest := sha256.Sum256(outputTestSchema)
	oldLLM := &schemaTestLLM{responses: []contract.ChatResponse{{Content: `{"answer":"draft","extra":true}`}}}
	oldStep := stdlib.NewToolLoopStep(oldLLM, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 1, Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 2},
		SystemPrompt: llm.requests[0].Messages[0].Content, OutputSchema: &outputTestSchema,
		CompletionVerifierID: stdlib.BareToolProtocolCompletionPolicyID + ":weave.node-output.v1:" + hex.EncodeToString(digest[:]),
		CompletionVerifier: stdlib.CompletionVerifierFunc(func(context.Context, stdlib.CompletionCandidate) (stdlib.CompletionDecision, error) {
			return stdlib.CompletionDecision{Feedback: "correct the final JSON"}, nil
		}),
	})
	oldGraph := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	oldGraph.AddStep("chat", oldStep, loom.End())
	oldStore := loom.NewMemStore()
	oldPause, err := oldGraph.Run(t.Context(), schemaInput(), oldStore)
	if err != nil || !oldPause.Yielded {
		t.Fatalf("old policy fixture did not pause: %v", err)
	}
	oldOutcome, _, err := stdlib.ReadToolLoopOutcome(oldPause.State)
	if err != nil {
		t.Fatal(err)
	}
	oldSequence, _ := json.Marshal(oldPause.State["__checkpoint_seq"])
	_ = json.Unmarshal(oldSequence, &seq)
	oldDelta, err := stdlib.PrepareToolLoopResume(oldPause.State, stdlib.ToolLoopResumeGrant{ID: "old-resume", ExpectedRunID: oldPause.RunID, ExpectedCheckpointSeq: seq, ExpectedYieldToken: oldPause.State["__yield_token"].(string), ExpectedSlice: oldOutcome.Slice, AuthorizedTotalRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Resume(ctx, oldPause.RunID, oldDelta, oldStore); err == nil || !strings.Contains(err.Error(), "policy changed") {
		t.Fatalf("old policy was silently upgraded: %v", err)
	}
	if len(llm.requests) != 2 || len(oldLLM.requests) != 1 {
		t.Fatal("old policy mismatch reached the model")
	}
}

func TestNodeOutputDiagnosticPathCannotCarryUnboundedModelText(t *testing.T) {
	path := "/" + strings.Repeat("合同", 2000) + "/line\nsecret"
	got := diagnosticFieldPath(path)
	if len(got) > 195 || !utf8.ValidString(got) || strings.Contains(got, "secret") || strings.Contains(got, "\n") {
		t.Fatalf("unsafe path=%q", got)
	}
}

type schemaEchoLLM struct{}

func (schemaEchoLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if request.Schema == nil {
		return nil, errors.New("missing invocation schema")
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(*request.Schema, &schema); err != nil || len(schema.Required) != 1 {
		return nil, errors.New("unexpected schema")
	}
	content, _ := json.Marshal(map[string]string{schema.Required[0]: "done"})
	return &contract.ChatResponse{Content: string(content)}, nil
}
func (schemaEchoLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

func TestNodeOutputBindingsAreIsolatedForConcurrentSharedSteps(t *testing.T) {
	step := nodeToolLoopStep(schemaEchoLLM{}, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 2})
	var group sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			schema, _ := json.Marshal(map[string]any{"type": "object", "required": []string{name}, "properties": map[string]any{name: map[string]any{"type": "string"}}, "additionalProperties": false})
			ctx := WithNodeOutputSchema(t.Context(), schema)
			// The binding must own the frozen bytes independently of its caller.
			for index := range schema {
				schema[index] = ' '
			}
			result, err := step(ctx, schemaInput())
			if err != nil || result["output"] != `{"`+name+`":"done"}` {
				t.Errorf("node %s: output=%#v err=%v", name, result, err)
			}
		}(name)
	}
	group.Wait()
}

const protocolCandidate = "I will inspect the provided material.\n\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read_frozen_material\">\n<｜｜DSML｜｜ parameter name=\"materialId\" string=\"true\">fixture-file</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"

func TestNodeOutputProtocolGuardCoversTextAndComposesWithJSON(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[structured], func(t *testing.T) {
			responses := []contract.ChatResponse{{Content: protocolCandidate}, {Content: "Verified inspection result."}}
			var schema json.RawMessage
			if structured {
				schema = outputTestSchema
				responses = []contract.ChatResponse{{Content: protocolCandidate}, {Content: `{"answer":"draft","extra":true}`}, {Content: `{"answer":"accepted"}`}}
			}
			llm := &schemaTestLLM{responses: responses}
			step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 3})
			result, err := step(WithNodeOutputSchema(t.Context(), schema), schemaInput())
			if err != nil || strings.Contains(result["output"].(string), "DSML") {
				t.Fatalf("protocol text became final result: %#v %v", result, err)
			}
			if len(llm.requests) != len(responses) {
				t.Fatalf("calls=%d want=%d", len(llm.requests), len(responses))
			}
			if !structured && llm.requests[1].Schema != nil {
				t.Fatal("text node acquired a JSON schema")
			}
			if structured && !strings.Contains(llm.requests[2].Messages[len(llm.requests[2].Messages)-1].Content, "/extra") {
				t.Fatal("protocol guard replaced exact JSON verifier")
			}
		})
	}
}

func TestNodeOutputProtocolGuardIsScopedAndCannotSynthesizeCalls(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{Content: protocolCandidate}, {Content: protocolCandidate}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 1})
	// Ordinary agent graphs retain their previous opt-out behavior.
	result, err := step(t.Context(), schemaInput())
	if err != nil || result["output"] != protocolCandidate {
		t.Fatalf("unscoped behavior changed: %#v %v", result, err)
	}
	result, err = step(WithNodeOutputSchema(t.Context(), nil), schemaInput())
	var violation *NodeOutputViolation
	if !errors.As(err, &violation) || violation.Code != "unexecuted_tool_protocol" || result["output"] != nil {
		t.Fatalf("scoped protocol was accepted or executed: %#v %v", result, err)
	}
	if !errors.Is(err, stdlib.ErrCompletionUnverified) || len(llm.requests) != 2 {
		t.Fatal("protocol guard exceeded existing budget")
	}
}

type workbenchActionDispatcher struct{ calls *atomic.Int32 }

func (*workbenchActionDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: "submit_material", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (d *workbenchActionDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	d.calls.Add(1)
	return &contract.ToolResult{CallID: call.ID, Content: `{"status":"succeeded"}`}, nil
}

func TestWorkbenchResultCorrectionDoesNotReplayToolsAcrossControlledResume(t *testing.T) {
	frozenSchema := machine.WorkbenchResultSchemaV1()
	invalid := workbenchOutput(t, strings.Repeat("界", 1033))
	validSummary := strings.Repeat("中", 998) + "😀🙂"
	if got := utf8.RuneCountInString(validSummary); got != 1000 {
		t.Fatalf("valid fixture code points=%d", got)
	}
	valid := workbenchOutput(t, validSummary)
	llm := &schemaTestLLM{responses: []contract.ChatResponse{
		{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-1", Name: "submit_material", Args: `{}`}}},
		{StopReason: "stop", Content: invalid},
		{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-2", Name: "submit_material", Args: `{}`}}},
		{StopReason: "stop", Content: valid},
	}}
	callCount := &atomic.Int32{}
	tools := &workbenchActionDispatcher{calls: callCount}
	step := nodeToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 1,
		Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 4},
	})
	graph := loom.NewGraph(t.Name(), "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", step, loom.End())
	store := loom.NewMemStore()
	checks := 0
	check := func(context.Context, string) (bool, string, error) {
		checks++
		return true, "", nil
	}
	ctx := WithNodeCompletionCheck(
		WithWorkbenchResultOutput(WithNodeOutputSchema(t.Context(), frozenSchema)),
		"receipt-policy-v1", check,
	)
	paused, err := graph.Run(ctx, schemaInput(), store)
	if err != nil || !paused.Yielded || tools.calls.Load() != 1 {
		t.Fatalf("initial action/pause: yielded=%v calls=%d err=%v", paused != nil && paused.Yielded, tools.calls.Load(), err)
	}
	providerSchema := machine.WorkbenchResultProviderSchemaV1()
	if len(llm.requests) != 1 || llm.requests[0].Schema == nil || string(*llm.requests[0].Schema) != string(providerSchema) {
		t.Fatal("provider did not receive runtime Workbench bounds")
	}
	if string(frozenSchema) != string(machine.WorkbenchResultSchemaV1()) {
		t.Fatal("runtime provider injection changed the frozen schema")
	}

	paused = resumeWorkbenchOutput(t, graph, store, ctx, paused, "resume-1")
	if !paused.Yielded || tools.calls.Load() != 1 || len(llm.requests) != 2 {
		t.Fatalf("overlong correction pause: yielded=%v calls=%d requests=%d", paused.Yielded, tools.calls.Load(), len(llm.requests))
	}
	marker, _ := paused.State[workbenchResultCorrectionStateKey].(string)
	if !strings.HasPrefix(marker, "workbench_result_v1:") {
		t.Fatalf("correction state did not survive pause: %q", marker)
	}
	transcript, ok := paused.State["__toolloop_msgs"].([]contract.Message)
	if !ok || !messagesContain(transcript, "observed=1033") {
		t.Fatal("bounded codepoint diagnostic was not returned through Loom correction feedback")
	}

	paused = resumeWorkbenchOutput(t, graph, store, ctx, paused, "resume-2")
	if !paused.Yielded || tools.calls.Load() != 1 || len(llm.requests) != 3 {
		t.Fatalf("tool replay was not blocked within correction budget: yielded=%v calls=%d requests=%d", paused.Yielded, tools.calls.Load(), len(llm.requests))
	}
	paused = resumeWorkbenchOutput(t, graph, store, ctx, paused, "resume-3")
	if paused.Yielded || paused.State["output"] != valid || tools.calls.Load() != 1 || checks != 1 {
		t.Fatalf("final state=%#v action calls=%d checks=%d", paused.State, tools.calls.Load(), checks)
	}
	if len(llm.requests) != 4 {
		t.Fatalf("model rounds=%d want=4", len(llm.requests))
	}
	foundBlocked := false
	for _, message := range llm.requests[3].Messages {
		if message.Role == "tool" && strings.Contains(message.Content, "Tools are unavailable while correcting") {
			foundBlocked = true
		}
	}
	if !foundBlocked {
		t.Fatal("controlled resume lost the blocked replay tool result")
	}
	if paused.State[workbenchResultCorrectionStateKey] != "" {
		t.Fatal("correction-only state was not cleared after valid output")
	}
}

func TestWorkbenchResultCorrectionPersistsThroughPGCheckpointRestart(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	connection, err := url.Parse(databaseURL)
	if err != nil || connection.User == nil {
		t.Skip("Workbench checkpoint regression requires the dedicated local PostgreSQL test cluster")
	}
	_, hasPassword := connection.User.Password()
	host := connection.Hostname()
	if (host != "127.0.0.1" && host != "localhost" && host != "::1") || connection.Port() != "55439" ||
		connection.Path != "/postgres" || connection.User.Username() != "postgres" || hasPassword {
		t.Skip("Workbench checkpoint regression is restricted to the dedicated local PostgreSQL test cluster")
	}

	pool := testutil.PostgresPool(t)
	searchPath := pool.Config().ConnConfig.RuntimeParams["search_path"]
	if searchPath == "" {
		t.Fatal("isolated PostgreSQL schema is unavailable")
	}
	callCount := &atomic.Int32{}
	tools := &workbenchActionDispatcher{calls: callCount}
	checks := 0
	check := func(context.Context, string) (bool, string, error) {
		checks++
		return true, "", nil
	}
	ctx := WithNodeCompletionCheck(
		WithWorkbenchResultOutput(WithNodeOutputSchema(t.Context(), machine.WorkbenchResultSchemaV1())),
		"receipt-policy-v1", check,
	)
	graphName := t.Name()
	newGraph := func(llm *schemaTestLLM) *loom.Graph {
		return newWorkbenchControlledGraph(graphName, llm, tools)
	}

	firstLLM := &schemaTestLLM{responses: []contract.ChatResponse{{
		StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-1", Name: "submit_material", Args: `{}`}},
	}}}
	first := runWorkbenchGraphOnPG(t, searchPath, newGraph(firstLLM), ctx)
	if !first.Yielded || callCount.Load() != 1 {
		t.Fatalf("initial action/pause: yielded=%v calls=%d", first.Yielded, callCount.Load())
	}
	assertWorkbenchPGCheckpoint(t, pool, graphName, first.RunID, false)

	secondLLM := &schemaTestLLM{responses: []contract.ChatResponse{{StopReason: "stop", Content: workbenchOutput(t, strings.Repeat("界", 1033))}}}
	second := resumeWorkbenchGraphOnPG(t, searchPath, newGraph(secondLLM), ctx, first, "resume-1")
	if !second.Yielded || callCount.Load() != 1 || len(secondLLM.requests) != 1 {
		t.Fatalf("correction checkpoint: yielded=%v calls=%d requests=%d", second.Yielded, callCount.Load(), len(secondLLM.requests))
	}
	assertWorkbenchPGCheckpoint(t, pool, graphName, second.RunID, true)

	thirdLLM := &schemaTestLLM{responses: []contract.ChatResponse{{
		StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "write-2", Name: "submit_material", Args: `{}`}},
	}}}
	third := resumeWorkbenchGraphOnPG(t, searchPath, newGraph(thirdLLM), ctx, second, "resume-2")
	if !third.Yielded || callCount.Load() != 1 || len(thirdLLM.requests) != 1 {
		t.Fatalf("restart replay was not blocked: yielded=%v calls=%d requests=%d", third.Yielded, callCount.Load(), len(thirdLLM.requests))
	}
	assertWorkbenchPGCheckpoint(t, pool, graphName, third.RunID, true)

	validSummary := strings.Repeat("中", 998) + "😀🙂"
	if utf8.RuneCountInString(validSummary) != 1000 {
		t.Fatal("final fixture is not exactly 1000 Unicode code points")
	}
	valid := workbenchOutput(t, validSummary)
	fourthLLM := &schemaTestLLM{responses: []contract.ChatResponse{{StopReason: "stop", Content: valid}}}
	fourth := resumeWorkbenchGraphOnPG(t, searchPath, newGraph(fourthLLM), ctx, third, "resume-3")
	if fourth.Yielded || fourth.State["output"] != valid || callCount.Load() != 1 || checks != 1 {
		t.Fatalf("final state=%#v calls=%d checks=%d", fourth.State, callCount.Load(), checks)
	}
	assertWorkbenchPGCheckpoint(t, pool, graphName, fourth.RunID, false)
	if len(fourthLLM.requests) != 1 || !messagesContain(fourthLLM.requests[0].Messages, "Tools are unavailable while correcting") {
		t.Fatal("PostgreSQL restore did not retain the correction tool denial")
	}
}

func newWorkbenchControlledGraph(name string, llm contract.LLM, tools contract.ToolDispatcher) *loom.Graph {
	step := nodeToolLoopStep(llm, tools, stdlib.ToolLoopOpts{
		Model: "test", MaxIterations: 1,
		Control: &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: 4},
	})
	graph := loom.NewGraph(name, "chat", loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	graph.AddStep("chat", step, loom.End())
	return graph
}

func runWorkbenchGraphOnPG(t *testing.T, searchPath string, graph *loom.Graph, ctx context.Context) *loom.RunResult {
	t.Helper()
	store := openWorkbenchPGStore(t, searchPath)
	defer store.Close()
	result, err := graph.Run(ctx, schemaInput(), store)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func resumeWorkbenchGraphOnPG(t *testing.T, searchPath string, graph *loom.Graph, ctx context.Context, previous *loom.RunResult, grantID string) *loom.RunResult {
	t.Helper()
	store := openWorkbenchPGStore(t, searchPath)
	defer store.Close()
	return resumeWorkbenchOutput(t, graph, store, ctx, previous, grantID)
}

func openWorkbenchPGStore(t *testing.T, searchPath string) *pgstore.PGStore {
	t.Helper()
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("could not parse local PostgreSQL test URL")
	}
	query := connection.Query()
	query.Set("search_path", searchPath)
	connection.RawQuery = query.Encode()
	store, err := pgstore.New(connection.String())
	if err != nil {
		t.Fatal("could not open isolated Loom PostgreSQL checkpoint store")
	}
	return store
}

func assertWorkbenchPGCheckpoint(t *testing.T, pool *pgxpool.Pool, graphName, runID string, wantCorrection bool) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, "checkpoint:"+graphName, runID).Scan(&raw); err != nil {
		t.Fatal("checkpoint was not persisted in PostgreSQL")
	}
	var checkpoint struct {
		State map[string]json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal("persisted PostgreSQL checkpoint is invalid")
	}
	var marker string
	if err := json.Unmarshal(checkpoint.State[workbenchResultCorrectionStateKey], &marker); err != nil {
		t.Fatal("persisted correction state is missing")
	}
	if hasCorrection := strings.HasPrefix(marker, "workbench_result_v1:"); hasCorrection != wantCorrection {
		t.Fatalf("persisted correction gate=%v want=%v", hasCorrection, wantCorrection)
	}
}

func TestWorkbenchResultViolationExposesBoundsWithoutOutputText(t *testing.T) {
	private := strings.Repeat("密", 1033)
	bad := workbenchOutput(t, private)
	llm := &schemaTestLLM{responses: []contract.ChatResponse{{StopReason: "stop", Content: bad}, {StopReason: "stop", Content: bad}}}
	step := nodeToolLoopStep(llm, &CompileGuardDispatcher{}, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 2})
	ctx := WithWorkbenchResultOutput(WithNodeOutputSchema(t.Context(), machine.WorkbenchResultSchemaV1()))
	_, err := step(ctx, schemaInput())
	var violation *NodeOutputViolation
	if !errors.As(err, &violation) || !errors.Is(err, stdlib.ErrCompletionUnverified) {
		t.Fatalf("lost Workbench output failure: %v", err)
	}
	if violation.Path != "/summary" || violation.LimitKind != "max" || violation.Limit != 1000 || violation.ObservedLength != 1033 || violation.OutputBytes != len(bad) || len(violation.OutputSHA256) != 64 {
		t.Fatalf("diagnostic=%+v", violation)
	}
	encoded, marshalErr := json.Marshal(violation)
	if marshalErr != nil || strings.Contains(string(encoded), private) || strings.Contains(err.Error(), private) {
		t.Fatalf("diagnostic leaked output text: %s err=%v", encoded, marshalErr)
	}
}

func TestWorkbenchTextCorrectionReopensMissingActionFlow(t *testing.T) {
	llm := &schemaTestLLM{responses: []contract.ChatResponse{
		{StopReason: "stop", Content: workbenchOutput(t, strings.Repeat("界", 1001))},
		{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "premature", Name: "submit_material", Args: `{}`}}},
		{StopReason: "stop", Content: workbenchOutput(t, "已完成材料检查")},
		{StopReason: "tool_calls", ToolCalls: []contract.ToolCall{{ID: "required", Name: "submit_material", Args: `{}`}}},
		{StopReason: "stop", Content: workbenchOutput(t, "已完成材料检查")},
	}}
	callCount := &atomic.Int32{}
	tools := &workbenchActionDispatcher{calls: callCount}
	step := nodeToolLoopStep(llm, tools, stdlib.ToolLoopOpts{Model: "test", MaxIterations: 5})
	check := func(context.Context, string) (bool, string, error) {
		if tools.calls.Load() == 0 {
			return false, "required receipt missing", nil
		}
		return true, "", nil
	}
	ctx := WithNodeCompletionCheck(
		WithWorkbenchResultOutput(WithNodeOutputSchema(t.Context(), machine.WorkbenchResultSchemaV1())),
		"receipt-policy-v1", check,
	)
	result, err := step(ctx, schemaInput())
	if err != nil || result["output"] != workbenchOutput(t, "已完成材料检查") || tools.calls.Load() != 1 {
		t.Fatalf("result=%#v action calls=%d err=%v", result, tools.calls.Load(), err)
	}
	if len(llm.requests) != 5 {
		t.Fatalf("model rounds=%d want=5", len(llm.requests))
	}
	if !strings.Contains(llm.requests[2].Messages[len(llm.requests[2].Messages)-1].Content, "Tools are unavailable while correcting") {
		t.Fatal("initial text correction did not block tool replay")
	}
	if !strings.Contains(llm.requests[3].Messages[len(llm.requests[3].Messages)-1].Content, "required receipt missing") {
		t.Fatal("valid text correction did not resume the existing missing-receipt feedback")
	}
}

func resumeWorkbenchOutput(t *testing.T, graph *loom.Graph, store loom.Store, ctx context.Context, previous *loom.RunResult, grantID string) *loom.RunResult {
	t.Helper()
	outcome, present, err := stdlib.ReadToolLoopOutcome(previous.State)
	if err != nil || !present {
		t.Fatalf("missing controlled outcome: %+v %v", outcome, err)
	}
	seqRaw, err := json.Marshal(previous.State["__checkpoint_seq"])
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := json.Unmarshal(seqRaw, &seq); err != nil {
		t.Fatal(err)
	}
	delta, err := stdlib.PrepareToolLoopResume(previous.State, stdlib.ToolLoopResumeGrant{
		ID: grantID, ExpectedRunID: previous.RunID, ExpectedCheckpointSeq: seq,
		ExpectedYieldToken: previous.State["__yield_token"].(string), ExpectedSlice: outcome.Slice,
		AuthorizedTotalRounds: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := graph.Resume(ctx, previous.RunID, delta, store)
	if err != nil {
		t.Fatal(err)
	}
	return resumed
}

func workbenchOutput(t *testing.T, summary string) string {
	t.Helper()
	encoded, err := json.Marshal(machine.WorkbenchResultV1{
		Disposition: "complete", Summary: summary, MissingItems: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func messagesContain(messages []contract.Message, text string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}
