package runtimes

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestLocalExecutorPreservesEngineReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	fixture, err := filepath.Abs(filepath.Join("..", "engine", "testdata", "codex-0.144.5.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex-fixture")
	contents := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.144.5'; exit 0; fi\ncommand cat \"$WEAVE_TEST_FIXTURE\"\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	t.Setenv("WEAVE_TEST_FIXTURE", fixture)
	record := &registry.AgentRecord{
		Name: "worker", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1,
		Engine: engine.Codex,
	}
	result, err := NewLocalExecutor(t.TempDir(), "", "", "").ExecRemote(
		t.Context(), record.WorkspaceID, record,
		execution.AgentExecutionStamp{
			AgentID: record.ID, AgentVersion: record.Version,
			ExecutionScope: execution.ScopeLegacyOrchestrator,
		},
		"fixture", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "OK" || result.Usage == nil || result.Usage.InputTokens != 14058 ||
		result.Usage.EngineVersion != "codex-cli 0.144.5" {
		t.Fatalf("result=%+v", result)
	}
}
