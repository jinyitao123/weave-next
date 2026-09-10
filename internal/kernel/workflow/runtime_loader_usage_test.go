package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

type runtimeCLIReceiptExecutor struct {
	result engine.RunResult
	err    error
}

func (e runtimeCLIReceiptExecutor) ExecRemote(
	context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp,
	string, []execenv.Attachment,
) (engine.RunResult, error) {
	return e.result, e.err
}

func TestRuntimeCLIEntryPreservesFailedAttemptsAndMissingReceipt(t *testing.T) {
	receipt := &engine.UsageReceipt{
		InputTokens: 31, OutputTokens: 7, CostUSD: 0.4,
		HasTokens: true, HasCost: true, Source: engine.UsageSourceCLIReported,
		Scope: engine.UsageScopeInvocation, EngineVersion: "fixture-cli 1.0",
	}
	record := &registry.AgentRecord{WorkspaceID: "workspace-1", ID: "agent-1", Version: 1}
	entry, err := NewRuntimeCLIEntry(runtimeCLIReceiptExecutor{
		result: engine.RunResult{Attempts: []engine.UsageAttempt{
			{AttemptID: "task-failed", Status: "failed", Usage: receipt, Events: []engine.Event{{Kind: "tool_call", Tool: "shell", CallID: "call-1"}}},
			{AttemptID: "task-timeout", Status: "timeout"},
		}, Events: []engine.Event{{Kind: "tool_call", Tool: "shell", CallID: "call-1"}}},
		err: errors.New("CLI attempts exhausted"),
	}, record, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := entry.ExecuteAccounted(t.Context(), "prompt")
	if execErr == nil {
		t.Fatal("failed CLI execution returned nil error")
	}
	if len(result.Attempts) != 2 || result.Attempts[0].InputTokens != 31 ||
		!result.Attempts[0].HasTokens || !result.Attempts[0].HasCost ||
		result.Attempts[0].Source != engine.UsageSourceCLIReported {
		t.Fatalf("failed receipt attempts = %#v", result.Attempts)
	}
	if result.Attempts[1].HasTokens || result.Attempts[1].HasCost ||
		result.Attempts[1].InputTokens != 0 || result.Attempts[1].CostUSD != 0 {
		t.Fatalf("missing receipt was not preserved as unknown dimensions: %#v", result.Attempts[1])
	}
	if len(result.Attempts[0].Events) != 1 || len(result.Events) != 1 {
		t.Fatalf("observed events were not preserved: %#v", result)
	}
}

func TestRuntimeCLIEntryCarriesDeferredDeliveryEvidence(t *testing.T) {
	entry, err := NewRuntimeCLIEntry(runtimeCLIReceiptExecutor{result: engine.RunResult{
		Status: "completed", Output: "Later deliver `outputs/report.md`.",
		Diagnostics: []engine.Diagnostic{{Code: "delivery_artifact_uncollected", Message: `file_not_collected: "outputs/report.md"`}},
	}}, &registry.AgentRecord{WorkspaceID: "workspace-1", ID: "agent-1", Version: 1}, execution.AgentExecutionStamp{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := entry.ExecuteAccounted(t.Context(), "plan only")
	if err != nil || result.DeliveryError != "" || result.Status != "completed" || len(result.Diagnostics) != 1 || result.Diagnostics[0].Message != `file_not_collected: "outputs/report.md"` {
		t.Fatalf("delivery evidence was lost or failed early: result=%#v err=%v", result, err)
	}
}

func TestRuntimeCLIResultObservedEventsPrefersPhysicalAttempts(t *testing.T) {
	result := RuntimeCLIResult{
		Events: []RuntimeCLIEvent{{Kind: "tool_call", CallID: "aggregate"}},
		Attempts: []RuntimeCLIUsageAttempt{
			{AttemptID: "task-1", Events: []RuntimeCLIEvent{{Kind: "tool_call", CallID: "call-1"}}},
			{AttemptID: "task-2", Events: []RuntimeCLIEvent{{Kind: "tool_result", CallID: "call-2"}}},
		},
	}
	events := result.ObservedEvents(2)
	if len(events) != 2 || events[0].CallID != "task-1:call-1" || events[1].CallID != "task-2:call-2" {
		t.Fatalf("observed events = %#v", events)
	}
	if got := result.ObservedEvents(1); len(got) != 1 || got[0].CallID != "task-1:call-1" {
		t.Fatalf("limited events = %#v", got)
	}
	if got := (RuntimeCLIResult{Events: result.Events}).ObservedEvents(2); len(got) != 1 || got[0].CallID != "aggregate" {
		t.Fatalf("aggregate fallback = %#v", got)
	}
	if got := (RuntimeCLIResult{Attempts: []RuntimeCLIUsageAttempt{{AttemptID: "task-empty"}}, Events: result.Events}).ObservedEvents(2); len(got) != 1 || got[0].CallID != "aggregate" {
		t.Fatalf("empty-attempt fallback = %#v", got)
	}
}

func TestRuntimeCLITextOutputUsesCollectedFileInsteadOfReceipt(t *testing.T) {
	body := "# 已知事实\n内部读书会，30 分钟。\n# 待确认\n负责人。\n# 下一步\n准备讨论材料。"
	for _, answer := range []string{"已生成 `brief.md:1`。", "Open [brief](outputs/brief.md)."} {
		result := RuntimeCLIResult{Output: answer, Artifacts: []RuntimeCLIArtifact{
			{Path: "brief.md", ContentType: "text/markdown", Content: body},
		}}
		got, err := result.TextOutput()
		if err != nil || got != body || result.Output != answer || result.Artifacts[0].Content != body {
			t.Fatalf("text output = %q, error = %v", got, err)
		}
	}
	for _, answer := range []string{"Full inline answer.", "Check `other/brief.md`.", "Check `brief.md.bak`."} {
		result := RuntimeCLIResult{Output: answer, Artifacts: []RuntimeCLIArtifact{{Path: "brief.md", Content: body}}}
		if got, err := result.TextOutput(); err != nil || got != answer {
			t.Fatalf("unreferenced artifact replaced answer: %q, %v", got, err)
		}
	}
	empty := RuntimeCLIResult{Output: "Created `brief.md`.", Artifacts: []RuntimeCLIArtifact{{Path: "brief.md", Content: " "}}}
	if output, err := empty.TextOutput(); err != nil || output != empty.Output {
		t.Fatal("empty file rewrote the execution outcome instead of leaving it for delivery verification")
	}
	multiple := RuntimeCLIResult{Output: "See `brief.md` and `facts.csv`.", Artifacts: []RuntimeCLIArtifact{
		{Path: "brief.md", Content: body}, {Path: "facts.csv", Content: "fact\nmeeting\n"},
	}}
	if got, err := multiple.TextOutput(); err != nil || got != multiple.Output {
		t.Fatalf("ambiguous main file was guessed: %q, %v", got, err)
	}
}

func TestRuntimeCLITextOutputResolvesOnlyCollectedCurrentAbsoluteReferences(t *testing.T) {
	for _, location := range []string{"brief.md", "outputs/brief.md", "other/brief.md"} {
		t.Run(location, func(t *testing.T) {
			workDir := t.TempDir()
			before := runtimes.SnapshotOutputArtifacts(workDir)
			name := filepath.Join(workDir, location)
			if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				t.Fatal(err)
			}
			body := "# Brief\nThe actual current result.\n"
			if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			answer := "Saved [brief](" + filepath.ToSlash(name) + ":1)."
			runResult := engine.RunResult{Output: answer, Status: "completed"}
			runtimes.CollectRunOutputArtifacts(workDir, before, &runResult)
			if location == "other/brief.md" {
				if runResult.Status != "completed" || runResult.Err != "" || len(runResult.Artifacts) != 0 || len(runResult.Diagnostics) != 1 || runResult.ArtifactCollection == nil || len(runResult.ArtifactCollection.Issues) != 1 {
					t.Fatalf("out-of-scope file became a receipt-only success: %#v", runResult)
				}
				return
			}
			entry, err := NewRuntimeCLIEntry(runtimeCLIReceiptExecutor{result: runResult}, &registry.AgentRecord{}, execution.AgentExecutionStamp{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := entry.ExecuteAccounted(t.Context(), "Write the brief")
			if err != nil {
				t.Fatal(err)
			}
			got, err := result.TextOutput()
			want := body
			if err != nil || got != want {
				t.Fatalf("text delivery = %q, want %q, error = %v", got, want, err)
			}
		})
	}
}

func TestRuntimeCLIEntryPreservesEngineIdentityAndReturnsTechnicalCollectionError(t *testing.T) {
	for _, kind := range []string{"limit", "error"} {
		original := engine.RunResult{Status: "completed", SessionID: "session-original", Output: "Saved report", Usage: &engine.UsageReceipt{InputTokens: 7, HasTokens: true}, ArtifactCollection: &fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: false, Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 262144, MaxTotalBytes: 524288}, Issues: []fileartifact.CollectionIssue{{Path: "report.md", Reason: "fixture_reason", Kind: kind, Claimed: true}}}}
		encoded, _ := json.Marshal(runtimes.CLIEngineExecResult(original))
		var wire runtimes.EngineExecResult
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		entry, err := NewRuntimeCLIEntry(runtimeCLIReceiptExecutor{result: wire.EngineRunResult()}, &registry.AgentRecord{}, execution.AgentExecutionStamp{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := entry.ExecuteAccounted(t.Context(), "collect")
		if (err != nil) != (kind == "error") || result.Status != "completed" || result.Err != "" || result.SessionID != "session-original" || result.DeliveryError != "" || len(result.Attempts) != 1 || result.Attempts[0].InputTokens != 7 || result.ArtifactCollection == nil || result.ArtifactCollection.Issues[0].Kind != kind {
			t.Fatalf("independent execution and collection facts lost: %#v %v", result, err)
		}
	}
}
