package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// nativeInference uses the configured service's native tool-call protocol.
// It never starts a CLI, executes a proposed tool, or adds business answers.
type nativeInference struct {
	cfg                    settings
	endpoint, token, model string
	seq                    atomic.Int64
}

func newNativeInference(cfg settings) *nativeInference {
	home, err := os.UserHomeDir()
	must(err)
	raw, err := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	must(err)
	var local struct {
		Env map[string]string `json:"env"`
	}
	must(json.Unmarshal(raw, &local))
	token := local.Env["ANTHROPIC_AUTH_TOKEN"]
	if token == "" {
		token = local.Env["ANTHROPIC_API_KEY"]
	}
	if token == "" {
		panic("native acceptance inference credential unavailable")
	}
	model := local.Env["ANTHROPIC_MODEL"]
	if strings.EqualFold(model, "k3[1m]") {
		model = "k3"
	}
	endpoint := strings.TrimRight(local.Env["ANTHROPIC_BASE_URL"], "/") + "/v1/chat/completions"
	write(filepath.Join(cfg.Root, "stage-d/native-transport.json"), map[string]any{
		"endpoint": endpoint, "requested_model": model, "client_identity": "weave-acceptance/1.0",
		"credential_source":    "existing local CLI environment configuration; value not archived",
		"native_cli_execution": false, "tool_execution_owner": "Loom via managed MCP",
	})
	return &nativeInference{cfg: cfg, endpoint: endpoint, token: token, model: model}
}

func (n *nativeInference) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	messages := []any{}
	seenCalls := map[string]bool{}
	for _, m := range request.Messages {
		if m.Role == "tool" && m.ToolCallID != "" && !seenCalls[m.ToolCallID] {
			name := "run_python"
			if len(request.Tools) > 0 {
				name = request.Tools[0].Name
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": m.ToolCallID, "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}}})
			seenCalls[m.ToolCallID] = true
		}
		row := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			row["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := []any{}
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Args}})
				seenCalls[c.ID] = true
			}
			row["tool_calls"] = calls
		}
		messages = append(messages, row)
	}
	limit := request.MaxTokens
	if limit == 0 {
		limit = 16384
	}
	body := map[string]any{"model": n.model, "messages": messages, "max_tokens": limit, "stream": false}
	if len(request.Tools) > 0 {
		defs := []any{}
		for _, t := range request.Tools {
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
		}
		body["tools"] = defs
	}
	if request.Schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": request.Schema}}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+n.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "weave-acceptance/1.0")
	seq := n.seq.Add(1)
	response, err := (&http.Client{Timeout: 20 * time.Minute}).Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	record := map[string]any{"at": time.Now().UTC(), "request": body, "http_status": response.StatusCode}
	if json.Valid(raw) {
		record["response"] = json.RawMessage(raw)
	} else {
		record["response_text"] = string(raw)
	}
	write(filepath.Join(n.cfg.Root, fmt.Sprintf("stage-d/native-model-%d-%03d.json", os.Getpid(), seq)), record)
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("native inference HTTP %d: %s", response.StatusCode, string(raw))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Calls   []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Input  int `json:"prompt_tokens"`
			Output int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	if len(decoded.Choices) != 1 {
		return nil, fmt.Errorf("native inference expected one choice")
	}
	choice := decoded.Choices[0]
	if choice.Finish == "length" {
		return nil, fmt.Errorf("native inference output truncated")
	}
	out := &contract.ChatResponse{Content: choice.Message.Content, StopReason: choice.Finish, Usage: contract.Usage{InputTokens: decoded.Usage.Input, OutputTokens: decoded.Usage.Output}}
	for _, c := range choice.Message.Calls {
		out.ToolCalls = append(out.ToolCalls, contract.ToolCall{ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments})
	}
	return out, nil
}
