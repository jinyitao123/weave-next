// Command probe is an opt-in acceptance experiment, not a product entry point.
// It exercises the pinned Loom library with real tools and the existing CLI LLM adapter.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
)

var root string
var caseName string

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func record(name string, value any) {
	b, err := json.MarshalIndent(value, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(root, name), b, 0600))
}
func event(value any) {
	b, err := json.Marshal(value)
	must(err)
	f, err := os.OpenFile(filepath.Join(root, caseName+"-events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	must(err)
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	must(err)
}

type localInference struct {
	backend engine.Backend
	seq     int
}

func (e *localInference) ExecRemote(ctx context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, prompt string, _ []execspec.Attachment) (engine.RunResult, error) {
	e.seq++
	dir := filepath.Join(root, "inference-workspace")
	must(os.MkdirAll(dir, 0700))
	must(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("This is a bounded inference-only experiment. Return the requested protocol JSON. Do not execute native tools, shell commands, or subagents. Tool names in the serialized request are for Loom to execute.\n"), 0600))
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	nativeTool := false
	result, err := e.backend.Run(c, engine.RunSpec{WorkDir: dir, Prompt: prompt, EngineVersion: os.Getenv("LOOM_PROBE_CLI_VERSION"), Timeout: 3 * time.Minute, OnPublicEvent: func(v engine.Event, _ bool) {
		if v.Kind == "tool_call" {
			nativeTool = true
			cancel()
		}
	}})
	record(fmt.Sprintf("%s-inference-%d-%d.json", caseName, os.Getpid(), e.seq), map[string]any{"output": result.Output, "usage": result.Usage, "reported_models": result.ReportedModels, "native_tool_attempt": nativeTool, "status": result.Status, "error": result.Err})
	if nativeTool {
		return result, fmt.Errorf("inference driver attempted a native tool; experiment rejected")
	}
	return result, err
}

type crashStore struct {
	loom.Store
	armed bool
}

func (s *crashStore) Put(ctx context.Context, ns, key string, b []byte) error {
	if err := s.Store.Put(ctx, ns, key, b); err != nil {
		return err
	}
	if s.armed && strings.HasPrefix(ns, "checkpoint:loom-probe:") && !strings.Contains(key, "/") {
		var cp struct {
			LastStep string `json:"last_step"`
			RunID    string `json:"run_id"`
			Seq      int64  `json:"seq"`
		}
		if json.Unmarshal(b, &cp) == nil && cp.LastStep == "calculate" {
			record(caseName+"-crash.json", map[string]any{"run_id": cp.RunID, "checkpoint_sequence": cp.Seq, "last_step": cp.LastStep, "pid": os.Getpid(), "exit_code": 86, "at": time.Now().UTC()})
			fmt.Println("injected process exit after calculate checkpoint", cp.RunID)
			os.Exit(86)
		}
	}
	return nil
}
func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: probe <root> <toolloop|mem|pg|pgsafe> <run|crash|resume|fork>")
		os.Exit(2)
	}
	root, caseName = os.Args[1], os.Args[2]
	phase := os.Args[3]
	must(os.MkdirAll(root, 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var store loom.Store = loom.NewMemStore()
	if strings.HasPrefix(caseName, "pg") {
		s, err := pgstore.New(os.Getenv("LOOM_PROBE_DATABASE_URL"))
		must(err)
		defer s.Close()
		store = s
	}
	if phase == "crash" {
		store = &crashStore{Store: store, armed: true}
	}
	tools := mcphost.NewHTTPHost("http://127.0.0.1:18193/mcp")
	backend, err := engine.New(engine.Claude, os.Getenv("LOOM_PROBE_CLI_PATH"))
	must(err)
	agent := &registry.AgentRecord{Name: "loom-kernel-validation", ID: "loom-kernel-validation", WorkspaceID: "default", Version: 1, Engine: engine.Claude}
	llm, err := runtimellm.New(&localInference{backend: backend}, "default", agent, execution.AgentExecutionStamp{AgentID: agent.ID, AgentVersion: 1, ExecutionScope: execution.ScopeTeamWorkerLeaf})
	must(err)
	g := loom.NewGraph("loom-probe:"+caseName, "load", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(20))
	if caseName == "toolloop" {
		g = loom.NewGraph("loom-probe:toolloop", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(20))
		g.AddStep("chat", stdlib.NewToolLoopStep(llm, tools, stdlib.ToolLoopOpts{MaxIterations: 6, SystemPrompt: "Use the three provided tools in order: load_baseline, calculate_scenarios, verify_results. Each exactly once. Then produce a concise Chinese table with baseline SHA256, three kinetic energies and cruise times, verification receipts and conceptual limits. You have no Bash tool; request only the supplied tool names. Never claim execution without returned evidence."}), loom.End())
	} else {
		steps := []struct{ name, tool, next string }{{"load", "load_baseline", "calculate"}, {"calculate", "calculate_scenarios", "verify"}, {"verify", "verify_results", "summarize"}}
		for _, s := range steps {
			s := s
			g.AddStep(s.name, func(ctx context.Context, state loom.State) (loom.State, error) {
				result, err := tools.Dispatch(ctx, contract.ToolCall{ID: caseName + "-" + s.name, Name: s.tool, Args: "{}"})
				if err != nil {
					return nil, err
				}
				if result.IsError {
					return nil, fmt.Errorf("tool %s returned error", s.tool)
				}
				event(map[string]any{"kind": "real_tool_completed", "step": s.name, "tool": s.tool, "content": result.Content, "at": time.Now().UTC(), "pid": os.Getpid()})
				update := loom.State{s.name: result.Content}
				if caseName == "pgsafe" && s.name == "calculate" {
					update["__yield"] = true
					update["__yield_phase"] = "after_step"
				}
				return update, nil
			}, loom.Always(s.next))
		}
		g.AddStep("summarize", func(ctx context.Context, state loom.State) (loom.State, error) {
			raw, err := json.Marshal(map[string]any{"baseline": state["load"], "calculation": state["calculate"], "verification": state["verify"], "scenario_note": state["scenario_note"]})
			if err != nil {
				return nil, err
			}
			response, err := llm.Chat(ctx, contract.ChatRequest{Messages: []contract.Message{{Role: "system", Content: "Only summarize the supplied real tool receipts. Do not call tools. Return a concise Chinese table with the 3 speeds, classical kinetic energies and cruise years, source SHA256, verification status, and conceptual boundaries. If scenario_note is present, append it clearly as a changed interpretation assumption, without changing the source receipts or numbers."}, {Role: "user", Content: string(raw)}}})
			if err != nil {
				return nil, err
			}
			event(map[string]any{"kind": "summary_completed", "pid": os.Getpid(), "at": time.Now().UTC()})
			return loom.State{"output": response.Content}, nil
		}, loom.End())
	}
	input := loom.State{"messages": []contract.Message{{Role: "user", Content: "请真实读取日冕基线并复算 0.01c、0.03c、0.05c，质量 1e9 kg，返回证据。"}}}
	var result *loom.RunResult
	if phase == "resume" || phase == "fork" {
		var cp struct {
			RunID string `json:"run_id"`
		}
		data, e := os.ReadFile(filepath.Join(root, caseName+"-crash.json"))
		must(e)
		must(json.Unmarshal(data, &cp))
		if phase == "fork" {
			history, historyErr := g.History(ctx, store, cp.RunID)
			must(historyErr)
			if len(history) == 0 {
				must(fmt.Errorf("fork requires a saved history checkpoint"))
			}
			var seq int64
			for _, checkpoint := range history {
				if checkpoint.LastStep == "verify" {
					seq = checkpoint.Seq
				}
			}
			if seq == 0 {
				must(fmt.Errorf("fork requires the verification checkpoint before summary"))
			}
			key := fmt.Sprintf("%s/%012d", cp.RunID, seq)
			before, readErr := store.Get(ctx, "checkpoint:"+g.Name, key)
			must(readErr)
			result, err = g.ResumeAt(ctx, cp.RunID, seq, loom.State{"scenario_note": "该分支只改变报告解释，要求强调未计入推进效率与减速成本；复用同一组已核验数值。"}, store)
			after, readErr := store.Get(ctx, "checkpoint:"+g.Name, key)
			must(readErr)
			beforeHash, afterHash := sha256.Sum256(before), sha256.Sum256(after)
			record(caseName+"-fork-source-integrity.json", map[string]any{"source_run_id": cp.RunID, "source_sequence": seq, "before_sha256": fmt.Sprintf("%x", beforeHash), "after_sha256": fmt.Sprintf("%x", afterHash), "unchanged": beforeHash == afterHash})
			if beforeHash != afterHash {
				must(fmt.Errorf("fork changed source checkpoint"))
			}
		} else {
			result, err = g.Resume(ctx, cp.RunID, loom.State{}, store)
		}
	} else {
		result, err = g.Run(ctx, input, store)
	}
	record(caseName+"-"+phase+"-result.json", map[string]any{"result": result, "error": fmt.Sprint(err), "pid": os.Getpid(), "at": time.Now().UTC()})
	if result != nil {
		fmt.Println("run", result.RunID, "stop", result.StopReason)
		if text, ok := result.State["output"].(string); ok {
			must(os.WriteFile(filepath.Join(root, caseName+"-"+phase+"-report.md"), []byte(text), 0600))
		}
		history, herr := g.History(ctx, store, result.RunID)
		record(caseName+"-"+phase+"-history.json", map[string]any{"history": history, "error": fmt.Sprint(herr)})
	}
	must(err)
}
