package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

var toolSchema = json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Python 3 standard-library source, executed in this task directory; read inputs/ and write outputs/. Commands must exit within 25 seconds."},"export":{"type":"boolean","description":"After verification, read and register the complete current UTF-8 output files for Workbench delivery."}},"required":["code"],"additionalProperties":false}`)

const toolDescription = "Run Python 3 standard-library code in this engineering task directory. Read inputs/, write outputs/, run bounded verification commands. Return real stdout, stderr and exit status. export=true registers actual output file contents for delivery. No global installation or access to other workspaces."

type inference struct {
	cfg     settings
	backend engine.Backend
	seq     atomic.Int64
}

// The acceptance transport uses ordinary JSON argument objects. It avoids
// asking the model to escape a JSON string inside another JSON string. It
// supplies no task solution, and native tools are still rejected below.
type directInference struct{ executor *inference }

func (m directInference) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	prompt := `You are the inference provider for an external agent runtime.
Do not call native tools, shell commands, or subagents. The runtime owns all
tool execution. Read the request and return exactly one JSON object:
{"content":"concise assistant summary or final answer","tool_calls":[{"name":"allowed tool name","args":{"required_argument":"value"}}]}
The args field is an actual JSON object, never a JSON-encoded string.
Use the exact allowed tool names and argument schemas in the request.
Prefer small bounded tool calls: at most 6000 characters of code per call,
writing one file or a small edit before inspecting its actual result. Do not
try to generate a whole application in one giant tool argument. This is an
output transport limit; decide the task's actual work and steps yourself.
Preserve real newlines in code strings through valid JSON escaping. No Markdown
fences or text outside the response object. If no tool is needed, tool_calls=[];
if request.schema is present, content must be its valid JSON result.
<request>
` + string(payload) + "\n</request>"
	var usage contract.Usage
	for attempt := 0; attempt < 3; attempt++ {
		result, err := m.executor.ExecRemote(ctx, "", nil, execution.AgentExecutionStamp{}, prompt, nil)
		if err != nil {
			return nil, err
		}
		if err := engine.ValidateUsageReceipt(result.Usage); err != nil {
			return nil, err
		}
		if result.Usage != nil {
			usage.InputTokens += result.Usage.InputTokens
			usage.OutputTokens += result.Usage.OutputTokens
			usage.CostUSD += result.Usage.CostUSD
		}
		out, parseErr := parseDirectResponse(result, req.Tools)
		if parseErr == nil {
			out.Usage = usage
			return out, nil
		}
		if attempt == 2 {
			return nil, parseErr
		}
		prompt = "Your previous inference response failed JSON protocol validation: " + parseErr.Error() + `
Repair the response format yourself. Do not execute native tools or commands.
Return exactly one JSON object with content:string and tool_calls:[{name:string,args:object}].
Preserve the intended code and task content. Fix only serialization, escaping,
or trailing text. No Markdown fences or duplicate closing braces.
<previous_response>
` + result.Output + "\n</previous_response>"
	}
	return nil, errors.New("inference repair attempts exhausted")
}

func parseDirectResponse(result engine.RunResult, tools []contract.ToolDef) (*contract.ChatResponse, error) {
	var response struct {
		Content string `json:"content"`
		Calls   []struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool_calls"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(result.Output)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("inference protocol: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("inference protocol trailing data")
	}
	allowed := map[string]bool{}
	for _, tool := range tools {
		allowed[tool.Name] = true
	}
	out := &contract.ChatResponse{Content: response.Content}
	for i, call := range response.Calls {
		var args map[string]json.RawMessage
		if !allowed[call.Name] || json.Unmarshal(call.Args, &args) != nil || args == nil {
			return nil, errors.New("inference tool name or argument object invalid")
		}
		out.ToolCalls = append(out.ToolCalls, contract.ToolCall{ID: fmt.Sprintf("inference-%s-%d", result.SessionID, i), Name: call.Name, Args: string(call.Args)})
	}
	return out, nil
}

func (e *inference) ExecRemote(ctx context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, prompt string, _ []execspec.Attachment) (engine.RunResult, error) {
	seq := e.seq.Add(1)
	dir := filepath.Join(e.cfg.Root, "stage-d/inference")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return engine.RunResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Return the requested inference JSON protocol. Do not call native tools or subagents. Put proposed tool calls in the protocol for Loom or Workbench to dispatch.\n"), 0600); err != nil {
		return engine.RunResult{}, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var native atomic.Bool
	result, err := e.backend.Run(child, engine.RunSpec{WorkDir: dir, Prompt: prompt, EngineVersion: e.cfg.CLIVersion, Timeout: 20 * time.Minute, OnPublicEvent: func(event engine.Event, _ bool) {
		if event.Kind == "tool_call" {
			native.Store(true)
			cancel()
		}
	}})
	write(filepath.Join(e.cfg.Root, fmt.Sprintf("stage-d/model-%d-%03d.json", os.Getpid(), seq)), map[string]any{"request": prompt, "result": result, "native_tool_attempt": native.Load(), "error": fmt.Sprint(err), "at": time.Now().UTC()})
	if native.Load() {
		return result, errors.New("native tool attempted; acceptance rejected")
	}
	return result, err
}
func serveGateway(ctx context.Context, cfg settings) {
	// This acceptance transport must expose inference only. Prompt instructions
	// and cancellation after a tool event do not remove native tool capability.
	wrapper := filepath.Join(cfg.Root, "inference-only-cli.sh")
	quotedCLI := "'" + strings.ReplaceAll(cfg.CLIPath, "'", "'\"'\"'") + "'"
	must(os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quotedCLI+" --tools '' --strict-mcp-config --mcp-config '{\"mcpServers\":{}}' --disable-slash-commands --system-prompt 'You are an inference-only model. Follow the supplied request and return its JSON response protocol. All proposed tool calls belong in that response for the caller to execute.' \"$@\"\n"), 0700))
	backend, err := engine.New(engine.Claude, wrapper)
	must(err)
	infer := &inference{cfg: cfg, backend: backend}
	var model interface {
		Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error)
	} = directInference{executor: infer}
	workDir := filepath.Join(cfg.Root, "stage-d")
	if os.Getenv("WEAVE_ACCEPTANCE_NATIVE_API") == "1" {
		model = newNativeInference(cfg)
		workDir = filepath.Join(cfg.Root, "stage-d/task")
	}
	var tools atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "engineering-acceptance", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "run_python", "description": toolDescription, "inputSchema": toolSchema, "annotations": map[string]any{"readOnlyHint": false}}}}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Code   string `json:"code"`
					Export bool   `json:"export"`
				} `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil || p.Name != "run_python" {
				http.Error(w, "unknown tool", 400)
				return
			}
			seq := tools.Add(1)
			callCtx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(callCtx, "/usr/bin/python3", "-c", p.Arguments.Code)
			cmd.Dir = workDir
			data, runErr := cmd.CombinedOutput()
			exit := 0
			if runErr != nil {
				exit = 1
			}
			receipt := map[string]any{"call": seq, "code": p.Arguments.Code, "stdout_stderr": string(data), "exit_code": exit, "at": time.Now().UTC()}
			if p.Arguments.Export && exit == 0 {
				files, err := readOutputs(filepath.Join(workDir, "outputs"))
				if err != nil {
					receipt["export_error"] = err.Error()
					exit = 1
				} else {
					receipt["weave_member_artifacts_v1"] = files
				}
			}
			write(filepath.Join(cfg.Root, fmt.Sprintf("stage-d/tool-%d-%03d.json", os.Getpid(), seq)), receipt)
			// The trigger is only a counted successful test command. The controller
			// additionally verifies the tool receipt committed before killing Weave.
			counted := regexp.MustCompile(`(?m)^Ran [1-9][0-9]* tests? in .+$`).Match(data) && regexp.MustCompile(`(?m)^OK$`).Match(data)
			if exit == 0 && counted && strings.Contains(strings.ToLower(p.Arguments.Code), "test") {
				if _, err := os.Stat(filepath.Join(cfg.Root, "stage-d/model-tests-milestone.json")); os.IsNotExist(err) {
					write(filepath.Join(cfg.Root, "stage-d/model-tests-milestone.json"), receipt)
				}
			}
			raw, _ := json.Marshal(receipt)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}, "isError": exit != 0}
		default:
			http.Error(w, "unknown method", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"tool_call_id"`
				ToolCalls  []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Parameters  json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
			Stream    bool `json:"stream"`
			MaxTokens int  `json:"max_tokens"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid model request", 400)
			return
		}
		request := contract.ChatRequest{Model: req.Model, MaxTokens: req.MaxTokens}
		for _, msg := range req.Messages {
			content := ""
			if json.Unmarshal(msg.Content, &content) != nil {
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(msg.Content, &blocks) == nil {
					for _, b := range blocks {
						if b.Type == "text" {
							content += b.Text
						}
					}
				}
			}
			m := contract.Message{Role: msg.Role, Content: content, ToolCallID: msg.ToolCallID}
			for _, call := range msg.ToolCalls {
				m.ToolCalls = append(m.ToolCalls, contract.ToolCall{ID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments})
			}
			request.Messages = append(request.Messages, m)
		}
		workerRequest := false
		for _, tool := range req.Tools {
			request.Tools = append(request.Tools, contract.ToolDef{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
			if strings.Contains(tool.Function.Name, "run_python") {
				workerRequest = true
			}
		}
		_, milestoneErr := os.Stat(filepath.Join(cfg.Root, "stage-d/model-tests-milestone.json"))
		_, faultErr := os.Stat(filepath.Join(cfg.Root, "stage-d/fault-completed.json"))
		if workerRequest && milestoneErr == nil && os.IsNotExist(faultErr) {
			write(filepath.Join(cfg.Root, "stage-d/awaiting-fault.json"), map[string]any{"at": time.Now().UTC(), "gateway_pid": os.Getpid(), "next_inference_not_started": true})
			<-r.Context().Done()
			return
		}
		response, err := model.Chat(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		calls := []any{}
		for i, call := range response.ToolCalls {
			calls = append(calls, map[string]any{"index": i, "id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Args}})
		}
		finish := "stop"
		if len(calls) > 0 {
			finish = "tool_calls"
		}
		usage := map[string]any{"prompt_tokens": response.Usage.InputTokens, "completion_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.InputTokens + response.Usage.OutputTokens}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			delta := map[string]any{"role": "assistant", "content": response.Content}
			if len(calls) > 0 {
				delta["tool_calls"] = calls
			}
			raw, _ := json.Marshal(map[string]any{"id": "acceptance-response", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			raw, _ = json.Marshal(map[string]any{"id": "acceptance-response", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
		} else {
			message := map[string]any{"role": "assistant", "content": response.Content}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "acceptance-response", "model": req.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
		}
	})
	server := &http.Server{Addr: "127.0.0.1:" + cfg.GatewayPort, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); server.Close() }()
	err = server.ListenAndServe()
	if err != http.ErrServerClosed {
		must(err)
	}
}
func readOutputs(root string) ([]fileartifact.File, error) {
	var files []fileartifact.File
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "__pycache__" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("output symlinks unsupported")
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, ".pyc") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		kind := mime.TypeByExtension(filepath.Ext(name))
		if kind == "" {
			kind = "text/plain"
		}
		files = append(files, fileartifact.File{Path: filepath.ToSlash(name), ContentType: kind, Content: string(raw)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := fileartifact.Validate(files); err != nil {
		return nil, err
	}
	return files, nil
}
