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
	"os/exec"
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
	}
	args = append(args, "--")

	runCtx := ctx
	cancel := func() {}
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()

	cmd := exec.Command(cliPath, args...)
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
		parsed := parseClaudeOutput(stdout)
		done <- outcome{parsed: parsed, err: cmd.Wait()}
	}()

	select {
	case finished := <-done:
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
	for _, entry := range os.Environ() {
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
	if claudeUsesHostOAuth(env["WEAVE_CLAUDE_AUTH_MODE"]) {
		return ""
	}
	return strings.TrimSpace(model)
}

type claudeOutput struct {
	output      string
	sessionID   string
	status      string
	errText     string
	resultSeen  bool
	resultCount int
	parseFailed bool
	usage       *UsageReceipt
	diagnostics []Diagnostic
	events      []Event
}

func parseClaudeOutput(stdout io.Reader) claudeOutput {
	var output claudeOutput
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
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
			}
		case "assistant":
			message := jsonObject(event["message"])
			if message == nil {
				continue
			}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(message["content"], &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				if block.Type == "text" && block.Text != "" {
					output.events = append(output.events, Event{Kind: "text", Text: block.Text})
				}
			}
		case "result":
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
		Output:      parsed.output,
		SessionID:   parsed.sessionID,
		Status:      parsed.status,
		Usage:       parsed.usage,
		Diagnostics: append([]Diagnostic(nil), parsed.diagnostics...),
	}
}

func finishClaudeRun(parsed claudeOutput, stderr string, waitErr error) (RunResult, error) {
	result := claudeRunResult(parsed)
	if waitErr == nil && parsed.resultSeen && parsed.status == "completed" {
		return result, nil
	}
	result.Status = "failed"
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
