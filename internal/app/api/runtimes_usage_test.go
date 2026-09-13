package api

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestValidateRuntimeEngineExecResultBindsReceiptToAdmittedVersion(t *testing.T) {
	payload, err := json.Marshal(runtimes.EngineExecRequest{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"},
		Engine: engine.Codex, EngineVersion: "codex-cli 0.144.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{WorkspaceID: "workspace-1", Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, ID: "task-1", RuntimeID: "runtime-1", Payload: payload}
	valid := runtimes.EngineExecResult{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"},
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

func TestValidateRuntimeEngineExecResultRequiresVersionedTerminalStatus(t *testing.T) {
	payload, err := json.Marshal(runtimes.EngineExecRequest{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Engine: engine.Claude})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeEngineExecResult(&taskqueue.Task{WorkspaceID: "workspace-1", Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Payload: payload}, runtimes.EngineExecResult{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Output: "ok"}); err == nil {
		t.Fatal("result without terminal status accepted")
	}
}

func TestValidateRuntimeCollectionReceiptRejectsMalformedEvidence(t *testing.T) {
	payload, _ := json.Marshal(runtimes.EngineExecRequest{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Engine: engine.Codex})
	task := &taskqueue.Task{WorkspaceID: "workspace-1", Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Payload: payload}
	evidence := fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: false, Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 262144, MaxTotalBytes: 524288}, Issues: []fileartifact.CollectionIssue{{Path: "report.pdf", Reason: "unsupported_file_type", Kind: "limit", Claimed: true}}}
	result := runtimes.EngineExecResult{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Status: "completed", SessionID: "session-original", ArtifactCollection: &evidence}
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
	payload, _ = json.Marshal(runtimes.EngineExecRequest{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, Engine: runtimes.EngineLoom})
	task.Payload = payload
	if err := validateRuntimeEngineExecResult(task, runtimes.EngineExecResult{Subject: execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}, ArtifactCollection: &evidence}); err == nil {
		t.Fatal("CLI collection evidence admitted on Loom engine carrier")
	}
}
