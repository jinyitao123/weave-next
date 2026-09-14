package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestManagedHostSingleUserBindingPersistsAndCannotDowngrade(t *testing.T) {
	root := t.TempDir()
	alice := execution.Subject{WorkspaceID: "shared", UserID: "alice"}
	bob := execution.Subject{WorkspaceID: "shared", UserID: "bob"}
	guard := &subjectGuard{root: root, mode: runtimeprotocol.SubjectIsolationSingleUser}
	if _, _, err := guard.bind(alice); err != nil {
		t.Fatal(err)
	}
	restarted := &subjectGuard{root: root, mode: runtimeprotocol.SubjectIsolationSingleUser}
	if _, _, err := restarted.bind(alice); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.bind(bob); err == nil {
		t.Fatal("second user admitted without isolation")
	}
	restarted.mode = runtimeprotocol.SubjectIsolationStrong
	if _, _, err := restarted.bind(bob); err != nil {
		t.Fatal(err)
	}
	restarted.mode = runtimeprotocol.SubjectIsolationSingleUser
	if _, _, err := restarted.bind(alice); err == nil {
		t.Fatal("isolation downgrade exposed a previously shared Host")
	}
	if err := os.WriteFile(filepath.Join(root, ".subject-bindings.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.bind(alice); err == nil {
		t.Fatal("corrupt binding silently replaced")
	}
}

func TestManagedHostStrongIsolationUsesRealCLIAndScopedCredentials(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS strong isolation acceptance")
	}
	root := t.TempDir()
	guard, err := newSubjectGuard(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if guard.mode != runtimeprotocol.SubjectIsolationStrong {
		t.Fatal("host did not pass actual isolation probe")
	}
	alice := execution.Subject{WorkspaceID: "shared", UserID: "alice"}
	bob := execution.Subject{WorkspaceID: "shared", UserID: "bob"}
	for _, subject := range []execution.Subject{alice, bob} {
		for _, dir := range []string{"home", "config", "credentials", "work", "outputs"} {
			home := filepath.Join(root, ".subjects", subject.Digest(), dir)
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "private"), []byte(subject.UserID), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"host.json", "server-secrets", ".weave-results/receipt.json"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("server-private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The process receives adversarial known paths, not only a clean directory.
	script := filepath.Join(t.TempDir(), "codex-subject-fixture")
	contents := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli fixture'; exit 0; fi
test -z "$SERVER_PRIVATE_API_KEY" || exit 20
test "$ONEAPI_API_KEY" = "$WEAVE_ACTOR_USER_ID-key" || exit 21
test "$(cat "$HOME/private")" = "$WEAVE_ACTOR_USER_ID" || exit 22
if [ "$WEAVE_ACTOR_USER_ID" = alice ]; then foreign='` + filepath.Join(root, ".subjects", bob.Digest()) + `'; else foreign='` + filepath.Join(root, ".subjects", alice.Digest()) + `'; fi
for dir in home config credentials work outputs; do if cat "$foreign/$dir/private"; then exit 23; fi; done
for foreign in '` + filepath.Join(root, "host.json") + `' '` + filepath.Join(root, "server-secrets") + `' '` + filepath.Join(root, ".weave-results", "receipt.json") + `'; do if cat "$foreign"; then exit 24; fi; done
mkdir -p outputs
printf '%s' "$WEAVE_ACTOR_USER_ID" > outputs/report.md
printf '%s\n' '{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"Saved [report](outputs/report.md)."}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":4,"reasoning_output_tokens":0}}'
`
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	t.Setenv("SERVER_PRIVATE_API_KEY", "must-never-inherit")
	record := &registry.AgentRecord{WorkspaceID: "shared", Name: "worker", ID: "worker", Version: 1, Engine: engine.Codex, Model: "fixture"}
	d := &service{workspacesRoot: root, subjectGuard: guard, credentials: func(ctx context.Context, claim *runtimeprotocol.ExecutionClaim) (ProviderCredentials, error) {
		actual, err := execution.RequireSubject(ctx, "shared")
		if err != nil || actual != claim.Subject {
			return ProviderCredentials{}, execution.ErrSubjectMismatch
		}
		return ProviderCredentials{BaseURL: "https://provider.invalid/v1", APIKey: actual.UserID + "-key"}, nil
	}, runEngine: runEngine, engineCapabilities: []runtimeprotocol.EngineCapability{{Engine: engine.Codex, BinaryVersion: "codex-cli fixture"}}}
	for _, subject := range []execution.Subject{alice, bob} {
		payload, _ := json.Marshal(runtimes.EngineExecRequest{Subject: subject, Record: record, Agent: record.Name, Engine: record.Engine, Model: record.Model, Prompt: "Write report"})
		claim := testExecutionClaim(t, &taskqueue.Task{ID: subject.UserID + "-task", WorkspaceID: subject.WorkspaceID, Subject: subject, ClaimEpoch: 1, AgentID: record.ID, AgentVersion: 1, Agent: record.Name, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Payload: payload})
		receipt, err := d.executeTask(t.Context(), claim)
		if err != nil || receipt.Status != "completed" || len(receipt.Artifacts) != 1 || receipt.Artifacts[0].Content != subject.UserID {
			t.Fatalf("subject %s execution: %+v %v", subject.UserID, receipt, err)
		}
		if receipt.UsageReceipt == nil || receipt.UsageReceipt.InputTokens != 10 {
			t.Fatal("physical CLI usage missing")
		}
		if strings.Contains(receipt.Output, "-key") {
			t.Fatal("credential surfaced in output")
		}
	}
}

func TestManagedHostSingleUserBindingSpansWorkspaceHosts(t *testing.T) {
	machine := t.TempDir()
	alice := &subjectGuard{root: t.TempDir(), bindingRoot: machine, mode: runtimeprotocol.SubjectIsolationSingleUser}
	bob := &subjectGuard{root: t.TempDir(), bindingRoot: machine, mode: runtimeprotocol.SubjectIsolationSingleUser}
	if _, _, err := alice.bind(execution.Subject{WorkspaceID: "one", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bob.bind(execution.Subject{WorkspaceID: "two", UserID: "bob"}); err == nil {
		t.Fatal("another workspace bypassed the physical machine's single-user binding")
	}
}

func TestManagedHostDeclinesUnobservedBinaryVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixture")
	}
	script := filepath.Join(t.TempDir(), "unknown-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ENGINE_CODEX_PATH", script)
	t.Setenv("WEAVE_ENGINE_OPENCODE_PATH", "/unavailable")
	t.Setenv("WEAVE_ENGINE_CLAUDE_PATH", "/unavailable")
	host, err := NewManagedHost(t.Context(), HostConfig{Server: "http://127.0.0.1:1", Token: "unused", WorkspacesRoot: t.TempDir(), Concurrency: 1, Credentials: func(context.Context, *runtimeprotocol.ExecutionClaim) (ProviderCredentials, error) {
		t.Fatal("version probe resolved personal credentials")
		return ProviderCredentials{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	caps := host.Capabilities()
	if len(caps) != 1 || caps[0].Availability != runtimeprotocol.EngineAvailabilityUnavailable || caps[0].UnavailableReason != "engine_version_unavailable" {
		t.Fatalf("unobserved CLI was eligible: %+v", caps)
	}
}
