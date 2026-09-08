package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestRepairLegacyUsageSummaryOnlyRepairsPartialUTF8Tail(t *testing.T) {
	for _, unit := range []string{"界", "🌍"} {
		for offset := 0; offset < 4; offset++ {
			original := strings.Repeat("x", offset) + strings.Repeat(unit, 4096)
			legacy := engine.UsageReceipt{RawSummary: original[:4096], InputTokens: 123, HasTokens: true}
			wire, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			var decoded engine.UsageReceipt
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			repairLegacyUsageSummary(&decoded)
			if len(decoded.RawSummary) > 4096 || !strings.HasPrefix(original, decoded.RawSummary) || decoded.InputTokens != 123 {
				t.Fatalf("legacy receipt not repaired safely: bytes=%d", len(decoded.RawSummary))
			}
		}
	}
	for _, raw := range []string{strings.Repeat("x", 4097), strings.Repeat("x", 5000) + "\uFFFD", "ok\uFFFD"} {
		receipt := &engine.UsageReceipt{RawSummary: raw}
		repairLegacyUsageSummary(receipt)
		if receipt.RawSummary != raw {
			t.Fatal("unrelated summary modified")
		}
	}
}

func TestValidateRuntimeEngineExecResultBindsReceiptToAdmittedVersion(t *testing.T) {
	payload, err := json.Marshal(runtimes.EngineExecRequest{
		Engine: engine.Codex, EngineVersion: "codex-cli 0.144.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{ID: "task-1", RuntimeID: "runtime-1", Payload: payload}
	valid := runtimes.EngineExecResult{
		Status: "completed",
		UsageReceipt: &engine.UsageReceipt{
			InputTokens: 1, OutputTokens: 0, HasTokens: true,
			Source: engine.UsageSourceCLIReported, Scope: engine.UsageScopeInvocation,
			EngineVersion: "codex-cli 0.144.5",
		},
		Diagnostics: []engine.Diagnostic{{Code: "usage_cost_unreported", Message: "cost absent"}},
	}
	if err := validateRuntimeEngineExecResult(task, valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}

	wrongVersion := valid
	copy := *valid.UsageReceipt
	copy.EngineVersion = "codex-cli 0.143.0"
	wrongVersion.UsageReceipt = &copy
	if err := validateRuntimeEngineExecResult(task, wrongVersion); err == nil {
		t.Fatal("wrong binary version accepted")
	}

	wrongSource := valid
	copy = *valid.UsageReceipt
	copy.Source = "provider-estimated"
	wrongSource.UsageReceipt = &copy
	if err := validateRuntimeEngineExecResult(task, wrongSource); err == nil {
		t.Fatal("wrong source accepted")
	}
}

func TestValidateRuntimeEngineExecResultKeepsLegacyNoReceiptCompatible(t *testing.T) {
	payload, err := json.Marshal(runtimes.EngineExecRequest{Engine: engine.Claude})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeEngineExecResult(&taskqueue.Task{Payload: payload}, runtimes.EngineExecResult{Output: "ok"}); err != nil {
		t.Fatalf("legacy result rejected: %v", err)
	}
}

func TestValidateRuntimeCollectionReceiptRejectsMalformedEvidence(t *testing.T) {
	payload, _ := json.Marshal(runtimes.EngineExecRequest{Engine: engine.Codex})
	task := &taskqueue.Task{Payload: payload}
	evidence := fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: false, Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 262144, MaxTotalBytes: 524288}, Issues: []fileartifact.CollectionIssue{{Path: "report.pdf", Reason: "unsupported_file_type", Kind: "limit", Claimed: true}}}
	result := runtimes.EngineExecResult{Status: "completed", SessionID: "session-original", ArtifactCollection: &evidence}
	if err := validateRuntimeEngineExecResult(task, result); err != nil {
		t.Fatalf("valid independent collection evidence rejected: %v", err)
	}
	for _, mutate := range []func(*fileartifact.CollectionEvidence){
		func(e *fileartifact.CollectionEvidence) { e.SchemaVersion = 2 },
		func(e *fileartifact.CollectionEvidence) { e.Complete = true },
		func(e *fileartifact.CollectionEvidence) { e.Limits.MaxFiles = 129 },
		func(e *fileartifact.CollectionEvidence) {
			e.Issues = []fileartifact.CollectionIssue{{Path: "/private/host", Reason: "unreadable", Kind: "error"}}
		},
		func(e *fileartifact.CollectionEvidence) {
			e.Issues = []fileartifact.CollectionIssue{{Reason: "invented_success", Kind: "passed"}}
		},
	} {
		copy := evidence
		mutate(&copy)
		result.ArtifactCollection = &copy
		if err := validateRuntimeEngineExecResult(task, result); err == nil {
			t.Fatalf("malformed evidence admitted: %#v", copy)
		}
	}
	payload, _ = json.Marshal(runtimes.EngineExecRequest{Engine: runtimes.EngineLoom})
	task.Payload = payload
	if err := validateRuntimeEngineExecResult(task, runtimes.EngineExecResult{ArtifactCollection: &evidence}); err == nil {
		t.Fatal("CLI collection evidence admitted on Loom engine carrier")
	}
}
