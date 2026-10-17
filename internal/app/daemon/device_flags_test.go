package daemon

import (
	"path/filepath"
	"testing"
)

func TestDeviceFlagRunsWithoutEnginesInItsOwnWorkspaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg, err := parseFlags([]string{"--server", "https://weave.example", "--runtime-token", "rtk_x", "--device", "--cua-driver", "/Applications/CuaDriver.app/Contents/MacOS/cua-driver"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.device || cfg.cuaDriver != "/Applications/CuaDriver.app/Contents/MacOS/cua-driver" {
		t.Fatalf("device config = %+v", cfg)
	}
	if cfg.workspacesRoot != filepath.Join(home, ".weave", "device-workspaces") {
		t.Fatalf("workspaces root = %q", cfg.workspacesRoot)
	}
	// An empty, non-nil list means "detected nothing": no team work is routed here.
	if cfg.detectedEngines == nil || len(cfg.detectedEngines) != 0 || len(cfg.engineCapabilities) != 0 {
		t.Fatalf("engines = %v %v", cfg.detectedEngines, cfg.engineCapabilities)
	}

	explicit, err := parseFlags([]string{"--server", "https://weave.example", "--runtime-token", "rtk_x", "--device", "--workspaces-root", filepath.Join(home, "elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.workspacesRoot != filepath.Join(home, "elsewhere") {
		t.Fatalf("explicit workspaces root = %q", explicit.workspacesRoot)
	}

	runtime, err := parseFlags([]string{"--server", "https://weave.example", "--runtime-token", "rtk_x"})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.device || runtime.detectedEngines != nil {
		t.Fatalf("a team runtime must still detect its engines: %+v", runtime)
	}
}
