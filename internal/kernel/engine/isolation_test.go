package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestProcessIsolationRealFiles(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS isolation acceptance")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := ProbeIsolation(ctx, root); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root, "alice")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "spool", "server-config"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "bob"), filepath.Join(owned, "escape")); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "other-host-secret")
	if err := os.WriteFile(external, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd, err := isolatedCommand(ctx, "/bin/sh", []string{"-c", `for name in bob spool server-config; do if cat "$1/$name"; then exit 1; fi; done; if cat "$2/escape"; then exit 1; fi; if cat "$3"; then exit 1; fi; printf owned > "$2/output"`, "probe", root, owned, external}, &ProcessIsolation{SubjectRoot: owned, ProtectedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolation failed: %v %s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(owned, "output")); err != nil || string(data) != "owned" {
		t.Fatalf("own output unavailable: %v", err)
	}
}
