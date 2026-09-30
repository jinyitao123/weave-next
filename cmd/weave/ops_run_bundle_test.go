package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestImportRunRefusesNonLoopbackHostsUnlessDisposable(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true, "db.localhost": true,
		"10.0.0.5": false, "postgres": false, "weave-db.internal": false, "": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestExportAndImportRunRejectMissingArguments(t *testing.T) {
	dir := t.TempDir()
	for name, run := range map[string]func([]string, *bytes.Buffer, *bytes.Buffer) int{
		"export": func(a []string, o, e *bytes.Buffer) int { return runOpsExportRun(a, o, e) },
		"import": func(a []string, o, e *bytes.Buffer) int { return runOpsImportRun(a, o, e) },
	} {
		var stdout, stderr bytes.Buffer
		if code := run(nil, &stdout, &stderr); code != 2 {
			t.Errorf("%s without arguments exited %d, want 2", name, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runOpsImportRun([]string{"--file", filepath.Join(dir, "missing.json")}, &stdout, &stderr); code == 0 {
		t.Error("import of a missing file succeeded")
	}
}
