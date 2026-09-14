package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestUsageSummaryByteLimitSurvivesUnicodeJSONRoundTrip(t *testing.T) {
	for _, unit := range []string{"界", "🌍"} {
		for offset := 0; offset < 4; offset++ {
			raw := strings.Repeat("x", offset) + strings.Repeat(unit, 4096)
			receipt, _ := newUsageReceipt(&reportedTokenUsage{InputTokens: 1}, nil, raw)
			receipt.EngineVersion = "fixture 1"
			wire, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			var received UsageReceipt
			if err := json.Unmarshal(wire, &received); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(receipt.RawSummary) || received.RawSummary != receipt.RawSummary {
				t.Fatal("JSON changed a partial Unicode character")
			}
			if err := ValidateUsageReceipt(&received); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestParseClaudeUsageRealFixtureExplicitZero(t *testing.T) {
	parsed := parseClaudeOutput(bytes.NewReader(readFixture(t, "claude-2.1.245.jsonl")))
	if parsed.usage == nil {
		t.Fatalf("expected explicit zero receipt, diagnostics=%+v", parsed.diagnostics)
	}
	assertReceipt(t, parsed.usage, 0, 0, 0, true, true)
}

func TestParseClaudeUsageNormalizesExclusiveCacheCounters(t *testing.T) {
	parsed := parseClaudeOutput(strings.NewReader(`{"type":"result","subtype":"success","is_error":false,"result":"OK","total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":7,"output_tokens":5}}` + "\n"))
	if parsed.usage == nil {
		t.Fatalf("expected receipt, diagnostics=%+v", parsed.diagnostics)
	}
	assertReceipt(t, parsed.usage, 20, 5, 0.25, true, true)
}

func TestParseCodexUsageRealFixtureKeepsInclusiveInput(t *testing.T) {
	parsed := parseCodexOutput(bytes.NewReader(readFixture(t, "codex-0.144.5.jsonl")))
	if parsed.usage == nil {
		t.Fatalf("expected receipt, diagnostics=%+v", parsed.diagnostics)
	}
	assertReceipt(t, parsed.usage, 14058, 5, 0, true, false)
	if !hasDiagnostic(parsed.diagnostics, "usage_cost_unreported") {
		t.Fatalf("missing cost diagnostic: %+v", parsed.diagnostics)
	}
}

func TestParseCodexOutputCarriesObservedToolInputAndOutput(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-fixture"}`,
		`{"type":"item.started","item":{"id":"command-1","type":"command_execution","command":"printf hello"}}`,
		`{"type":"item.completed","item":{"id":"command-1","type":"command_execution","command":"printf hello","aggregated_output":"hello","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"message-1","type":"agent_message","text":"OK"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":2,"reasoning_output_tokens":0}}`,
	}, "\n") + "\n"
	parsed := parseCodexOutput(strings.NewReader(stream))
	if len(parsed.events) != 2 || parsed.events[0].Kind != "tool_call" || parsed.events[1].Kind != "tool_result" {
		t.Fatalf("events=%+v", parsed.events)
	}
	if parsed.events[0].Status != "running" || parsed.events[1].Status != "ok" || parsed.events[1].Tool != "shell" ||
		parsed.events[1].Input != "printf hello" || parsed.events[1].Output != "hello" {
		t.Fatalf("tool result=%+v", parsed.events[1])
	}
}

func TestParseOpenCodeUsageRealFixtureSumsStepsAndCache(t *testing.T) {
	parsed := parseOpenCodeOutput(bytes.NewReader(readFixture(t, "opencode-1.2.10.jsonl")))
	if parsed.usage == nil {
		t.Fatalf("expected receipt, diagnostics=%+v", parsed.diagnostics)
	}
	assertReceipt(t, parsed.usage, 21984, 62, 0.0034398, true, true)
	if parsed.terminalCount != 1 || len(parsed.steps) != 2 {
		t.Fatalf("terminal=%d steps=%d", parsed.terminalCount, len(parsed.steps))
	}
}

func TestParseOpenCodeMissingTerminalIsNotPartialUsage(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(readFixture(t, "opencode-1.2.10.jsonl"))), "\n")
	parsed := parseOpenCodeOutput(strings.NewReader(strings.Join(lines[:len(lines)-1], "\n") + "\n"))
	if parsed.usage != nil {
		t.Fatalf("partial steps must not become a receipt: %+v", parsed.usage)
	}
	if !hasDiagnostic(parsed.diagnostics, "usage_terminal_missing") {
		t.Fatalf("missing terminal diagnostic: %+v", parsed.diagnostics)
	}
}

func TestParseFailureNeverBecomesZeroUsage(t *testing.T) {
	parsed := parseCodexOutput(strings.NewReader("not-json\n" + string(readFixture(t, "codex-0.144.5.jsonl"))))
	if parsed.usage != nil {
		t.Fatalf("malformed stream must not produce usage: %+v", parsed.usage)
	}
	if !hasDiagnostic(parsed.diagnostics, "usage_parse_failed") {
		t.Fatalf("missing parse diagnostic: %+v", parsed.diagnostics)
	}
}

func TestCodexRejectsInvalidReportedUsage(t *testing.T) {
	parsed := parseCodexOutput(strings.NewReader(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":2,"output_tokens":1,"reasoning_output_tokens":0}}` + "\n"))
	if parsed.usage != nil || !hasDiagnostic(parsed.diagnostics, "usage_invalid") {
		t.Fatalf("invalid receipt accepted: usage=%+v diagnostics=%+v", parsed.usage, parsed.diagnostics)
	}
}

func TestOpenCodeOutputAlreadyIncludesReasoning(t *testing.T) {
	parsed := parseOpenCodeOutput(strings.NewReader(`{"type":"step_finish","sessionID":"session-fixture","part":{"type":"step-finish","reason":"stop","cost":0.00320222,"tokens":{"total":11165,"input":11027,"output":138,"reasoning":135,"cache":{"read":0,"write":0}}}}` + "\n"))
	if parsed.usage == nil {
		t.Fatalf("expected receipt, diagnostics=%+v", parsed.diagnostics)
	}
	assertReceipt(t, parsed.usage, 11027, 138, 0.00320222, true, true)
}

func TestFailedAndTimeoutRunsRetainReportedUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	tests := []struct {
		name    string
		fixture string
		backend func(string) Backend
	}{
		{name: Claude, fixture: "claude-2.1.245.jsonl", backend: func(path string) Backend { return &claudeBackend{cliPath: path} }},
		{name: Codex, fixture: "codex-0.144.5.jsonl", backend: func(path string) Backend { return &codexBackend{cliPath: path} }},
		{name: OpenCode, fixture: "opencode-1.2.10.jsonl", backend: func(path string) Backend { return &opencodeBackend{cliPath: path} }},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/failed", func(t *testing.T) {
			result, err := runFixtureCLI(t, tc.backend, tc.fixture, "exit 7", 0)
			if err == nil || result.Status != "failed" || result.Usage == nil {
				t.Fatalf("status=%q usage=%+v err=%v diagnostics=%+v", result.Status, result.Usage, err, result.Diagnostics)
			}
		})
		t.Run(tc.name+"/timeout", func(t *testing.T) {
			result, err := runTimeoutFixtureCLI(t, tc.backend, tc.fixture)
			if err == nil || result.Status != "timeout" || result.Usage == nil {
				t.Fatalf("status=%q usage=%+v err=%v diagnostics=%+v", result.Status, result.Usage, err, result.Diagnostics)
			}
		})
	}
}

func runTimeoutFixtureCLI(t *testing.T, backend func(string) Backend, fixture string) (RunResult, error) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "receipt-emitted")
	script := filepath.Join(dir, "fixture-cli")
	contents := "#!/bin/sh\ncommand cat \"$WEAVE_TEST_FIXTURE\"\ntouch \"$WEAVE_TEST_MARKER\"\nsleep 30\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	fixturePath, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := backend(script).Run(ctx, RunSpec{
			WorkDir: dir, Prompt: "fixture",
			Env: map[string]string{
				"WEAVE_TEST_FIXTURE": fixturePath,
				"WEAVE_TEST_MARKER":  marker,
			},
			EngineVersion: "fixture-version",
		})
		done <- outcome{result: result, err: runErr}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, statErr := os.Stat(marker); statErr == nil {
			cancel()
			finished := <-done
			return finished.result, finished.err
		}
		select {
		case <-deadline.C:
			t.Fatal("fixture CLI did not emit its terminal receipt")
		case <-ticker.C:
		}
	}
}

func TestResumeDoesNotCountCLIUsage(t *testing.T) {
	result := RunResult{Usage: &UsageReceipt{HasTokens: true}}
	bindUsageReceipt(&result, RunSpec{ResumeID: "session-resume"})
	if result.Usage != nil || !hasDiagnostic(result.Diagnostics, "usage_resume_disabled") {
		t.Fatalf("resume usage=%+v diagnostics=%+v", result.Usage, result.Diagnostics)
	}
}

func runFixtureCLI(t *testing.T, backend func(string) Backend, fixture, terminalCommand string, timeout time.Duration) (RunResult, error) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fixture-cli")
	contents := "#!/bin/sh\ncommand cat \"$WEAVE_TEST_FIXTURE\"\n" + terminalCommand + "\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	fixturePath, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return backend(script).Run(t.Context(), RunSpec{
		WorkDir: dir,
		Prompt:  "fixture",
		Env: map[string]string{
			"WEAVE_TEST_FIXTURE": fixturePath,
		},
		Timeout:       timeout,
		EngineVersion: "fixture-version",
	})
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertReceipt(t *testing.T, receipt *UsageReceipt, input, output int, cost float64, hasTokens, hasCost bool) {
	t.Helper()
	if receipt.InputTokens != input || receipt.OutputTokens != output ||
		math.Abs(receipt.CostUSD-cost) > 1e-12 || receipt.HasTokens != hasTokens || receipt.HasCost != hasCost ||
		receipt.Source != UsageSourceCLIReported || receipt.Scope != UsageScopeInvocation {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func hasDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, item := range diagnostics {
		if item.Code == code {
			return true
		}
	}
	return false
}

func TestBinaryVersionProbeDoesNotInheritCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixture")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "probe-env")
	binary := filepath.Join(dir, "engine")
	script := "#!/bin/sh\nif [ -n \"$OPENAI_API_KEY$ONEAPI_API_KEY$WEAVE_RUNTIME_TOKEN$HOME$WEAVE_ACTOR_USER_ID\" ]; then exit 17; fi\nprintf '%s' \"$PATH\" > '" + marker + "'\necho 'fixture-cli 1.2.3'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "operator-secret")
	t.Setenv("ONEAPI_API_KEY", "operator-secret")
	t.Setenv("WEAVE_RUNTIME_TOKEN", "runtime-secret")
	t.Setenv("HOME", filepath.Join(dir, "operator-home"))
	t.Setenv("WEAVE_ACTOR_USER_ID", "alice")
	if got := BinaryVersion(t.Context(), binary); got != "fixture-cli 1.2.3" {
		t.Fatalf("version probe inherited credentials or failed: %q", got)
	}
}
