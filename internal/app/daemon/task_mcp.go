package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

func runtimeTaskMCPTargets(server string, task *runtimeprotocol.ExecutionClaim, request runtimeprotocol.ExecutionRequest) ([]execenv.TaskMCPTarget, error) {
	if len(request.TaskMCP) == 0 {
		return nil, nil
	}
	if task == nil || task.ClaimEpoch <= 0 || filepath.Base(task.TaskID) != task.TaskID || task.TaskID == "." || task.TaskID == ".." {
		return nil, fmt.Errorf("runtime: missing or unredacted task MCP claim")
	}
	targets := make([]execenv.TaskMCPTarget, len(request.TaskMCP))
	for index, target := range request.TaskMCP {
		expected := fmt.Sprintf("/v1/runtime/tasks/%s/mcp/%d", task.TaskID, index)
		if target.URL != expected || !strings.HasPrefix(target.Token, secret.TaskMCPTokenPrefix) {
			return nil, fmt.Errorf("runtime: invalid task MCP target")
		}
		targets[index] = execenv.TaskMCPTarget{URL: strings.TrimRight(server, "/") + expected, Token: target.Token}
	}
	return targets, nil
}

func engineTaskMCPServers(targets []execenv.TaskMCPTarget) []engine.MCPServerEndpoint {
	if len(targets) == 0 {
		return nil
	}
	result := make([]engine.MCPServerEndpoint, len(targets))
	for index, target := range targets {
		result[index] = engine.MCPServerEndpoint{URL: target.URL, TokenEnv: fmt.Sprintf("WEAVE_MCP_BOUNDARY_TOKEN_%d", index)}
	}
	return result
}
