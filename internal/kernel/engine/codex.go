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
	// A ChatGPT subscription owns its Codex model selection independently of
	// the API-provider model stored on the AgentRecord. Passing that provider
	// model through can select an API-only model (for example gpt-5.2) which the
	// same Codex CLI correctly rejects under ChatGPT authentication. Let the
	// host Codex configuration choose its subscription-supported default.
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
		parsed := parseCodexOutput(stdout)
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
	if codexUsesHostChatGPTAuth(env["WEAVE_CODEX_AUTH_MODE"]) {
		return ""
	}
	return codexModelName(model)
}

type codexOutput struct {
	output          string
	sessionID       string
	status          string
	errText         string
	completionCount int
	parseFailed     bool
	usage           *UsageReceipt
	diagnostics     []Diagnostic
}

const codexJSONLMaxEventBytes = 16 * 1024 * 1024

func parseCodexOutput(stdout interface{ Read([]byte) (int, error) }) codexOutput {
	var output codexOutput
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
		case "item.completed":
			item := jsonObject(event["item"])
			switch jsonString(item["type"]) {
			case "agent_message":
				output.output = jsonString(item["text"])
			case "mcp_tool_call":
				// RunResult has no event stream yet; recognising the item keeps
				// the wire format forward-compatible without widening the API.
			}
		case "turn.completed":
			output.completionCount++
			receipt, diagnostics := parseCodexUsage(event, string(line))
			output.usage = receipt
			output.diagnostics = append(output.diagnostics, diagnostics...)
			if output.status != "failed" {
				output.status = "completed"
			}
		case "turn.failed":
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
		Output:      parsed.output,
		SessionID:   parsed.sessionID,
		Status:      parsed.status,
		Usage:       parsed.usage,
		Diagnostics: append([]Diagnostic(nil), parsed.diagnostics...),
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
