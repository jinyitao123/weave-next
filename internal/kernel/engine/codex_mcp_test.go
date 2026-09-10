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

func TestCodexDenyAllRemovesAmbientToolsAndPassesOutputSchema(t *testing.T) {
	work := t.TempDir()
	cli := filepath.Join(work, "fixture-codex")
	script := `#!/bin/sh
if [ "$1" = "mcp" ]; then printf '%s' '[{"name":"ambient","enabled":true}]'; exit 0; fi
printf '%s\n' "$@" > "$PWD/args"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{}"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b := codexBackend{cliPath: cli}
	_, err := b.Run(t.Context(), RunSpec{WorkDir: work, Prompt: "choose", DisableTools: true, OutputSchema: []byte(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(work, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"mcp_servers.ambient.enabled=false", "shell_tool", "unified_exec", "multi_agent", "apps", "plugins", "skip_host_skill_discovery", "web_search=\"disabled\"", "--output-schema"} {
		if !strings.Contains(string(raw), part) {
			t.Fatalf("missing deny-all control %s", part)
		}
	}
	files, _ := filepath.Glob(filepath.Join(work, ".weave-output-schema-*"))
	if len(files) != 0 {
		t.Fatal("temporary schema not cleaned")
	}
	if _, err := b.Run(t.Context(), RunSpec{DisableTools: true, MCPServers: []MCPServerEndpoint{{URL: "https://example.invalid"}}}); err == nil {
		t.Fatal("deny-all accepted tool binding")
	}
}
