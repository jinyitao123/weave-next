package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"context"
	"encoding/json"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimebridge"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestManagedHostPreservesEngineReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "kernel", "engine", "testdata", "codex-0.144.5.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex-fixture")
	fixtureData, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	contents := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.144.5'; exit 0; fi\ncat <<'FIXTURE'\n" + strings.TrimSpace(string(fixtureData)) + "\nFIXTURE\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	record := &registry.AgentRecord{
		Name: "worker", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1,
		Engine: engine.Codex,
	}
	result, err := runManagedFixture(t, record, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "OK" || result.Usage == nil || result.Usage.InputTokens != 14058 ||
		result.Usage.EngineVersion != "codex-cli 0.144.5" {
		t.Fatalf("result=%+v", result)
	}
}

func TestManagedHostCollectsTheActualReferencedReport(t *testing.T) {
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
	result, err := runManagedFixture(t, record, "Write the report")
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

func TestManagedHostPreservesCompletedReceiptAndCollectionGap(t *testing.T) {
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
	result, err := runManagedFixture(t, record, "Write the report")
	if err != nil || result.Status != "completed" || result.Err != "" || !managedCollectionHasIssue(result.ArtifactCollection, "unsupported_file_type") {
		t.Fatalf("receipt-only invocation accepted: status=%q error=%v", result.Status, err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "partial.txt" || result.Artifacts[0].Content != "Already completed work" || result.Usage == nil {
		t.Fatalf("partial content or observed usage lost: %#v", result)
	}
}

func runManagedFixture(t *testing.T, record *registry.AgentRecord, prompt string) (engine.RunResult, error) {
	t.Helper()
	subject := execution.Subject{WorkspaceID: record.WorkspaceID, UserID: "user-1"}
	payload, _ := json.Marshal(runtimes.EngineExecRequest{Subject: subject, Record: record, Agent: record.Name, Engine: record.Engine, Prompt: prompt})
	task := testExecutionClaim(t, &taskqueue.Task{ID: "task-fixture", WorkspaceID: record.WorkspaceID, Subject: subject, ClaimEpoch: 1, AgentID: record.ID, AgentVersion: record.Version, Agent: record.Name, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Payload: payload})
	root := t.TempDir()
	guard, err := newSubjectGuard(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	d := &service{workspacesRoot: root, subjectGuard: guard, credentials: func(context.Context, *runtimeprotocol.ExecutionClaim) (ProviderCredentials, error) {
		return ProviderCredentials{BaseURL: "https://provider.invalid/v1", APIKey: "subject-fixture"}, nil
	}, runEngine: runEngine, engineCapabilities: []runtimeprotocol.EngineCapability{{Engine: engine.Codex, BinaryVersion: engine.BinaryVersion(t.Context(), os.Getenv("WEAVE_ENGINE_CODEX_PATH"))}}}
	receipt, err := d.executeTask(t.Context(), task)
	return runtimebridge.Result(receipt).EngineRunResult(), err
}
func managedCollectionHasIssue(evidence *fileartifact.CollectionEvidence, reason string) bool {
	if evidence == nil {
		return false
	}
	for _, issue := range evidence.Issues {
		if issue.Reason == reason && issue.Claimed {
			return true
		}
	}
	return false
}
