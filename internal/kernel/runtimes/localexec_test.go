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
		execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: record.WorkspaceID, UserID: "user-1"}), record.WorkspaceID, record,
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

func TestLocalExecutorCollectsTheActualReferencedReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	script := filepath.Join(t.TempDir(), "codex-report-fixture")
	contents := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli fixture'; exit 0; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then shift; cd "$1" || exit 1; fi
  shift
done
printf '# Brief\nActual saved result.\n' > brief.md
printf '{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"Saved [brief](%s/brief.md:1)."}}\n' "$PWD"
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":4,"reasoning_output_tokens":0}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	record := &registry.AgentRecord{Name: "writer", ID: "writer-1", WorkspaceID: "workspace-1", Version: 1, Engine: engine.Codex}
	result, err := NewLocalExecutor(t.TempDir(), "", "", "").ExecRemote(execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: record.WorkspaceID, UserID: "user-1"}), record.WorkspaceID, record,
		execution.AgentExecutionStamp{AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeLegacyOrchestrator},
		"Write the report", nil)
	if err != nil || result.Status != "completed" || len(result.Artifacts) != 1 {
		t.Fatalf("report invocation = %#v, error = %v", result, err)
	}
	if result.Artifacts[0].Path != "brief.md" || result.Artifacts[0].Content != "# Brief\nActual saved result.\n" {
		t.Fatalf("report content was replaced by a receipt: %#v", result.Artifacts)
	}
	if result.Output != "Saved [brief](brief.md:1)." {
		t.Fatalf("absolute delivery reference was not normalized: %q", result.Output)
	}
}

func TestLocalExecutorPreservesCompletedReceiptAndCollectionGap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	script := filepath.Join(t.TempDir(), "codex-unsaved-fixture")
	contents := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli fixture'; exit 0; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then shift; cd "$1" || exit 1; fi
  shift
done
mkdir -p outputs
printf 'Already completed work' > outputs/partial.txt
printf 'unsupported document format' > brief.pdf
printf '{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"Saved [brief](%s/brief.pdf)."}}\n' "$PWD"
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":4,"reasoning_output_tokens":0}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	record := &registry.AgentRecord{Name: "writer", ID: "writer-1", WorkspaceID: "workspace-1", Version: 1, Engine: engine.Codex}
	result, err := NewLocalExecutor(t.TempDir(), "", "", "").ExecRemote(execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: record.WorkspaceID, UserID: "user-1"}), record.WorkspaceID, record,
		execution.AgentExecutionStamp{AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeLegacyOrchestrator},
		"Write the report", nil)
	if err != nil || result.Status != "completed" || result.Err != "" || !collectionHasIssue(result.ArtifactCollection, "unsupported_file_type", true) {
		t.Fatalf("receipt-only invocation accepted: status=%q error=%v", result.Status, err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "partial.txt" || result.Artifacts[0].Content != "Already completed work" || result.Usage == nil {
		t.Fatalf("partial content or observed usage lost: %#v", result)
	}
}
