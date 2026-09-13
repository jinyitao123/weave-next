package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

var codexMCPConfigName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Codex merges -c maps with host configuration. Enumerate the effective names
// with the same working directory and environment, then explicitly disable
// them for this process before adding the task gateways. No host file changes
// or login-material copies are needed under native ChatGPT authentication.
func codexTaskMCPArgs(ctx context.Context, cliPath string, spec RunSpec) ([]string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, cliPath, "mcp", "list", "--json")
	cmd.Dir = spec.WorkDir
	cmd.Env = envWithCLIPath(codexEnv(spec.Env, spec.WorkDir), cliPath)
	raw, err := cmd.Output()
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("codex: cannot inspect effective MCP configuration")
	}
	var configured []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &configured) != nil || len(configured) > 1024 {
		return nil, fmt.Errorf("codex: invalid effective MCP configuration")
	}
	args := []string{}
	seen := map[string]bool{}
	for _, server := range configured {
		if !codexMCPConfigName.MatchString(server.Name) {
			return nil, fmt.Errorf("codex: unsupported MCP configuration name")
		}
		seen[server.Name] = true
		args = append(args, "-c", "mcp_servers."+server.Name+".enabled=false")
	}
	for index, server := range spec.MCPServers {
		digest := sha256.Sum256([]byte(server.URL))
		name := fmt.Sprintf("weave_task_%x_%d", digest[:8], index)
		if seen[name] {
			return nil, fmt.Errorf("codex: task MCP configuration name collision")
		}
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s={enabled=true,url=%q,bearer_token_env_var=%q}", name, server.URL, server.TokenEnv))
	}
	return args, nil
}
