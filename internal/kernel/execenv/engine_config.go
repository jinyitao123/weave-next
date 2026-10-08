package execenv

import (
	"fmt"
	"strconv"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

func WriteEngineConfigWithAuthMode(engineName, workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvValue, authMode string, targets ...TaskMCPTarget) error {
	if len(targets) > 0 {
		copy := *rec
		copy.MCPServers = make([]registry.MCPServerConfig, len(targets))
		rec = &copy
	}
	switch engineName {
	case "opencode":
		return WriteOpenCodeConfig(workDir, rec, oneapiBase, boundaryBase, apiKeyEnvValue, targets...)
	case "codex":
		return WriteCodexHomeWithAuthMode(workDir, rec, oneapiBase, boundaryBase, apiKeyEnvValue, authMode, targets...)
	case "claude":
		return WriteClaudeConfig(workDir, rec, boundaryBase, targets...)
	default:
		return fmt.Errorf("execenv: unknown engine %q", engineName)
	}
}

func mcpServerTarget(rec *registry.AgentRecord, server registry.MCPServerConfig, idx int, boundaryBase string, targets ...TaskMCPTarget) (targetURL, token, host string, ok bool) {
	if len(targets) > 0 {
		if idx < 0 || idx >= len(targets) || targets[idx].URL == "" || targets[idx].Token == "" {
			return "", "", "", false
		}
		return targets[idx].URL, targets[idx].Token, "mcp", true
	}
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

// TaskMCPTarget grants access to one server for one current task claim.
type TaskMCPTarget struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}
