package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type claudeBackend struct {
	cliPath string
}

func (b *claudeBackend) Name() string { return Claude }

func (b *claudeBackend) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	cliPath := b.cliPath
	if cliPath == "" {
		cliPath = Claude
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if model := claudeModelForRun(spec.Model, spec.Env); model != "" {
		args = append(args, "--model", model)
	}
	mcpPath := filepath.Join(spec.WorkDir, ".weave-mcp.json")
	if _, err := os.Stat(mcpPath); err == nil {
		args = append(args, "--mcp-config="+mcpPath)
		if len(spec.MCPServers) > 0 {
			args = append(args, "--strict-mcp-config")
		}
	}
	args = append(args, "--")

	runCtx := ctx
	cancel := func() {}
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()

	cmd, commandErr := isolatedCommand(context.Background(), cliPath, args, spec.Isolation)
	if commandErr != nil {
		return failedClaudeResult(commandErr)
	}
	cmd.Dir = spec.WorkDir
	cmd.Env = envWithCLIPath(mergedClaudeEnv(spec.Env), cliPath)
	cmd.Stdin = strings.NewReader(spec.Prompt)
	applyProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failedClaudeResult(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return failedClaudeResult(err)
	}

	type outcome struct {
		parsed claudeOutput
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		parsed := parseClaudeOutputWithEvents(stdout, spec.OnPublicEvent)
		done <- outcome{parsed: parsed, err: cmd.Wait()}
	}()

	select {
	case finished := <-done:
		// Cancellation may race with process exit. A deliberate stop retains
		// its authority even if the native exit also reports a signal.
		if contextErr := runCtx.Err(); contextErr != nil {
			result := claudeRunResult(finished.parsed)
			result.Status, result.Err = "timeout", contextErr.Error()
			bindUsageReceipt(&result, spec)
			return result, fmt.Errorf("claude: %w", contextErr)
		}
		result, err := finishClaudeRun(finished.parsed, stderr.String(), finished.err)
		bindUsageReceipt(&result, spec)
		return result, err
	case <-runCtx.Done():
		_ = terminateProcess(cmd)
		var finished outcome
		select {
		case finished = <-done:
		case <-time.After(time.Second):
			_ = killProcess(cmd)
			finished = <-done
		}
		result := claudeRunResult(finished.parsed)
		result.Status = "timeout"
		result.Err = runCtx.Err().Error()
		bindUsageReceipt(&result, spec)
		return result, fmt.Errorf("claude: %w", runCtx.Err())
	}
}

func mergedClaudeEnv(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, entry := range cliAmbientEnvForRun(overrides) {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	for key, value := range overrides {
		env[key] = value
	}
	if claudeUsesHostOAuth(env["WEAVE_CLAUDE_AUTH_MODE"]) {
		for _, key := range []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ONEAPI_API_KEY",
			"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		} {
			delete(env, key)
		}
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func claudeUsesHostOAuth(authMode string) bool {
	return strings.EqualFold(strings.TrimSpace(authMode), "oauth")
}

func claudeModelForRun(model string, env map[string]string) string {
	return strings.TrimSpace(model)
}

type claudeOutput struct {
	output         string
	sessionID      string
	status         string
	errText        string
	resultSeen     bool
	resultCount    int
	parseFailed    bool
	usage          *UsageReceipt
	diagnostics    []Diagnostic
	events         []Event
	reportedModels []string
	toolObserved   bool
}

func parseClaudeOutput(stdout io.Reader) claudeOutput {
	return parseClaudeOutputWithEvents(stdout, nil)
}

func parseClaudeOutputWithEvents(stdout io.Reader, publish func(Event, bool)) claudeOutput {
	var output claudeOutput
	tools := map[string]string{}
	emit := func(event Event) {
		truncated := len(event.Text) > 4096 || len(event.Input) > 4096 || len(event.Output) > 4096
		event.Text, event.Input, event.Output = boundedCodexText(event.Text), boundedCodexText(event.Input), boundedCodexText(event.Output)
		if len(event.Tool) > 160 || len(event.CallID) > 200 {
			return
		}
		if len(output.events) < 200 {
			output.events = append(output.events, event)
		}
		if publish != nil {
			publish(event, truncated)
		}
	}
	observeModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 200 {
			return
		}
		for _, prior := range output.reportedModels {
			if prior == model {
				return
			}
		}
		if len(output.reportedModels) < 16 {
			output.reportedModels = append(output.reportedModels, model)
		}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), codexJSONLMaxEventBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil {
			output.parseFailed = true
			output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "claude emitted malformed JSONL: "+err.Error()))
			continue
		}
		switch jsonString(event["type"]) {
		case "system":
			if jsonString(event["subtype"]) == "init" {
				output.sessionID = jsonString(event["session_id"])
				observeModel(jsonString(event["model"]))
			}
		case "assistant", "user":
			message := jsonObject(event["message"])
			if message == nil {
				continue
			}
			observeModel(jsonString(message["model"]))
			var blocks []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			}
			if json.Unmarshal(message["content"], &blocks) != nil {
				continue
			}
			for index, block := range blocks {
				switch block.Type {
				case "text":
					if jsonString(event["type"]) == "assistant" && block.Text != "" {
						id := jsonString(message["id"])
						if id != "" {
							id = fmt.Sprintf("%s:%d", id, index)
						}
						emit(Event{Kind: "text", CallID: id, Text: block.Text})
					}
				case "tool_use":
					output.toolObserved = true
					tools[block.ID] = block.Name
					emit(Event{Kind: "tool_call", CallID: block.ID, Tool: block.Name, Input: string(block.Input), Status: "running"})
				case "tool_result":
					content := jsonString(block.Content)
					if content == "" {
						content = string(block.Content)
					}
					status := "ok"
					if block.IsError {
						status = "error"
					}
					emit(Event{Kind: "tool_result", CallID: block.ToolUseID, Tool: tools[block.ToolUseID], Output: content, Status: status})
				}
			}
		case "result":
			modelUsage := jsonObject(event["modelUsage"])
			modelNames := make([]string, 0, len(modelUsage))
			for name := range modelUsage {
				modelNames = append(modelNames, name)
			}
			sort.Strings(modelNames)
			for _, name := range modelNames {
				observeModel(name)
			}
			output.resultSeen = true
			output.resultCount++
			output.output = jsonString(event["result"])
			receipt, diagnostics := parseClaudeUsage(event, string(line))
			output.usage = receipt
			output.diagnostics = append(output.diagnostics, diagnostics...)
			subtype := jsonString(event["subtype"])
			var isError bool
			_ = json.Unmarshal(event["is_error"], &isError)
			if subtype == "success" && !isError {
				output.status = "completed"
			} else {
				output.status = "failed"
				output.errText = output.output
				if output.errText == "" {
					output.errText = subtype
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = io.Copy(io.Discard, stdout)
		if output.errText == "" {
			output.errText = err.Error()
		}
		output.status = "failed"
		output.parseFailed = true
		output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "claude JSONL scan failed: "+err.Error()))
	}
	if output.resultCount == 0 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_missing", "claude result event is missing"))
		output.usage = nil
	} else if output.resultCount != 1 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_ambiguous", "claude emitted more than one result event"))
		output.usage = nil
	}
	if output.parseFailed {
		output.usage = nil
	}
	return output
}

func parseClaudeUsage(event map[string]json.RawMessage, raw string) (*UsageReceipt, []Diagnostic) {
	usage := jsonObject(event["usage"])
	var tokens *reportedTokenUsage
	var diagnostics []Diagnostic
	if usage != nil {
		input, inputErr := decodeIntField(usage, "input_tokens")
		cacheCreate, createErr := decodeIntField(usage, "cache_creation_input_tokens")
		cacheRead, readErr := decodeIntField(usage, "cache_read_input_tokens")
		output, outputErr := decodeIntField(usage, "output_tokens")
		if err := errors.Join(inputErr, createErr, readErr, outputErr); err != nil {
			return nil, []Diagnostic{diagnostic("usage_invalid", "claude usage is invalid: "+err.Error())}
		}
		if input != nil && cacheCreate != nil && cacheRead != nil && output != nil {
			totalInput, err := addReportedInt(*input, *cacheCreate)
			if err == nil {
				totalInput, err = addReportedInt(totalInput, *cacheRead)
			}
			if err != nil {
				return nil, []Diagnostic{diagnostic("usage_invalid", "claude usage is invalid: "+err.Error())}
			}
			tokens = &reportedTokenUsage{InputTokens: totalInput, OutputTokens: *output}
		}
	}
	cost, err := decodeFloatField(event, "total_cost_usd")
	if err != nil {
		return nil, []Diagnostic{diagnostic("usage_invalid", "claude usage is invalid: "+err.Error())}
	}
	receipt, receiptDiagnostics := newUsageReceipt(tokens, cost, raw)
	diagnostics = append(diagnostics, receiptDiagnostics...)
	return receipt, diagnostics
}

func claudeRunResult(parsed claudeOutput) RunResult {
	return RunResult{
		Output:                   parsed.output,
		SessionID:                parsed.sessionID,
		Status:                   parsed.status,
		Usage:                    parsed.usage,
		Diagnostics:              append([]Diagnostic(nil), parsed.diagnostics...),
		Events:                   append([]Event(nil), parsed.events...),
		ReportedModels:           append([]string(nil), parsed.reportedModels...),
		RetrySafeBeforeExecution: parsed.status == "failed" && parsed.resultCount == 1 && !parsed.parseFailed && !parsed.toolObserved,
	}
}

func finishClaudeRun(parsed claudeOutput, stderr string, waitErr error) (RunResult, error) {
	result := claudeRunResult(parsed)
	if waitErr == nil && parsed.resultSeen && parsed.status == "completed" {
		return result, nil
	}
	result.Status = "failed"
	// Native process status is authoritative. Earlier CLI warnings on stderr
	// must not hide a killed process behind an unrelated model/work error.
	if reason := processTerminationReason(waitErr); reason != "" {
		result.Err = reason
		return result, fmt.Errorf("claude: %s", reason)
	}
	result.Err = parsed.errText
	if result.Err == "" {
		result.Err = strings.TrimSpace(stderr)
	}
	if result.Err == "" && waitErr != nil {
		result.Err = waitErr.Error()
	}
	if result.Err == "" && !parsed.resultSeen {
		result.Err = "missing result event"
	}
	return result, fmt.Errorf("claude: %s", result.Err)
}

func failedClaudeResult(err error) (RunResult, error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	result := RunResult{Status: "failed", Err: err.Error()}
	return result, fmt.Errorf("claude: %w", err)
}
