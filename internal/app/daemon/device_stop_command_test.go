package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDeviceStopCommandQuotesTheInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Mac notification uses POSIX shell quoting; Windows has its tray stop action")
	}
	dir := filepath.Join(t.TempDir(), "device node's bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "weave-runtime")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = device ] && [ \"$2\" = stop ] || exit 1\nprintf stopped > \"$WEAVE_DEVICE_STOP_TEST_RESULT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(t.TempDir(), "stopped")
	cmd := exec.Command("sh", "-c", deviceStopCommand(binary))
	cmd.Env = append(os.Environ(), "WEAVE_DEVICE_STOP_TEST_RESULT="+result)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installed stop command failed: %v %s", err, out)
	}
	if data, err := os.ReadFile(result); err != nil || string(data) != "stopped" {
		t.Fatalf("the quoted installed command did not execute with device stop: %q, %v", data, err)
	}
}
