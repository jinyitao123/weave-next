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
	"unicode/utf8"
)

type codexBackend struct {
	cliPath string
}

func (b *codexBackend) Name() string { return Codex }

func (b *codexBackend) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	cliPath := b.cliPath
	if cliPath == "" {
		cliPath = Codex
	}
	args := []string{
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
	}
	// Empty model follows the host default; explicit CLI-native names are
	// passed through under either native login or API authentication.
	if model := codexModelForRun(spec.Model, spec.Env); model != "" {
		args = append(args, "-m", model)
	}
	if len(spec.OutputSchema) > 0 {
		if !json.Valid(spec.OutputSchema) {
			return failedCodexResult(errors.New("invalid output schema"))
		}
		schemaFile, err := os.CreateTemp(spec.WorkDir, ".weave-output-schema-*.json")
		if err != nil {
			return failedCodexResult(fmt.Errorf("create output schema: %w", err))
		}
		schemaPath := schemaFile.Name()
		defer os.Remove(schemaPath)
		if _, err := schemaFile.Write(spec.OutputSchema); err != nil {
			_ = schemaFile.Close()
			return failedCodexResult(fmt.Errorf("write output schema: %w", err))
		}
		if err := schemaFile.Close(); err != nil {
			return failedCodexResult(fmt.Errorf("close output schema: %w", err))
		}
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args,
		"-C",
		spec.WorkDir,
		"-",
	)

	runCtx := ctx
	cancel := func() {}
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()

	cmd := exec.Command(cliPath, args...)
	cmd.Env = envWithCLIPath(codexEnv(spec.Env, spec.WorkDir), cliPath)
	cmd.Stdin = strings.NewReader(spec.Prompt)
	applyProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failedCodexResult(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return failedCodexResult(err)
	}

	type outcome struct {
		parsed codexOutput
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		parsed := parseCodexOutputWithEvents(stdout, spec.OnPublicEvent)
		done <- outcome{parsed: parsed, err: cmd.Wait()}
	}()

	select {
	case finished := <-done:
		result, err := finishCodexRun(finished.parsed, stderr.String(), finished.err)
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
		result := codexRunResult(finished.parsed)
		result.Status = "timeout"
		result.Err = runCtx.Err().Error()
		bindUsageReceipt(&result, spec)
		return result, fmt.Errorf("codex: %w", runCtx.Err())
	}
}

func codexEnv(overrides map[string]string, workDir string) []string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	for key, value := range overrides {
		env[key] = value
	}
	if codexUsesHostChatGPTAuth(env["WEAVE_CODEX_AUTH_MODE"]) {
		delete(env, "CODEX_HOME")
	} else {
		env["CODEX_HOME"] = filepath.Join(workDir, ".codex-home")
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

func codexUsesHostChatGPTAuth(authMode string) bool {
	return strings.EqualFold(strings.TrimSpace(authMode), "chatgpt")
}

func codexModelName(model string) string {
	_, name := OpenCodeModelRef(model)
	return name
}

func codexModelForRun(model string, env map[string]string) string {
	return codexModelName(model)
}

type codexOutput struct {
	output          string
	sessionID       string
	status          string
	errText         string
	completionCount int
	parseFailed     bool
	failedTurnSeen  bool
	toolObserved    bool
	usage           *UsageReceipt
	diagnostics     []Diagnostic
	events          []Event
}

const codexJSONLMaxEventBytes = 16 * 1024 * 1024

func parseCodexOutput(stdout interface{ Read([]byte) (int, error) }) codexOutput {
	return parseCodexOutputWithEvents(stdout, nil)
}

func parseCodexOutputWithEvents(stdout interface{ Read([]byte) (int, error) }, publish func(Event, bool)) codexOutput {
	var output codexOutput
	var lastMessageID, lastMessageText string
	// Codex agent_message is public text. reasoning is a separate item type
	// and is deliberately absent from this allowlist, including future variants.
	public := func(item map[string]json.RawMessage, kind string) {
		if publish == nil {
			return
		}
		if jsonString(item["type"]) == "agent_message" {
			text := jsonString(item["text"])
			bounded := boundedCodexText(text)
			id := jsonString(item["id"])
			if text != "" && (id == "" || id != lastMessageID || bounded != lastMessageText) {
				publish(Event{Kind: "text", CallID: id, Text: bounded}, len(text) > 4096)
				lastMessageID, lastMessageText = id, bounded
			}
		} else if event, ok := codexToolEvent(item, kind); ok {
			truncated := false
			for _, field := range []string{"command", "aggregated_output", "arguments", "input", "result", "output", "changes", "query", "error"} {
				raw := item[field]
				value := string(raw)
				if decoded := jsonString(raw); decoded != "" {
					value = decoded
				}
				truncated = truncated || len(value) > 4096
			}
			publish(event, truncated)
		}
	}
	scanner := bufio.NewScanner(stdout)
	// Codex emits command/tool results as a single JSONL event. A worker may
	// legitimately inspect a large artifact even when its final agent message is
	// small, so the Scanner default and the previous 1 MiB ceiling are too low.
	scanner.Buffer(make([]byte, 64*1024), codexJSONLMaxEventBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil {
			output.parseFailed = true
			output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "codex emitted malformed JSONL: "+err.Error()))
			continue
		}
		switch jsonString(event["type"]) {
		case "thread.started":
			output.sessionID = jsonString(event["thread_id"])
		case "item.started":
			item := jsonObject(event["item"])
			if kind := jsonString(item["type"]); kind != "agent_message" && kind != "reasoning" && kind != "" {
				output.toolObserved = true
			}
			public(item, "tool_call")
			if activity, ok := codexToolEvent(item, "tool_call"); ok && len(output.events) < 200 {
				output.events = append(output.events, activity)
			}
		case "item.completed":
			item := jsonObject(event["item"])
			if kind := jsonString(item["type"]); kind != "agent_message" && kind != "reasoning" && kind != "" {
				output.toolObserved = true
			}
			public(item, "tool_result")
			switch jsonString(item["type"]) {
			case "agent_message":
				output.output = jsonString(item["text"])
			}
			if activity, ok := codexToolEvent(item, "tool_result"); ok && len(output.events) < 200 {
				output.events = append(output.events, activity)
			}
		case "item.updated":
			item := jsonObject(event["item"])
			if kind := jsonString(item["type"]); kind != "agent_message" && kind != "reasoning" && kind != "" {
				output.toolObserved = true
			}
			public(item, "tool_call") // known in-progress tool snapshots remain running
		case "turn.completed":
			output.completionCount++
			receipt, diagnostics := parseCodexUsage(event, string(line))
			output.usage = receipt
			output.diagnostics = append(output.diagnostics, diagnostics...)
			if output.status != "failed" {
				output.status = "completed"
			}
		case "turn.failed":
			output.failedTurnSeen = true
			output.status = "failed"
			output.errText = eventErrorMessage(event)
			if output.errText == "" {
				output.errText = "turn failed"
			}
		case "error":
			output.status = "failed"
			output.errText = eventErrorMessage(event)
			if output.errText == "" {
				output.errText = "codex error"
			}
		default:
			// Unknown Codex event types are forward-compatible by default.
		}
	}
	if err := scanner.Err(); err != nil {
		// Scanner stops reading once its bound is exceeded. Drain the remaining
		// stdout so the child cannot block in a write while Run waits for exit.
		_, _ = io.Copy(io.Discard, stdout)
		output.status = "failed"
		output.errText = err.Error()
		output.parseFailed = true
		output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "codex JSONL scan failed: "+err.Error()))
	}
	if output.completionCount == 0 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_missing", "codex turn.completed event is missing"))
		output.usage = nil
	} else if output.completionCount != 1 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_ambiguous", "codex emitted more than one turn.completed event"))
		output.usage = nil
	}
	if output.parseFailed {
		output.usage = nil
	}
	return output
}

func boundedCodexText(value string) string {
	if len(value) <= 4096 {
		return value
	}
	value = value[:4096]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func codexRawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if text := jsonString(raw); text != "" {
		return boundedCodexText(text)
	}
	return boundedCodexText(string(raw))
}

// codexToolEvent normalises only execution items that Codex itself reports.
// Unknown items stay ignored rather than being guessed into tool activity.
func codexToolEvent(item map[string]json.RawMessage, kind string) (Event, bool) {
	if item == nil {
		return Event{}, false
	}
	itemType := jsonString(item["type"])
	activity := Event{Kind: kind, CallID: jsonString(item["id"]), Status: "running"}
	switch itemType {
	case "command_execution":
		activity.Tool = "shell"
		activity.Input = codexRawText(item["command"])
		activity.Output = codexRawText(item["aggregated_output"])
	case "mcp_tool_call":
		server := firstJSONField(item, "server", "server_name")
		tool := firstJSONField(item, "tool", "tool_name", "name")
		activity.Tool = strings.Trim(strings.Join([]string{server, tool}, "."), ".")
		if activity.Tool == "" {
			activity.Tool = "mcp_tool"
		}
		activity.Input = codexRawText(firstJSONRaw(item, "arguments", "input"))
		activity.Output = codexRawText(firstJSONRaw(item, "result", "output", "error"))
	case "file_change":
		activity.Tool = "file_change"
		activity.Input = codexRawText(firstJSONRaw(item, "changes", "input"))
		activity.Output = codexRawText(firstJSONRaw(item, "status", "output"))
	case "web_search":
		activity.Tool = "web_search"
		activity.Input = codexRawText(firstJSONRaw(item, "query", "input"))
		activity.Output = codexRawText(firstJSONRaw(item, "result", "output"))
	default:
		return Event{}, false
	}
	if kind == "tool_result" {
		activity.Status = "ok"
		itemStatus := strings.ToLower(jsonString(item["status"]))
		var exitCode int
		exitCodePresent := json.Unmarshal(item["exit_code"], &exitCode) == nil && len(item["exit_code"]) > 0
		if itemStatus == "failed" || itemStatus == "error" || exitCodePresent && exitCode != 0 || len(item["error"]) > 0 && string(item["error"]) != "null" {
			activity.Status = "error"
		}
	}
	if activity.CallID == "" {
		activity.CallID = itemType + ":" + activity.Tool
	}
	return activity, true
}

func firstJSONRaw(object map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if value, ok := object[key]; ok && len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

func parseCodexUsage(event map[string]json.RawMessage, raw string) (*UsageReceipt, []Diagnostic) {
	usage := jsonObject(event["usage"])
	var tokens *reportedTokenUsage
	if usage != nil {
		input, inputErr := decodeIntField(usage, "input_tokens")
		cached, cachedErr := decodeIntField(usage, "cached_input_tokens")
		output, outputErr := decodeIntField(usage, "output_tokens")
		reasoning, reasoningErr := decodeIntField(usage, "reasoning_output_tokens")
		if err := errors.Join(inputErr, cachedErr, outputErr, reasoningErr); err != nil {
			return nil, []Diagnostic{diagnostic("usage_invalid", "codex usage is invalid: "+err.Error())}
		}
		if input != nil && cached != nil && output != nil && reasoning != nil {
			if *cached > *input {
				return nil, []Diagnostic{diagnostic("usage_invalid", "codex cached_input_tokens exceeds inclusive input_tokens")}
			}
			if *reasoning > *output {
				return nil, []Diagnostic{diagnostic("usage_invalid", "codex reasoning_output_tokens exceeds inclusive output_tokens")}
			}
			tokens = &reportedTokenUsage{InputTokens: *input, OutputTokens: *output}
		}
	}
	return newUsageReceipt(tokens, nil, raw)
}

func codexRunResult(parsed codexOutput) RunResult {
	return RunResult{
		RetrySafeBeforeExecution: parsed.status == "failed" && parsed.failedTurnSeen && !parsed.parseFailed && !parsed.toolObserved,
		Output:                   parsed.output,
		SessionID:                parsed.sessionID,
		Status:                   parsed.status,
		Usage:                    parsed.usage,
		Diagnostics:              append([]Diagnostic(nil), parsed.diagnostics...),
		Events:                   append([]Event(nil), parsed.events...),
	}
}

func finishCodexRun(parsed codexOutput, stderr string, waitErr error) (RunResult, error) {
	result := codexRunResult(parsed)
	if waitErr == nil && parsed.status == "completed" && parsed.errText == "" {
		result.Status = "completed"
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
	if result.Err == "" {
		result.Err = "run ended without a completion event"
	}
	return result, fmt.Errorf("codex: %s", result.Err)
}

func failedCodexResult(err error) (RunResult, error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	result := RunResult{Status: "failed", Err: err.Error()}
	return result, fmt.Errorf("codex: %w", err)
}
