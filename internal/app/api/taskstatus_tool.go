package api

import (
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

// gatedTaskStatusDispatcher builds the builtin task-status tools behind the
// same write gate as MCP servers: cancel_task and retry_failed_leg are
// declared writes rejected by policy; the read tools pass through.
func (s *Server) gatedTaskStatusDispatcher(tenant, agent, userID, conversationID string) contract.ToolDispatcher {
	return mcphost.NewWriteGateDispatcher(
		mcphost.NewTaskStatusDispatcher(s.Fanout, s.Tasks, tenant, agent, userID),
		tenant, agent,
		mcphost.BuiltinTaskStatusServerURL, conversationID,
		mcphost.TaskStatusWriteTools,
	)
}

// subAgentTaskStatusDispatcher builds the read-only task-status tools exposed
// to depth-1 sub-agents; the gated write tools stay top-level only.
func (s *Server) subAgentTaskStatusDispatcher(tenant, agent, userID string) contract.ToolDispatcher {
	return mcphost.NewReadOnlyTaskStatusDispatcher(s.Fanout, s.Tasks, tenant, agent, userID)
}
