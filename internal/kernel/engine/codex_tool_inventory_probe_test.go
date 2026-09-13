//go:build !windows

package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This opt-in probe observes the installed CLI against a local rejecting
// provider. It does not call a real model or enable a production adapter.
func TestCodexRestrictedProfileStillNeedsToolEnforcement(t *testing.T) {
	if os.Getenv("WEAVE_CODEX_PROTOCOL_TEST") != "1" {
		t.Skip("WEAVE_CODEX_PROTOCOL_TEST is not set")
	}
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	received := make(chan []byte, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		select {
		case received <- raw:
		default:
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"end of protocol probe"}}`))
	}))
	defer provider.Close()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codex-home"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `model_provider="fixture"
model="gpt-5.4-mini"
[model_providers.fixture]
name="fixture"
base_url="` + provider.URL + `/v1"
env_key="WEAVE_TEST_MODEL_KEY"
wire_api="responses"
`
	if err := os.WriteFile(filepath.Join(dir, ".codex-home", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, cli, "-a", "never", "exec", "--json", "--skip-git-repo-check", "--sandbox", "read-only",
		"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "multi_agent", "--disable", "apps", "--disable", "plugins",
		"--disable", "computer_use", "--disable", "image_generation", "--disable", "hooks", "--disable", "skill_mcp_dependency_install",
		"-c", `web_search="disabled"`, "-c", "tools.view_image=false", "-c", "project_doc_max_bytes=0", "-C", dir, "Return 7.")
	cmd.Env = envWithCLIPath(codexEnv(map[string]string{"WEAVE_TEST_MODEL_KEY": "fixture"}, dir), cli)
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case raw := <-received:
		cancel()
		<-done
		var request struct {
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Tools) == 0 {
			t.Fatal("CLI tool inventory changed; reassess the runtime adapter")
		}
		evidence := struct {
			Version           string `json:"cli_version"`
			Tools             any    `json:"advertised_tools"`
			ProductionEnabled bool   `json:"production_enabled"`
		}{BinaryVersion(t.Context(), cli), request.Tools, false}
		t.Logf("restricted profile still advertises %d tools", len(request.Tools))
		if path := os.Getenv("WEAVE_CODEX_TOOL_PROBE_EVIDENCE"); path != "" {
			encoded, _ := json.MarshalIndent(evidence, "", "  ")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
	case err := <-done:
		t.Fatalf("CLI stopped before provider request: %v", err)
	case <-ctx.Done():
		<-done
		t.Fatal("no provider request received")
	}
}
