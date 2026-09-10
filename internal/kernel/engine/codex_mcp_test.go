//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexTaskMCPDisablesAmbientServersWithoutTokenArguments(t *testing.T) {
	work := t.TempDir()
	cli := filepath.Join(work, "fixture-codex")
	script := `#!/bin/sh
if [ "$1" != "mcp" ] || [ "$2" != "list" ]; then exit 3; fi
if [ -n "$WEAVE_RUNTIME_TOKEN" ] || [ -n "$WEAVE_SECRET_KEY" ] || [ -n "$WEAVE_MCP_BOUNDARY_TOKEN_99" ]; then exit 4; fi
printf '%s' '[{"name":"host_tools","enabled":true},{"name":"previous_task","enabled":false}]'
`
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_RUNTIME_TOKEN", "PRIVATE_RUNTIME")
	t.Setenv("WEAVE_SECRET_KEY", "PRIVATE_KEY")
	t.Setenv("WEAVE_MCP_BOUNDARY_TOKEN_99", "PRIVATE_OLD_TASK")
	spec := RunSpec{WorkDir: work, Env: map[string]string{"WEAVE_CODEX_AUTH_MODE": "chatgpt", "WEAVE_MCP_BOUNDARY_TOKEN_0": "CURRENT_TASK_TOKEN"}, MCPServers: []MCPServerEndpoint{{URL: "https://server.example/v1/runtime/tasks/task/mcp/0", TokenEnv: "WEAVE_MCP_BOUNDARY_TOKEN_0"}}}
	args, err := codexTaskMCPArgs(t.Context(), cli, spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, part := range []string{"mcp_servers.host_tools.enabled=false", "mcp_servers.previous_task.enabled=false", "enabled=true", "/v1/runtime/tasks/task/mcp/0", "bearer_token_env_var=\"WEAVE_MCP_BOUNDARY_TOKEN_0\""} {
		if !strings.Contains(joined, part) {
			t.Fatalf("missing task config %q", part)
		}
	}
	if strings.Contains(joined, "CURRENT_TASK_TOKEN") || strings.Contains(joined, "PRIVATE_") {
		t.Fatal("credential entered command arguments")
	}
	env := strings.Join(envWithCLIPath(codexEnv(spec.Env, work), cli), "\n")
	if strings.Contains(env, "PRIVATE_") || !strings.Contains(env, "WEAVE_MCP_BOUNDARY_TOKEN_0=CURRENT_TASK_TOKEN") {
		t.Fatal("child environment authority is incorrect")
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := codexTaskMCPArgs(t.Context(), cli, spec); err == nil {
		t.Fatal("failed config probe allowed task execution")
	}
}
