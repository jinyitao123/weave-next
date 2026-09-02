package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestExecuteTaskPreservesFailedEngineReceiptAndArtifacts(t *testing.T) {
	record := &registry.AgentRecord{
		Name: "worker", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1,
		Engine: engine.OpenCode, Model: "deepseek/deepseek-chat",
	}
	payload, err := json.Marshal(runtimes.EngineExecRequest{
		Agent: record.Name, Engine: record.Engine, Model: record.Model, Prompt: "fixture", Record: record,
		EngineVersion: "opencode 1.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &taskqueue.Task{
		ID: "task-1", WorkspaceID: record.WorkspaceID, Agent: record.Name,
		AgentID: record.ID, AgentVersion: record.Version,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2,
		ExecutionScope: execution.ScopeLegacyOrchestrator, Payload: payload,
	}
	d := &service{
		workspacesRoot:     t.TempDir(),
		engineCapabilities: []runtimes.EngineCapability{{Engine: engine.OpenCode, BinaryVersion: "opencode 1.2.10"}},
		runEngine: func(_ context.Context, _ string, spec engine.RunSpec) (engine.RunResult, error) {
			if spec.EngineVersion != "opencode 1.2.10" {
				t.Fatalf("engine version=%q", spec.EngineVersion)
			}
			outputsDir := filepath.Join(spec.WorkDir, "outputs")
			if err := os.MkdirAll(outputsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outputsDir, "partial.jsonl"), []byte("{\"status\":\"partial\"}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return engine.RunResult{
				Status: "failed", Err: "provider timeout",
				Usage: &engine.UsageReceipt{
					InputTokens: 8, OutputTokens: 2, HasTokens: true,
					Source: engine.UsageSourceCLIReported, Scope: engine.UsageScopeInvocation,
					EngineVersion: spec.EngineVersion,
				},
			}, errors.New("opencode: provider timeout")
		},
	}
	result, runErr := d.executeTask(context.Background(), task)
	if runErr == nil || result.Status != "failed" || result.UsageReceipt == nil || result.UsageReceipt.InputTokens != 8 {
		t.Fatalf("result=%+v err=%v", result, runErr)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "partial.jsonl" || result.Artifacts[0].ContentType != "application/x-ndjson" {
		t.Fatalf("failed result artifacts=%+v", result.Artifacts)
	}
}
