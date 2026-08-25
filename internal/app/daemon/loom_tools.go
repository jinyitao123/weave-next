package daemon

import (
	"net/url"
	"strconv"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

// newRuntimeToolDispatcher builds the ToolDispatcher a remote loom run uses: one
// MCP gateway per server the task references, composited into a single
// dispatcher. Each gateway is dialed at the server — /v1/runtime/tasks/:id/mcp/:idx,
// never the upstream MCP endpoint — carrying the runtime lease token. So the
// daemon never learns an upstream URL or header, and every tool listing and call
// is governed server-side by the same ToolBroker a local run uses (resolve,
// filter, write gate, audit). No daemon-side filter is applied: the
// governed gateway is the single point of truth.
//
// count is the redacted claim payload's Loom.MCPServerCount — the daemon knows
// how many gateways to dial without ever seeing what they point at. The result
// is always a usable dispatcher, never nil: loom's ToolLoop calls ListTools
// unconditionally, so a zero-server agent gets an empty composite (ListTools
// returns nothing) rather than a nil that would panic the run.
func newRuntimeToolDispatcher(client *runtimeClient, taskID string, count int) contract.ToolDispatcher {
	if count <= 0 {
		return mcphost.NewCompositeDispatcher()
	}
	authHeader := map[string]string{"Authorization": "Bearer " + client.token}
	hosts := make([]contract.ToolDispatcher, 0, count)
	for idx := range count {
		gatewayURL := client.baseURL + "/v1/runtime/tasks/" + url.PathEscape(taskID) + "/mcp/" + strconv.Itoa(idx)
		hosts = append(hosts, mcphost.NewHTTPHost(
			gatewayURL,
			mcphost.WithHTTPClient(client.httpClient),
			mcphost.WithHeaders(authHeader),
		))
	}
	return mcphost.NewCompositeDispatcher(hosts...)
}
