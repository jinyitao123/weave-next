// Command valueprobe compares three member-execution recovery mechanisms.
// It is an acceptance probe, not a product entry point.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
)

const crashExit = 86

type config struct {
	RunDir    string
	Candidate string
	Task      string
	Scenario  string
	Phase     string
}

type loopState struct {
	Messages      []contract.Message  `json:"messages"`
	Pending       []contract.ToolCall `json:"pending"`
	Final         string              `json:"final"`
	Done          bool                `json:"done"`
	LLMCalls      int                 `json:"llm_calls"`
	InputTokens   int                 `json:"input_tokens"`
	OutputTokens  int                 `json:"output_tokens"`
	ToolResults   int                 `json:"tool_results"`
	LastTool      string              `json:"last_tool"`
	NextOperation int                 `json:"next_operation"`
}

type ledgerEntry struct {
	Result      contract.ToolResult `json:"result"`
	Tool        string              `json:"tool"`
	SemanticKey string              `json:"semantic_key"`
	RecordedAt  time.Time           `json:"recorded_at"`
}

type localInference struct {
	backend engine.Backend
	runDir  string
	seq     int
}

func main() {
	if len(os.Args) != 6 {
		fatalf("usage: valueprobe RUN_DIR A|B|C t1|t2 s0|s1|s2 start|resume")
	}
	cfg := config{RunDir: os.Args[1], Candidate: os.Args[2], Task: os.Args[3], Scenario: os.Args[4], Phase: os.Args[5]}
	if !oneOf(cfg.Candidate, "A", "B", "C") || !oneOf(cfg.Task, "t1", "t2") || !oneOf(cfg.Scenario, "s0", "s1", "s2") || !oneOf(cfg.Phase, "start", "resume") {
		fatalf("invalid arguments: %+v", cfg)
	}
	must(os.MkdirAll(cfg.RunDir, 0700))
	appendEvent(cfg.RunDir, map[string]any{"kind": "process_start", "candidate": cfg.Candidate, "task": cfg.Task, "scenario": cfg.Scenario, "phase": cfg.Phase, "pid": os.Getpid(), "at": time.Now().UTC()})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	llm := newLLM(cfg)
	host := &toolHost{cfg: cfg}
	var state loopState
	var loomRunID string
	var historyCount int
	var err error
	if cfg.Candidate == "C" {
		state, loomRunID, historyCount, err = runLoom(ctx, cfg, llm, host)
	} else {
		state, err = runCustom(ctx, cfg, llm, host)
	}
	if err != nil {
		writeResult(cfg, state, loomRunID, historyCount, false, err)
		fatalf("run failed: %v", err)
	}
	correct, validation := validateWorkspace(ctx, cfg)
	writeResult(cfg, state, loomRunID, historyCount, correct, errors.New(validation))
	appendEvent(cfg.RunDir, map[string]any{"kind": "process_complete", "candidate": cfg.Candidate, "task": cfg.Task, "scenario": cfg.Scenario, "phase": cfg.Phase, "pid": os.Getpid(), "correct": correct, "at": time.Now().UTC()})
	if !correct {
		fatalf("independent validation failed: %s", validation)
	}
}

func newLLM(cfg config) contract.LLM {
	cliPath, err := exec.LookPath("claude")
	must(err)
	backend, err := engine.New(engine.Claude, cliPath)
	must(err)
	agent := &registry.AgentRecord{Name: "loom-value-probe", ID: "loom-value-probe", WorkspaceID: "default", Version: 1, Engine: engine.Claude}
	llm, err := runtimellm.New(&localInference{backend: backend, runDir: cfg.RunDir}, "default", agent, execution.AgentExecutionStamp{AgentID: agent.ID, AgentVersion: 1, ExecutionScope: execution.ScopeTeamWorkerLeaf})
	must(err)
	return llm
}

func (e *localInference) ExecRemote(ctx context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, prompt string, _ []execspec.Attachment) (engine.RunResult, error) {
	e.seq++
	dir := filepath.Join(e.runDir, "inference-workspace")
	must(os.MkdirAll(dir, 0700))
	instruction := "This is a bounded inference-only benchmark. Return the requested protocol response. Do not execute native tools, shell commands, subagents, or read workspace files. Tools in the serialized request are executed by the benchmark host.\n"
	must(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(instruction), 0600))
	version := os.Getenv("LOOM_VALUE_CLI_VERSION")
	if version == "" {
		version = "2.1.245"
	}
	started := time.Now()
	nativeTool := false
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	result, err := e.backend.Run(c, engine.RunSpec{WorkDir: dir, Prompt: prompt, EngineVersion: version, Timeout: 4 * time.Minute, OnPublicEvent: func(v engine.Event, _ bool) {
		if v.Kind == "tool_call" {
			nativeTool = true
			cancel()
		}
	}})
	recordJSON(filepath.Join(e.runDir, fmt.Sprintf("inference-%d-%02d.json", os.Getpid(), e.seq)), map[string]any{
		"output": result.Output, "usage": result.Usage, "reported_models": result.ReportedModels,
		"native_tool_attempt": nativeTool, "status": result.Status, "error": result.Err,
	})
	appendEvent(e.runDir, map[string]any{"kind": "inference_driver_complete", "pid": os.Getpid(), "sequence": e.seq, "elapsed_ms": time.Since(started).Milliseconds(), "native_tool_attempt": nativeTool, "reported_models": result.ReportedModels, "at": time.Now().UTC()})
	if nativeTool {
		return result, errors.New("inference driver attempted native tool execution")
	}
	return result, err
}

func initialState(task string) loopState {
	return loopState{Messages: []contract.Message{{Role: "system", Content: systemPrompt(task)}, {Role: "user", Content: userPrompt(task)}}}
}

func normalizeToolCallIDs(state *loopState, response *contract.ChatResponse) {
	for index := range response.ToolCalls {
		state.NextOperation++
		response.ToolCalls[index].ID = fmt.Sprintf("operation-%04d", state.NextOperation)
	}
}

func systemPrompt(task string) string {
	if task == "t1" {
		return "You are running a controlled member task. Use only the supplied tools. Call read_baseline, calculate_scenarios, and verify_results in that order. Do not claim success before real tool receipts. After verification, return concise Chinese JSON-like text with baseline id, three energies, three cruise times, and PASS status."
	}
	return "You are repairing a frozen digital-engineering fixture derived from a real run. Use only supplied tools. First inspect_workspace, then read_file for model/tests/test_physics.py, verification/run_checks.sh, and task.md. Apply all three repairs using apply_repair with repair_id fix_gamma_expectation, fix_timeout_portability, and fix_workdir. Then call run_acceptance and verify_manifest. Do not claim success before both return PASS. Return a concise Chinese repair report."
}

func userPrompt(task string) string {
	if task == "t1" {
		return "Read the frozen Corona baseline and independently calculate and verify 0.01c, 0.03c, and 0.05c for mass 1e9 kg and distance 4.25 ly."
	}
	return "Diagnose and repair the three registered failures in the fixture, run the bounded acceptance command, and produce verified evidence."
}

func runCustom(ctx context.Context, cfg config, llm contract.LLM, host *toolHost) (loopState, error) {
	state := initialState(cfg.Task)
	if cfg.Candidate == "B" && cfg.Phase == "resume" {
		if err := readJSON(filepath.Join(cfg.RunDir, "b-state.json"), &state); err != nil {
			return state, fmt.Errorf("load B checkpoint: %w", err)
		}
	}
	tools, err := host.ListTools(ctx)
	if err != nil {
		return state, err
	}
	for iteration := 0; iteration < 30 && !state.Done; iteration++ {
		if len(state.Pending) == 0 {
			started := time.Now()
			response, callErr := llm.Chat(ctx, contract.ChatRequest{Messages: state.Messages, Tools: tools, MaxTokens: 1200, Effort: contract.EffortLow})
			if callErr != nil {
				return state, callErr
			}
			state.LLMCalls++
			state.InputTokens += response.Usage.InputTokens
			state.OutputTokens += response.Usage.OutputTokens
			normalizeToolCallIDs(&state, response)
			state.Messages = append(state.Messages, response.AsMessage())
			state.Pending = append([]contract.ToolCall(nil), response.ToolCalls...)
			appendEvent(cfg.RunDir, map[string]any{"kind": "llm_completed", "candidate": cfg.Candidate, "task": cfg.Task, "scenario": cfg.Scenario, "tool_calls": len(response.ToolCalls), "input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "elapsed_ms": time.Since(started).Milliseconds(), "pid": os.Getpid(), "at": time.Now().UTC()})
			if len(response.ToolCalls) == 0 {
				state.Final, state.Done = response.Content, true
			}
			if cfg.Candidate == "B" {
				must(writeAtomicJSON(filepath.Join(cfg.RunDir, "b-state.json"), state))
				appendEvent(cfg.RunDir, map[string]any{"kind": "checkpoint_saved", "candidate": "B", "phase": "after_inference", "pid": os.Getpid(), "at": time.Now().UTC()})
			}
			continue
		}
		call := state.Pending[0]
		result, dispatchErr := host.Dispatch(ctx, call)
		if dispatchErr != nil {
			return state, dispatchErr
		}
		state.Messages = append(state.Messages, result.AsMessage())
		state.Pending = append([]contract.ToolCall(nil), state.Pending[1:]...)
		state.ToolResults++
		state.LastTool = call.Name
		if cfg.Candidate == "B" {
			must(writeAtomicJSON(filepath.Join(cfg.RunDir, "b-state.json"), state))
			appendEvent(cfg.RunDir, map[string]any{"kind": "checkpoint_saved", "candidate": "B", "phase": "after_tool", "tool": call.Name, "pid": os.Getpid(), "at": time.Now().UTC()})
		}
		maybeCrashAfterConfirmed(cfg, call.Name)
	}
	if !state.Done {
		return state, errors.New("tool loop exceeded 30 iterations")
	}
	return state, nil
}

func runLoom(ctx context.Context, cfg config, llm contract.LLM, host *toolHost) (loopState, string, int, error) {
	store, err := pgstore.New(databaseURL())
	if err != nil {
		return loopState{}, "", 0, err
	}
	defer store.Close()
	tools, err := host.ListTools(ctx)
	if err != nil {
		return loopState{}, "", 0, err
	}
	graphName := "loom-value:" + filepath.Base(cfg.RunDir)
	g := loom.NewGraph(graphName, "infer", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(100), loom.WithMaxIterations(80))
	g.AddStep("infer", func(ctx context.Context, raw loom.State) (loom.State, error) {
		state, err := decodeLoopState(raw)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		response, err := llm.Chat(ctx, contract.ChatRequest{Messages: state.Messages, Tools: tools, MaxTokens: 1200, Effort: contract.EffortLow})
		if err != nil {
			return nil, err
		}
		state.LLMCalls++
		state.InputTokens += response.Usage.InputTokens
		state.OutputTokens += response.Usage.OutputTokens
		normalizeToolCallIDs(&state, response)
		state.Messages = append(state.Messages, response.AsMessage())
		state.Pending = append([]contract.ToolCall(nil), response.ToolCalls...)
		if len(response.ToolCalls) == 0 {
			state.Final, state.Done = response.Content, true
		}
		appendEvent(cfg.RunDir, map[string]any{"kind": "llm_completed", "candidate": "C", "task": cfg.Task, "scenario": cfg.Scenario, "tool_calls": len(response.ToolCalls), "input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "elapsed_ms": time.Since(started).Milliseconds(), "pid": os.Getpid(), "at": time.Now().UTC()})
		return encodeLoopState(state, !state.Done), nil
	}, loom.Condition(func(s loom.State) bool { return stateBool(s, "loop_done") }, "", "dispatch"))
	g.AddStep("dispatch", func(ctx context.Context, raw loom.State) (loom.State, error) {
		state, err := decodeLoopState(raw)
		if err != nil {
			return nil, err
		}
		if len(state.Pending) == 0 {
			return nil, errors.New("dispatch step has no pending tool")
		}
		call := state.Pending[0]
		result, err := host.Dispatch(ctx, call)
		if err != nil {
			return nil, err
		}
		state.Messages = append(state.Messages, result.AsMessage())
		state.Pending = append([]contract.ToolCall(nil), state.Pending[1:]...)
		state.ToolResults++
		state.LastTool = call.Name
		return encodeLoopState(state, true), nil
	}, loom.Condition(func(s loom.State) bool { return stateInt(s, "pending_count") > 0 }, "dispatch", "infer"))

	var result *loom.RunResult
	if cfg.Phase == "resume" {
		var identity struct {
			RunID string `json:"run_id"`
		}
		if err := readJSON(filepath.Join(cfg.RunDir, "c-run.json"), &identity); err != nil {
			return loopState{}, "", 0, fmt.Errorf("load C run identity: %w", err)
		}
		result, err = g.Resume(ctx, identity.RunID, loom.State{}, store)
	} else {
		initial := initialState(cfg.Task)
		result, err = g.Run(ctx, encodeLoopState(initial, false), store)
	}
	for err == nil && result != nil && result.StopReason == loom.StopYielded {
		recordJSON(filepath.Join(cfg.RunDir, "c-run.json"), map[string]any{"run_id": result.RunID, "last_step": result.LastStep})
		state, decodeErr := decodeLoopState(result.State)
		if decodeErr != nil {
			return loopState{}, result.RunID, 0, decodeErr
		}
		appendEvent(cfg.RunDir, map[string]any{"kind": "checkpoint_saved", "candidate": "C", "phase": "after_" + result.LastStep, "tool": state.LastTool, "run_id": result.RunID, "pid": os.Getpid(), "at": time.Now().UTC()})
		maybeCrashAfterConfirmed(cfg, state.LastTool)
		result, err = g.Resume(ctx, result.RunID, loom.State{}, store)
	}
	if err != nil {
		return loopState{}, loomRunID(result), 0, err
	}
	if result == nil {
		return loopState{}, "", 0, errors.New("Loom returned nil result")
	}
	state, err := decodeLoopState(result.State)
	if err != nil {
		return loopState{}, result.RunID, 0, err
	}
	history, historyErr := g.History(ctx, store, result.RunID)
	if historyErr != nil {
		return state, result.RunID, 0, historyErr
	}
	return state, result.RunID, len(history), nil
}

func encodeLoopState(state loopState, yield bool) loom.State {
	messages, _ := json.Marshal(state.Messages)
	pending, _ := json.Marshal(state.Pending)
	return loom.State{
		"messages_json": string(messages), "pending_json": string(pending), "final": state.Final,
		"loop_done": state.Done, "llm_calls": state.LLMCalls, "input_tokens": state.InputTokens,
		"output_tokens": state.OutputTokens, "tool_results": state.ToolResults, "last_tool": state.LastTool,
		"next_operation": state.NextOperation, "pending_count": len(state.Pending), "__yield": yield, "__yield_phase": map[bool]string{true: "after_step", false: ""}[yield],
	}
}

func decodeLoopState(raw loom.State) (loopState, error) {
	state := loopState{Final: stateString(raw, "final"), Done: stateBool(raw, "loop_done"), LLMCalls: stateInt(raw, "llm_calls"), InputTokens: stateInt(raw, "input_tokens"), OutputTokens: stateInt(raw, "output_tokens"), ToolResults: stateInt(raw, "tool_results"), LastTool: stateString(raw, "last_tool"), NextOperation: stateInt(raw, "next_operation")}
	if value := stateString(raw, "messages_json"); value != "" {
		if err := json.Unmarshal([]byte(value), &state.Messages); err != nil {
			return state, err
		}
	}
	if value := stateString(raw, "pending_json"); value != "" {
		if err := json.Unmarshal([]byte(value), &state.Pending); err != nil {
			return state, err
		}
	}
	return state, nil
}

type toolHost struct{ cfg config }

func (h *toolHost) ListTools(context.Context) ([]contract.ToolDef, error) {
	object := func(properties string, required ...string) json.RawMessage {
		raw := fmt.Sprintf(`{"type":"object","properties":%s,"required":%s,"additionalProperties":false}`, properties, mustJSON(required))
		return json.RawMessage(raw)
	}
	if h.cfg.Task == "t1" {
		return []contract.ToolDef{
			{Name: "read_baseline", Description: "Read the frozen Corona input and its SHA-256.", InputSchema: object(`{}`), ReadOnly: true},
			{Name: "calculate_scenarios", Description: "Calculate and persist the three approved speed scenarios from the frozen input.", InputSchema: object(`{}`)},
			{Name: "verify_results", Description: "Independently verify persisted scenario results and write a verification receipt.", InputSchema: object(`{}`)},
		}, nil
	}
	return []contract.ToolDef{
		{Name: "inspect_workspace", Description: "List the frozen fixture and run the initial acceptance check.", InputSchema: object(`{}`), ReadOnly: true},
		{Name: "read_file", Description: "Read one fixture file.", InputSchema: object(`{"path":{"type":"string","enum":["model/tests/test_physics.py","verification/run_checks.sh","task.md"]}}`, "path"), ReadOnly: true},
		{Name: "apply_repair", Description: "Apply one registered repair by id.", InputSchema: object(`{"repair_id":{"type":"string","enum":["fix_gamma_expectation","fix_timeout_portability","fix_workdir"]}}`, "repair_id")},
		{Name: "run_acceptance", Description: "Run the bounded fixture acceptance script and return its real exit result.", InputSchema: object(`{}`), ReadOnly: true},
		{Name: "verify_manifest", Description: "Verify all three repairs and acceptance, then persist a machine receipt.", InputSchema: object(`{}`)},
	}, nil
}

func (h *toolHost) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	ledger := map[string]ledgerEntry{}
	ledgerPath := filepath.Join(h.cfg.RunDir, "tool-ledger.json")
	if err := readJSONIfExists(ledgerPath, &ledger); err != nil {
		return nil, err
	}
	opKey := call.ID
	if h.cfg.Candidate == "A" {
		opKey = strconv.Itoa(os.Getpid()) + ":" + call.ID
	}
	if existing, ok := ledger[opKey]; ok {
		result := existing.Result
		result.CallID = call.ID
		appendEvent(h.cfg.RunDir, map[string]any{"kind": "tool_dispatch", "candidate": h.cfg.Candidate, "task": h.cfg.Task, "scenario": h.cfg.Scenario, "tool": call.Name, "call_id": call.ID, "operation_key": opKey, "semantic_key": existing.SemanticKey, "actual_execution": false, "deduplicated": true, "pid": os.Getpid(), "at": time.Now().UTC()})
		return &result, nil
	}
	started := time.Now()
	content, semanticKey, effect, err := h.execute(ctx, call)
	result := contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: content, IsError: err != nil}
	if err != nil {
		result.Content = err.Error()
	}
	ledger[opKey] = ledgerEntry{Result: result, Tool: call.Name, SemanticKey: semanticKey, RecordedAt: time.Now().UTC()}
	if err := writeAtomicJSON(ledgerPath, ledger); err != nil {
		return nil, err
	}
	appendEvent(h.cfg.RunDir, map[string]any{"kind": "tool_dispatch", "candidate": h.cfg.Candidate, "task": h.cfg.Task, "scenario": h.cfg.Scenario, "tool": call.Name, "call_id": call.ID, "operation_key": opKey, "semantic_key": semanticKey, "actual_execution": true, "effect_applied": effect, "deduplicated": false, "is_error": result.IsError, "elapsed_ms": time.Since(started).Milliseconds(), "pid": os.Getpid(), "at": time.Now().UTC()})
	if err == nil && h.cfg.Scenario == "s1" && isCrashTool(h.cfg.Task, call.Name) && !fileExists(filepath.Join(h.cfg.RunDir, "crash-s1.done")) {
		must(os.WriteFile(filepath.Join(h.cfg.RunDir, "crash-s1.done"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600))
		appendEvent(h.cfg.RunDir, map[string]any{"kind": "injected_crash", "window": "after_effect_before_receipt", "tool": call.Name, "pid": os.Getpid(), "exit_code": crashExit, "at": time.Now().UTC()})
		os.Exit(crashExit)
	}
	return &result, nil
}

func (h *toolHost) execute(ctx context.Context, call contract.ToolCall) (string, string, bool, error) {
	workspace := filepath.Join(h.cfg.RunDir, "workspace")
	if h.cfg.Task == "t1" {
		return executeT1(ctx, workspace, call)
	}
	return executeT2(ctx, workspace, call)
}

func executeT1(_ context.Context, workspace string, call contract.ToolCall) (string, string, bool, error) {
	baselinePath := filepath.Join(workspace, "inputs", "baseline.json")
	switch call.Name {
	case "read_baseline":
		data, err := os.ReadFile(baselinePath)
		if err != nil {
			return "", "read_baseline", false, err
		}
		hash := sha256.Sum256(data)
		return fmt.Sprintf(`{"baseline":%s,"sha256":"%x"}`, strings.TrimSpace(string(data)), hash), "read_baseline", false, nil
	case "calculate_scenarios":
		var input struct {
			BaselineID string    `json:"baseline_id"`
			Mass       float64   `json:"mass_kg"`
			Distance   float64   `json:"distance_ly"`
			C          float64   `json:"speed_of_light_m_s"`
			Speeds     []float64 `json:"cruise_speed_c"`
		}
		data, err := os.ReadFile(baselinePath)
		if err != nil {
			return "", "calculate_scenarios", false, err
		}
		if err := json.Unmarshal(data, &input); err != nil {
			return "", "calculate_scenarios", false, err
		}
		rows := make([]map[string]float64, 0, len(input.Speeds))
		for _, beta := range input.Speeds {
			speed := beta * input.C
			rows = append(rows, map[string]float64{"beta": beta, "kinetic_energy_j": 0.5 * input.Mass * speed * speed, "cruise_years": input.Distance / beta})
		}
		out := map[string]any{"baseline_id": input.BaselineID, "mass_kg": input.Mass, "rows": rows}
		if err := writeAtomicJSON(filepath.Join(workspace, "outputs", "calculation.json"), out); err != nil {
			return "", "calculate_scenarios", false, err
		}
		return mustJSON(out), "calculate_scenarios", true, nil
	case "verify_results":
		var calc struct {
			BaselineID string               `json:"baseline_id"`
			Rows       []map[string]float64 `json:"rows"`
		}
		if err := readJSON(filepath.Join(workspace, "outputs", "calculation.json"), &calc); err != nil {
			return "", "verify_results", false, err
		}
		passed := calc.BaselineID == "corona-prephase-a-v1" && len(calc.Rows) == 3
		for _, row := range calc.Rows {
			beta := row["beta"]
			expectedE := 0.5 * 1e9 * (beta * 299792458.0) * (beta * 299792458.0)
			expectedY := 4.25 / beta
			passed = passed && relative(row["kinetic_energy_j"], expectedE) < 1e-12 && relative(row["cruise_years"], expectedY) < 1e-12
		}
		out := map[string]any{"passed": passed, "method": "independent_formula", "rows": len(calc.Rows)}
		if err := writeAtomicJSON(filepath.Join(workspace, "outputs", "verification.json"), out); err != nil {
			return "", "verify_results", false, err
		}
		if !passed {
			return mustJSON(out), "verify_results", true, errors.New("independent verification failed")
		}
		return mustJSON(out), "verify_results", true, nil
	default:
		return "", call.Name, false, fmt.Errorf("unknown T1 tool %q", call.Name)
	}
}

func executeT2(ctx context.Context, workspace string, call contract.ToolCall) (string, string, bool, error) {
	switch call.Name {
	case "inspect_workspace":
		var files []string
		_ = filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(workspace, path)
				files = append(files, rel)
			}
			return nil
		})
		sort.Strings(files)
		output, code := runCommand(ctx, workspace, "sh", "acceptance.sh")
		return mustJSON(map[string]any{"files": files, "initial_acceptance_exit": code, "output": output}), "inspect_workspace", false, nil
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
			return "", "read_file", false, err
		}
		if !oneOf(args.Path, "model/tests/test_physics.py", "verification/run_checks.sh", "task.md") {
			return "", "read_file:" + args.Path, false, errors.New("path not allowed")
		}
		data, err := os.ReadFile(filepath.Join(workspace, args.Path))
		return string(data), "read_file:" + args.Path, false, err
	case "apply_repair":
		var args struct {
			RepairID string `json:"repair_id"`
		}
		if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
			return "", "apply_repair", false, err
		}
		path, old, new, err := repairSpec(workspace, args.RepairID)
		if err != nil {
			return "", "apply_repair:" + args.RepairID, false, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "apply_repair:" + args.RepairID, false, err
		}
		text := string(data)
		if strings.Contains(text, old) {
			if err := os.WriteFile(path, []byte(strings.Replace(text, old, new, 1)), 0600); err != nil {
				return "", "apply_repair:" + args.RepairID, false, err
			}
			return mustJSON(map[string]any{"status": "applied", "repair_id": args.RepairID}), "apply_repair:" + args.RepairID, true, nil
		}
		if strings.Contains(text, new) {
			return `{"status":"already_applied"}`, "apply_repair:" + args.RepairID, false, nil
		}
		if !strings.Contains(text, old) {
			return "", "apply_repair:" + args.RepairID, false, errors.New("registered old text not found")
		}
		return "", "apply_repair:" + args.RepairID, false, errors.New("unreachable repair state")
	case "run_acceptance":
		output, code := runCommand(ctx, workspace, "sh", "acceptance.sh")
		content := mustJSON(map[string]any{"exit_code": code, "output": output})
		if code != 0 {
			return content, "run_acceptance", false, fmt.Errorf("acceptance exited %d", code)
		}
		return content, "run_acceptance", false, nil
	case "verify_manifest":
		output, code := runCommand(ctx, workspace, "sh", "acceptance.sh")
		checks := map[string]bool{}
		for _, id := range []string{"fix_gamma_expectation", "fix_timeout_portability", "fix_workdir"} {
			path, _, new, _ := repairSpec(workspace, id)
			data, _ := os.ReadFile(path)
			checks[id] = strings.Contains(string(data), new)
		}
		passed := code == 0
		for _, ok := range checks {
			passed = passed && ok
		}
		receipt := map[string]any{"passed": passed, "checks": checks, "acceptance_exit": code, "output": output}
		if err := writeAtomicJSON(filepath.Join(workspace, "outputs", "verification.json"), receipt); err != nil {
			return "", "verify_manifest", false, err
		}
		if !passed {
			return mustJSON(receipt), "verify_manifest", true, errors.New("manifest verification failed")
		}
		return mustJSON(receipt), "verify_manifest", true, nil
	default:
		return "", call.Name, false, fmt.Errorf("unknown T2 tool %q", call.Name)
	}
}

func repairSpec(workspace, id string) (string, string, string, error) {
	switch id {
	case "fix_gamma_expectation":
		return filepath.Join(workspace, "model/tests/test_physics.py"), "        self.assertAlmostEqual(lorentz_gamma(0.03), 1.0004503039763835, places=12)", "        expected = 1.0 / math.sqrt(1.0 - 0.03 * 0.03)\n        self.assertAlmostEqual(lorentz_gamma(0.03), expected, places=14)", nil
	case "fix_timeout_portability":
		return filepath.Join(workspace, "verification/run_checks.sh"), "timeout 120 python3 -m unittest discover -s tests -v", "PYTHONPATH=.. python3 -m unittest discover -s tests -v", nil
	case "fix_workdir":
		return filepath.Join(workspace, "verification/run_checks.sh"), "cd outputs/model", "cd \"$(dirname \"$0\")/../model\"", nil
	default:
		return "", "", "", fmt.Errorf("unknown repair id %q", id)
	}
}

func validateWorkspace(ctx context.Context, cfg config) (bool, string) {
	workspace := filepath.Join(cfg.RunDir, "workspace")
	if cfg.Task == "t1" {
		var verification struct {
			Passed bool `json:"passed"`
		}
		if err := readJSON(filepath.Join(workspace, "outputs", "verification.json"), &verification); err != nil {
			return false, err.Error()
		}
		return verification.Passed, "T1 independent verification passed"
	}
	output, code := runCommand(ctx, workspace, "sh", "acceptance.sh")
	if code != 0 {
		return false, fmt.Sprintf("T2 acceptance exit %d: %s", code, output)
	}
	var receipt struct {
		Passed bool `json:"passed"`
	}
	if err := readJSON(filepath.Join(workspace, "outputs", "verification.json"), &receipt); err != nil {
		return false, err.Error()
	}
	return receipt.Passed, "T2 acceptance and manifest passed"
}

func maybeCrashAfterConfirmed(cfg config, tool string) {
	if cfg.Scenario != "s2" || !isCrashTool(cfg.Task, tool) {
		return
	}
	marker := filepath.Join(cfg.RunDir, "crash-s2.done")
	if fileExists(marker) {
		return
	}
	must(os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600))
	appendEvent(cfg.RunDir, map[string]any{"kind": "injected_crash", "window": "after_confirmed_progress", "tool": tool, "pid": os.Getpid(), "exit_code": crashExit, "at": time.Now().UTC()})
	os.Exit(crashExit)
}

func isCrashTool(task, tool string) bool {
	return (task == "t1" && tool == "calculate_scenarios") || (task == "t2" && tool == "apply_repair")
}

func writeResult(cfg config, state loopState, runID string, history int, correct bool, validation error) {
	validationText := ""
	if validation != nil {
		validationText = validation.Error()
	}
	recordJSON(filepath.Join(cfg.RunDir, "result.json"), map[string]any{"candidate": cfg.Candidate, "task": cfg.Task, "scenario": cfg.Scenario, "final": state.Final, "done": state.Done, "llm_calls": state.LLMCalls, "input_tokens": state.InputTokens, "output_tokens": state.OutputTokens, "tool_results": state.ToolResults, "loom_run_id": runID, "loom_history_count": history, "independent_correct": correct, "validation": validationText, "completed_at": time.Now().UTC()})
}

func databaseURL() string {
	if value := os.Getenv("LOOM_VALUE_DATABASE_URL"); value != "" {
		return value
	}
	return "postgres://jinyitao@127.0.0.1:5432/weave_next?sslmode=disable"
}
func runCommand(ctx context.Context, dir, name string, args ...string) (string, int) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	return string(out) + "\n" + err.Error(), -1
}
func relative(actual, expected float64) float64 {
	if expected == 0 {
		return actual
	}
	v := (actual - expected) / expected
	if v < 0 {
		return -v
	}
	return v
}
func loomRunID(result *loom.RunResult) string {
	if result == nil {
		return ""
	}
	return result.RunID
}
func oneOf(value string, values ...string) bool {
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}
func stateString(s loom.State, key string) string { v, _ := s[key].(string); return v }
func stateBool(s loom.State, key string) bool     { v, _ := s[key].(bool); return v }
func stateInt(s loom.State, key string) int {
	switch v := s[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}
func mustJSON(value any) string         { data, err := json.Marshal(value); must(err); return string(data) }
func fileExists(path string) bool       { _, err := os.Stat(path); return err == nil }
func fatalf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...); os.Exit(1) }
func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}
func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func readJSONIfExists(path string, value any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func recordJSON(path string, value any) { must(writeAtomicJSON(path, value)) }
func writeAtomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func appendEvent(runDir string, value any) {
	data, err := json.Marshal(value)
	must(err)
	f, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	must(err)
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	must(err)
}
