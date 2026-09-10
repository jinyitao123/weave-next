//go:build !windows

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeProcessSignalIsNotHiddenByEarlierStderr(t *testing.T) {
	for _, test := range []struct {
		name, script string
		interrupted  bool
	}{
		{"signal after unrelated warning", "echo 'earlier model alias warning' >&2\nkill -KILL $$\n", true},
		{"signal words in ordinary error", "echo 'signal: killed' >&2\nexit 1\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := t.TempDir()
			cli := filepath.Join(work, "fixture-cli")
			if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+test.script), 0o700); err != nil {
				t.Fatal(err)
			}
			result, err := (&claudeBackend{cliPath: cli}).Run(t.Context(), RunSpec{WorkDir: work, Timeout: 5 * time.Second})
			if err == nil || result.Status != "failed" {
				t.Fatalf("status=%q err=%v", result.Status, err)
			}
			if got := strings.HasPrefix(result.Err, "runtime_process_interrupted:"); got != test.interrupted {
				t.Fatalf("interrupted=%v, error=%q", got, result.Err)
			}
			if result.RetrySafeBeforeExecution {
				t.Fatal("process interruption must not authorize automatic replay")
			}
		})
	}
}

func TestClaudeExplicitCancellationRetainsCancellationAuthority(t *testing.T) {
	work := t.TempDir()
	cli := filepath.Join(work, "fixture-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	result, err := (&claudeBackend{cliPath: cli}).Run(ctx, RunSpec{WorkDir: work, Timeout: 5 * time.Second})
	if !errors.Is(err, context.Canceled) || strings.Contains(result.Err, "runtime_process_interrupted:") {
		t.Fatalf("explicit stop lost its authority: result=%q err=%v", result.Err, err)
	}
}
