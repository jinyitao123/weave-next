package execenv

import (
	"fmt"
	"strconv"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// WriteEngineConfig writes the runtime-specific configuration for the named
// CLI engine into workDir. Each engine owns exactly one case so adapters can
// land independently; an unbuilt engine fails loudly instead of running
// half-configured.
func WriteEngineConfig(engineName, workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvValue string) error {
	return WriteEngineConfigWithAuthMode(engineName, workDir, rec, oneapiBase, boundaryBase, apiKeyEnvValue, "")
}

func WriteEngineConfigWithAuthMode(engineName, workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvValue, authMode string) error {
	switch engineName {
	case "opencode":
		return WriteOpenCodeConfig(workDir, rec, oneapiBase, boundaryBase, apiKeyEnvValue)
	case "codex":
		return WriteCodexHomeWithAuthMode(workDir, rec, oneapiBase, boundaryBase, apiKeyEnvValue, authMode)
	case "claude":
		return WriteClaudeConfig(workDir, rec, boundaryBase)
	default:
		return fmt.Errorf("execenv: unknown engine %q", engineName)
	}
}

func mcpServerTarget(rec *registry.AgentRecord, server registry.MCPServerConfig, idx int, boundaryBase string) (targetURL, token, host string, ok bool) {
	if boundaryBase == "" {
		return "", "", "", false
	}
	// Legacy inline records still receive the existing remote-URL validation,
	// while registry refs intentionally carry no upstream URL in AgentRecord.
	if server.ServerID == "" {
		if _, allowed := allowedRemoteURL(server.URL); !allowed {
			return "", "", "", false
		}
	}
	workspace := rec.WorkspaceID
	if workspace == "" {
		workspace = "default"
	}
	token = secret.BoundaryToken(workspace, rec.Name, idx)
	// A missing token deliberately leaves the boundary unauthenticated instead
	// of falling back to the upstream URL; the endpoint will reject it closed.
	if rec.Name == "" {
		return "", "", "", false
	}
	return boundaryBase + "/v1/mcp-boundary/" + workspace + "/" + rec.Name + "/" + strconv.Itoa(idx), token, "mcp", true
}
