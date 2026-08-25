package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	cmd := exec.Command(cliPath, args...)
	cmd.Env = envWithCLIPath(mergedEnv(spec.Env), cliPath)
	cmd.Stdin = strings.NewReader(spec.Prompt)
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
		return finishOpenCodeRun(finished.parsed, stderr.String(), finished.err)
	case <-runCtx.Done():
		_ = terminateProcess(cmd)
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = killProcess(cmd)
			<-done
		}
		result := RunResult{Status: "timeout", Err: runCtx.Err().Error()}
		return result, fmt.Errorf("opencode: %w", runCtx.Err())
	}
}

func mergedEnv(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
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
	text      strings.Builder
	sessionID string
	errText   string
}

func parseOpenCodeOutput(stdout interface{ Read([]byte) (int, error) }) openCodeOutput {
	var output openCodeOutput
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var event map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
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
		case "tool_call", "tool_result", "step_finish", "done":
		default:
			// New OpenCode event types are forward-compatible by default.
		}
	}
	if err := scanner.Err(); err != nil && output.errText == "" {
		output.errText = err.Error()
	}
	return output
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
	result := RunResult{Output: parsed.text.String(), SessionID: parsed.sessionID}
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

func failedOpenCodeResult(err error) (RunResult, error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	result := RunResult{Status: "failed", Err: err.Error()}
	return result, fmt.Errorf("opencode: %w", err)
}
