package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

func runtimeTaskMCPTargets(server string, task *taskqueue.Task, request runtimes.EngineExecRequest) ([]execenv.TaskMCPTarget, error) {
	if !request.BoundMCP {
		if len(request.TaskMCP) != 0 || request.FrozenMCP != nil {
			return nil, fmt.Errorf("runtime: unexpected task MCP authority")
		}
		return nil, nil
	}
	if task == nil || task.ClaimEpoch <= 0 || filepath.Base(task.ID) != task.ID || task.ID == "." || task.ID == ".." || request.Record == nil || len(request.Record.MCPServers) != 0 || request.FrozenMCP != nil || len(request.TaskMCP) == 0 {
		return nil, fmt.Errorf("runtime: missing or unredacted task MCP claim")
	}
	targets := append([]execenv.TaskMCPTarget(nil), request.TaskMCP...)
	for index := range targets {
		expected := fmt.Sprintf("/v1/runtime/tasks/%s/mcp/%d", task.ID, index)
		if targets[index].URL != expected || !strings.HasPrefix(targets[index].Token, secret.TaskMCPTokenPrefix) {
			return nil, fmt.Errorf("runtime: invalid task MCP target")
		}
		targets[index].URL = strings.TrimRight(server, "/") + expected
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
