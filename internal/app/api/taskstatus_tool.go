package api

import (
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

// subAgentTaskStatusDispatcher builds the read-only task-status tools exposed
// to depth-1 sub-agents; the gated write tools stay top-level only.
func (s *Server) subAgentTaskStatusDispatcher(tenant, agent, userID string) contract.ToolDispatcher {
	return mcphost.NewReadOnlyTaskStatusDispatcher(s.Fanout, s.Tasks, tenant, agent, userID)
}
