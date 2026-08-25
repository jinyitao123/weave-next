package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

// handleRuntimeTaskMCP is the task-scoped MCP gateway a remote loom daemon
// dials for each of its agent's MCP servers. It is the loom counterpart of the
// public /v1/mcp-boundary route, through the same governed ToolBroker, with
// two deliberate differences that keep secrets on the server:
//
//   - Auth is the runtime lease (claimedRuntimeTask), not an HMAC boundary
//     token — the daemon only ever holds its rtk_ runtime token.
//   - The agent record comes from the task's frozen snapshot, not a live
//     Registry.Get, so an agent edited after enqueue can't repoint a running
//     task's MCP server. The daemon never receives the upstream URL or headers
//     (redaction strips them from the claimed copy; see
//     runtimes.RedactClaimPayload); they stay in the server-side payload and
//     are applied here, inside the governed pipeline.
//
// Governance (resolve, authorize, fail-closed write gate, audit) is identical to
// the public boundary — BuildMCPServerAt runs the same layers — so a remote
// loom turn is gated exactly like a local one.
func (s *Server) handleRuntimeTaskMCP(c echo.Context) error {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return err
	}

	var payload runtimes.EngineExecRequest
	if decodeErr := json.Unmarshal(task.Payload, &payload); decodeErr != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	// The MCP facade is loom-only; CLI engines reach MCP through their own
	// per-server boundary tokens delivered in the engine env.
	if runtimes.CanonicalEngine(payload.Engine) != runtimes.EngineLoom || payload.Record == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}

	idx, convErr := strconv.Atoi(c.Param("idx"))
	if convErr != nil || idx < 0 || idx >= len(payload.Record.MCPServers) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}

	// The governing workspace is the runtime's own workspace (lease-verified),
	// never a value read out of the task payload.
	tenant := runtime.WorkspaceID
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())
	dispatcher, buildErr := broker.BuildMCPServerAt(
		c.Request().Context(),
		mcphost.ToolBrokerRequest{WorkspaceID: tenant, Agent: payload.Record},
		idx,
	)
	if buildErr != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	return handleStableGatewayRPC(c, dispatcher)
}
