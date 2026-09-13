package agentcatalog_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// This opt-in acceptance sample spends real inference and writes model-generated
// files only under its explicitly configured directory. Ordinary tests skip it.
func TestStandardPublishedToolsRealModel(t *testing.T) {
	root := os.Getenv("WEAVE_LOOM_REAL_MODEL_DIR")
	if root == "" {
		t.Skip("set WEAVE_LOOM_REAL_MODEL_DIR to run real inference")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	output := filepath.Join(root, "output")
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "model.py")); err == nil {
		t.Fatal("real sample requires a fresh output directory")
	}
	baseline, err := os.ReadFile("../../../docs/验收/2026-09-07-Runtime真实中断恢复/inputs/upstream/lead/model/baseline_frozen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "baseline_frozen.yaml"), baseline, 0600); err != nil {
		t.Fatal(err)
	}
	baselineHash := fmt.Sprintf("%x", sha256.Sum256(baseline))
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool2, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	pool = pool2
	schema := json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Python 3 source executed in the task output directory. Use standard library only; read input files, write your own model and tests, and execute them."}},"required":["code"],"additionalProperties":false}`)
	description := "Execute Python 3 in the task directory. Read files, write model and tests, run them; stdout/stderr and exit code are returned. Do not install packages or access paths outside this directory."
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "publication-real-tools", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "run_python", "description": description, "inputSchema": schema, "annotations": map[string]any{"readOnlyHint": false}}}}
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments struct {
					Code string `json:"code"`
				} `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.Name != "run_python" {
				http.Error(w, "unknown tool", 400)
				return
			}
			call := effects.Add(1)
			callCtx, stop := context.WithTimeout(r.Context(), 20*time.Second)
			defer stop()
			cmd := exec.CommandContext(callCtx, "/usr/bin/python3", "-c", params.Arguments.Code)
			cmd.Dir = output
			data, runErr := cmd.CombinedOutput()
			exit := 0
			if runErr != nil {
				exit = 1
			}
			receipt := map[string]any{"call": call, "tool": "run_python", "code": params.Arguments.Code, "output": string(data), "exit_code": exit, "pid": os.Getpid(), "at": time.Now().UTC()}
			writeRealSample(t, root, fmt.Sprintf("tool-%02d.json", call), receipt)
			body, _ := json.Marshal(receipt)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}, "isError": exit != 0}
		default:
			http.Error(w, "unknown method", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	workspace := "real-publication"
	ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspace, UserID: "test"})
	key := []byte(strings.Repeat("k", 32))
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspace); err != nil {
		t.Fatal(err)
	}
	mcp := mcpregistry.New(pool, key)
	registered, err := mcp.Create(ctx, workspace, "test", mcpregistry.UpsertServerRequest{Slug: "python", DisplayName: "Python sample", Transport: mcpregistry.TransportStreamableHTTP, URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mcp.RecordProbeSuccess(ctx, workspace, registered.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "run_python", Description: description, InputSchema: schema}})
	if err != nil {
		t.Fatal(err)
	}
	prompt := `You are validating a published engineering member's tool wiring. Use run_python to read baseline_frozen.yaml first. It is the sole input; preserve it unchanged. Create model.py that reads this file and computes classical kinetic energy for the crewed_1mt and orbital_material_5mt cases, dust impact energy, cruise travel years and rotating habitat gravity. Standard library only; support the simple YAML subset needed without external packages. Write results.json with numeric keys energy_1mt_j, energy_5mt_j, dust_j, travel_years, gravity_m_s2, and string key baseline_sha256. Write test_model.py with independently derived checks; execute model.py and the tests through run_python. Write report.md explaining units, baseline identity, the single approved speed, and conceptual limits. Deliver all four files. Do not merely print code. Never claim an action without tool results. No native CLI tools. You decide the tool sequence and implementation.`
	bundle, resolver, saved := publishStandardToolSample(t, pool, key, workspace, server.URL, registered.ID, registered.FunctionalRevision, "run_python", prompt)
	writeRealSample(t, root, "published-artifact.json", saved)
	backend, err := engine.New(engine.Claude, os.Getenv("WEAVE_LOOM_REAL_CLI_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	inference := &publicationRealInference{t: t, root: root, backend: backend}
	agent := &registry.AgentRecord{WorkspaceID: workspace, ID: bundle.Agent.AgentID, Version: int(bundle.Agent.AgentVersion), Engine: engine.Claude}
	model, err := runtimellm.New(inference, workspace, agent, execution.AgentExecutionStamp{AgentID: agent.ID, AgentVersion: agent.Version, ExecutionScope: execution.ScopeTeamWorkerLeaf})
	if err != nil {
		t.Fatal(err)
	}
	opts, closer, err := workflow.NewRuntimeHostFactory().Build(ctx, bundle, publicationTestSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	opts.LLM = model
	compiled, err := compiler.CompileFrozenWithRegistry(ctx, publicationDescriptors(t), bundle, resolver, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Run(ctx, loom.State{"last_user_message": prompt, "messages": []contract.Message{{Role: "user", Content: prompt}}}, loom.NewMemStore())
	writeRealSample(t, root, "run-result.json", map[string]any{"result": result, "error": fmt.Sprint(err), "tool_calls": effects.Load(), "model_turns": inference.seq})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"model.py", "test_model.py", "results.json", "report.md"} {
		if info, err := os.Stat(filepath.Join(output, file)); err != nil || info.Size() == 0 {
			t.Fatalf("missing delivery %s", file)
		}
	}
	// Independent numerical acceptance outside the model's generated tests.
	raw, err := os.ReadFile(filepath.Join(output, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	speed := 0.03 * 299792458.0
	expected := map[string]float64{"energy_1mt_j": 0.5 * 1e9 * speed * speed, "energy_5mt_j": 0.5 * 5e9 * speed * speed, "dust_j": 0.5 * 1e-6 * speed * speed, "travel_years": 4.25 / 0.03, "gravity_m_s2": math.Pow(2*math.Pi*2/60, 2) * 1000}
	for key, want := range expected {
		got, ok := values[key].(float64)
		if !ok || math.Abs(got-want)/want > 1e-7 {
			t.Fatalf("independent check %s got=%v want=%g", key, values[key], want)
		}
	}
	if values["baseline_sha256"] != baselineHash {
		t.Fatal("baseline provenance mismatch")
	}
	after, err := os.ReadFile(filepath.Join(output, "baseline_frozen.yaml"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(after)) != baselineHash {
		t.Fatal("baseline modified")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "test_model.py")
	cmd.Dir = output
	data, err := cmd.CombinedOutput()
	writeRealSample(t, root, "independent-rerun.json", map[string]any{"output": string(data), "error": fmt.Sprint(err), "expected": expected})
	if err != nil {
		t.Fatalf("generated tests failed: %s", data)
	}
	if effects.Load() < 2 {
		t.Fatal("no meaningful model/tool loop")
	}
}

type publicationRealInference struct {
	t       *testing.T
	root    string
	backend engine.Backend
	seq     int
}

func (e *publicationRealInference) ExecRemote(ctx context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, prompt string, _ []execspec.Attachment) (engine.RunResult, error) {
	e.seq++
	dir := filepath.Join(e.root, "inference")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return engine.RunResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Return the requested inference protocol JSON only. Do not execute native tools or subagents. All requested tool calls belong in tool_calls for Loom to execute.\n"), 0600); err != nil {
		return engine.RunResult{}, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var native atomic.Bool
	result, err := e.backend.Run(child, engine.RunSpec{WorkDir: dir, Prompt: prompt, Timeout: 8 * time.Minute, EngineVersion: os.Getenv("WEAVE_LOOM_REAL_CLI_VERSION"), OnPublicEvent: func(event engine.Event, _ bool) {
		if event.Kind == "tool_call" {
			native.Store(true)
			cancel()
		}
	}})
	writeRealSample(e.t, e.root, fmt.Sprintf("model-%02d.json", e.seq), map[string]any{"request": prompt, "output": result.Output, "usage": result.Usage, "models": result.ReportedModels, "native_tool_attempt": native.Load(), "error": fmt.Sprint(err)})
	if native.Load() {
		return result, fmt.Errorf("native tool attempted; real sample rejected")
	}
	return result, err
}
func writeRealSample(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
		t.Error(err)
	}
}
