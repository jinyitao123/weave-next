package execenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// WriteClaudeConfig mirrors the runtime-neutral instructions to the filename
// Claude reads and writes its HTTP MCP server configuration.
func WriteClaudeConfig(workDir string, rec *registry.AgentRecord, boundaryBase string, targets ...TaskMCPTarget) error {
	if rec == nil {
		return fmt.Errorf("execenv: nil agent record")
	}

	agentsPath := filepath.Join(workDir, "AGENTS.md")
	instructions, err := os.ReadFile(agentsPath)
	switch {
	case err == nil:
		if err := writeFileIfChanged(filepath.Join(workDir, "CLAUDE.md"), instructions, 0o644); err != nil {
			return err
		}
	case os.IsNotExist(err):
	case err != nil:
		return fmt.Errorf("execenv: read %s: %w", agentsPath, err)
	}

	servers := make(map[string]any)
	for idx, server := range rec.MCPServers {
		targetURL, token, host, ok := mcpServerTarget(rec, server, idx, boundaryBase, targets...)
		if !ok {
			continue
		}
		name := uniqueMCPName(servers, host)
		entry := map[string]any{
			"type": "http",
			"url":  targetURL,
		}
		if token != "" {
			entry["headers"] = map[string]string{"Authorization": "Bearer " + token}
		}
		servers[name] = entry
	}
	data, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return fmt.Errorf("execenv: encode claude MCP config: %w", err)
	}
	data = append(data, '\n')
	return writeFileIfChanged(filepath.Join(workDir, ".weave-mcp.json"), data, 0o600)
}
