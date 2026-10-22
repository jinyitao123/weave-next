package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeviceInstallDryRunDoesNotEnrollOrTouchTheMachine(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{
		"uname": "#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo arm64 ;; *) exit 1 ;; esac\n",
		"curl":  "#!/bin/sh\nprintf 'curl was called\\n' >> \"$WEAVE_INSTALL_TEST_CALLS\"\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(t.TempDir(), "network-calls")
	cmd := exec.Command("sh", "install_assets/install.sh", "--server", "http://install.example.invalid", "--device-code", "not-a-real-code", "--dry-run")
	if runtime.GOOS == "windows" {
		// Git sh needs a POSIX PATH entry to find the extensionless fixture
		// scripts before its own uname.exe/curl.exe. Pass paths as arguments.
		cmd = exec.Command("sh", "-c", `fixture_bin=$(cygpath -u "$1") || exit; shift; PATH="$fixture_bin:$PATH"; export PATH; exec sh "$@"`, "install-dry-run", bin, "install_assets/install.sh", "--server", "http://install.example.invalid", "--device-code", "not-a-real-code", "--dry-run")
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "WEAVE_INSTALL_TEST_CALLS="+calls)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Dry run:") {
		t.Fatalf("dry run: %v, %s", err, out)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("dry run contacted the server: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".weave")); !os.IsNotExist(err) {
		t.Fatalf("dry run changed the machine: %v", err)
	}
}
