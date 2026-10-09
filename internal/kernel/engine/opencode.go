package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type opencodeBackend struct {
	cliPath string
}

func (b *opencodeBackend) Name() string { return OpenCode }

// OpenCodeModelRef splits a weave model string into the provider/model pair
// OpenCode expects. A bare model name (no "/") belongs to the generated
// OpenAI-compatible provider that execenv writes into opencode.json, so it
// defaults to that provider instead of being misread as a provider name.
func OpenCodeModelRef(model string) (provider, name string) {
	if p, n, ok := strings.Cut(model, "/"); ok {
		return p, n
	}
	return "openai", model
}

func (b *opencodeBackend) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	cliPath := b.cliPath
	if cliPath == "" {
		cliPath = OpenCode
	}
	args := []string{"run", "--format", "json", "--dir", spec.WorkDir}
	if spec.Model != "" {
		provider, name := OpenCodeModelRef(spec.Model)
		args = append(args, "--model", provider+"/"+name)
	}
	runCtx := ctx
	cancel := func() {}
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()

	cmd, commandErr := isolatedCommand(context.Background(), cliPath, args, spec.Isolation)
	if commandErr != nil {
		return failedOpenCodeResult(commandErr)
	}
	cmd.Dir = spec.WorkDir
	cmd.Env = envWithCLIPath(mergedEnv(spec.Env), cliPath)
	cmd.Stdin = strings.NewReader(promptWithOutputSchema(spec.Prompt, spec.OutputSchema))
	applyProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failedOpenCodeResult(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return failedOpenCodeResult(err)
	}

	type outcome struct {
		parsed openCodeOutput
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		parsed := parseOpenCodeOutput(stdout)
		done <- outcome{parsed: parsed, err: cmd.Wait()}
	}()

	select {
	case finished := <-done:
		result, err := finishOpenCodeRun(finished.parsed, stderr.String(), finished.err)
		bindUsageReceipt(&result, spec)
		if len(spec.OutputSchema) > 0 {
			result.Output = unwrapJSONOutput(result.Output)
		}
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
		result := openCodeRunResult(finished.parsed)
		result.Status = "timeout"
		result.Err = runCtx.Err().Error()
		bindUsageReceipt(&result, spec)
		return result, fmt.Errorf("opencode: %w", runCtx.Err())
	}
}

func mergedEnv(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, entry := range cliAmbientEnvForRun(overrides) {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	for key, value := range overrides {
		env[key] = value
	}
	env["OPENCODE_PERMISSION"] = `{"*":"allow"}`

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

type openCodeOutput struct {
	text          strings.Builder
	sessionID     string
	errText       string
	parseFailed   bool
	terminalCount int
	steps         []openCodeStepReceipt
	usage         *UsageReceipt
	diagnostics   []Diagnostic
}

type openCodeStepReceipt struct {
	reason string
	tokens *reportedTokenUsage
	cost   *float64
	raw    string
}

func parseOpenCodeOutput(stdout interface{ Read([]byte) (int, error) }) openCodeOutput {
	var output openCodeOutput
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil {
			output.parseFailed = true
			output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "opencode emitted malformed JSONL: "+err.Error()))
			continue
		}
		eventType := jsonString(event["type"])
		if sessionID := firstJSONField(event, "sessionID", "session_id"); sessionID != "" {
			output.sessionID = sessionID
		}
		switch eventType {
		case "session":
			if output.sessionID == "" {
				output.sessionID = jsonString(event["id"])
			}
		case "text", "message":
			if eventRole(event) == "user" {
				continue
			}
			if text := eventText(event); text != "" {
				output.text.WriteString(text)
			}
		case "error":
			if message := eventErrorMessage(event); message != "" {
				output.errText = message
			}
		case "step_finish":
			step, err := parseOpenCodeStep(event, string(line))
			if err != nil {
				output.parseFailed = true
				output.diagnostics = append(output.diagnostics, diagnostic("usage_invalid", "opencode usage is invalid: "+err.Error()))
				continue
			}
			output.steps = append(output.steps, step)
			if step.reason != "" && step.reason != "tool-calls" {
				output.terminalCount++
			}
		case "tool_call", "tool_result", "done":
		default:
			// New OpenCode event types are forward-compatible by default.
		}
	}
	if err := scanner.Err(); err != nil {
		if output.errText == "" {
			output.errText = err.Error()
		}
		output.parseFailed = true
		output.diagnostics = append(output.diagnostics, diagnostic("usage_parse_failed", "opencode JSONL scan failed: "+err.Error()))
	}
	output.finalizeUsage()
	return output
}

func parseOpenCodeStep(event map[string]json.RawMessage, raw string) (openCodeStepReceipt, error) {
	part := jsonObject(event["part"])
	if part == nil {
		return openCodeStepReceipt{}, errors.New("step_finish.part is required")
	}
	step := openCodeStepReceipt{reason: jsonString(part["reason"]), raw: raw}
	cost, err := decodeFloatField(part, "cost")
	if err != nil {
		return openCodeStepReceipt{}, err
	}
	step.cost = cost
	tokens := jsonObject(part["tokens"])
	if tokens == nil {
		return step, nil
	}
	total, totalErr := decodeIntField(tokens, "total")
	input, inputErr := decodeIntField(tokens, "input")
	output, outputErr := decodeIntField(tokens, "output")
	reasoning, reasoningErr := decodeIntField(tokens, "reasoning")
	cache := jsonObject(tokens["cache"])
	var cacheRead, cacheWrite *int
	var cacheReadErr, cacheWriteErr error
	if cache != nil {
		cacheRead, cacheReadErr = decodeIntField(cache, "read")
		cacheWrite, cacheWriteErr = decodeIntField(cache, "write")
	}
	if err := errors.Join(totalErr, inputErr, outputErr, reasoningErr, cacheReadErr, cacheWriteErr); err != nil {
		return openCodeStepReceipt{}, err
	}
	if total == nil || input == nil || output == nil || reasoning == nil || cacheRead == nil || cacheWrite == nil {
		return step, nil
	}
	if *reasoning > *output {
		return openCodeStepReceipt{}, errors.New("reasoning tokens exceed inclusive output tokens")
	}
	normalizedInput, err := addReportedInt(*input, *cacheRead)
	if err == nil {
		normalizedInput, err = addReportedInt(normalizedInput, *cacheWrite)
	}
	if err != nil {
		return openCodeStepReceipt{}, err
	}
	normalizedTotal, err := addReportedInt(normalizedInput, *output)
	if err != nil {
		return openCodeStepReceipt{}, err
	}
	if normalizedTotal != *total {
		return openCodeStepReceipt{}, errors.New("tokens.total does not equal inclusive input plus inclusive output")
	}
	step.tokens = &reportedTokenUsage{InputTokens: normalizedInput, OutputTokens: *output}
	return step, nil
}

func (output *openCodeOutput) finalizeUsage() {
	if output.parseFailed {
		output.usage = nil
		return
	}
	if output.terminalCount == 0 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_missing", "opencode unique terminal step_finish event is missing"))
		output.usage = nil
		return
	}
	if output.terminalCount != 1 {
		output.diagnostics = append(output.diagnostics, diagnostic("usage_terminal_ambiguous", "opencode emitted more than one terminal step_finish event"))
		output.usage = nil
		return
	}
	var raw string
	tokensComplete := len(output.steps) > 0
	costComplete := len(output.steps) > 0
	var totals reportedTokenUsage
	var cost float64
	for _, step := range output.steps {
		raw = appendRawSummary(raw, step.raw)
		if step.tokens == nil {
			tokensComplete = false
		} else if tokensComplete {
			var err error
			totals.InputTokens, err = addReportedInt(totals.InputTokens, step.tokens.InputTokens)
			if err == nil {
				totals.OutputTokens, err = addReportedInt(totals.OutputTokens, step.tokens.OutputTokens)
			}
			if err != nil {
				output.diagnostics = append(output.diagnostics, diagnostic("usage_invalid", "opencode usage is invalid: "+err.Error()))
				output.usage = nil
				return
			}
		}
		if step.cost == nil {
			costComplete = false
		} else if costComplete {
			cost += *step.cost
		}
	}
	var tokenDimension *reportedTokenUsage
	if tokensComplete {
		tokenDimension = &totals
	}
	var costDimension *float64
	if costComplete {
		costDimension = &cost
	}
	receipt, diagnostics := newUsageReceipt(tokenDimension, costDimension, raw)
	output.usage = receipt
	output.diagnostics = append(output.diagnostics, diagnostics...)
}

func eventText(event map[string]json.RawMessage) string {
	if part := jsonObject(event["part"]); part != nil {
		if text := jsonString(part["text"]); text != "" {
			return text
		}
	}
	return jsonString(event["content"])
}

func eventRole(event map[string]json.RawMessage) string {
	if role := jsonString(event["role"]); role != "" {
		return role
	}
	if part := jsonObject(event["part"]); part != nil {
		return jsonString(part["role"])
	}
	return ""
}

func eventErrorMessage(event map[string]json.RawMessage) string {
	errorObject := jsonObject(event["error"])
	if errorObject == nil {
		return jsonString(event["message"])
	}
	if data := jsonObject(errorObject["data"]); data != nil {
		if message := jsonString(data["message"]); message != "" {
			return message
		}
	}
	if message := jsonString(errorObject["message"]); message != "" {
		return message
	}
	return jsonString(event["message"])
}

func firstJSONField(object map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := jsonString(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	return value
}

func finishOpenCodeRun(parsed openCodeOutput, stderr string, waitErr error) (RunResult, error) {
	result := openCodeRunResult(parsed)
	if waitErr == nil && parsed.errText == "" {
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
	return result, fmt.Errorf("opencode: %s", result.Err)
}

func openCodeRunResult(parsed openCodeOutput) RunResult {
	return RunResult{
		Output:      parsed.text.String(),
		SessionID:   parsed.sessionID,
		Usage:       parsed.usage,
		Diagnostics: append([]Diagnostic(nil), parsed.diagnostics...),
	}
}

func failedOpenCodeResult(err error) (RunResult, error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	result := RunResult{Status: "failed", Err: err.Error()}
	return result, fmt.Errorf("opencode: %w", err)
}
