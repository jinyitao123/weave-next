package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/labstack/echo/v4"
)

// MCPAgentReferenceReader is a product directory port, independent of the
// kernel's resource catalog and credential resolution.
type MCPAgentReferenceReader interface {
	MCPReferenceCounts(context.Context, string, []string) (map[string]int, error)
}

type mcpServerProductView struct {
	mcpregistry.ServerView
	AgentCount int `json:"agent_count"`
}

type mcpCatalogProductView struct {
	Server mcpServerProductView `json:"server"`
	Tools  []mcpregistry.Tool   `json:"tools"`
}

func projectMCPServers(ctx context.Context, directory MCPAgentReferenceReader, workspaceID string, servers []mcpregistry.ServerView) ([]mcpServerProductView, error) {
	if directory == nil {
		return nil, errors.New("agent directory is unavailable")
	}
	ids := make([]string, len(servers))
	for i, server := range servers {
		if server.WorkspaceID != workspaceID {
			return nil, errors.New("tool resource workspace mismatch")
		}
		ids[i] = server.ID
	}
	counts, err := directory.MCPReferenceCounts(ctx, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	views := make([]mcpServerProductView, len(servers))
	for i, server := range servers {
		count, found := counts[server.ID]
		if !found || count < 0 {
			return nil, errors.New("tool resource membership projection is incomplete")
		}
		views[i] = mcpServerProductView{ServerView: server, AgentCount: count}
	}
	return views, nil
}

func (s *Server) writeMCPServerProductView(c echo.Context, status int, server mcpregistry.ServerView) error {
	views, err := projectMCPServers(c.Request().Context(), s.Registry, getTenant(c), []mcpregistry.ServerView{server})
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "工具使用情况暂时不可用，请稍后重试"})
	}
	return c.JSON(status, views[0])
}

func (s *Server) writeMCPCatalogProductView(c echo.Context, result mcpregistry.ProbeResult) error {
	views, err := projectMCPServers(c.Request().Context(), s.Registry, getTenant(c), []mcpregistry.ServerView{result.Server})
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "工具使用情况暂时不可用，请稍后重试"})
	}
	return c.JSON(http.StatusOK, mcpCatalogProductView{Server: views[0], Tools: result.Tools})
}
