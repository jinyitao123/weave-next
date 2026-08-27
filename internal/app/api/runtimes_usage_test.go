package api

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

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
