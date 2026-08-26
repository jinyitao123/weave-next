package api

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/mcpprotocol"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/labstack/echo/v4"
)

var (
	boundarySecretOnce sync.Once
	boundarySecretKey  []byte
)

func boundaryKey() []byte {
	boundarySecretOnce.Do(func() {
		key, err := secret.KeyFromEnv()
		if err == nil {
			boundarySecretKey = key
		}
	})
	return boundarySecretKey
}

// BoundaryToken returns the HMAC token authorizing one agent MCP boundary.
// It returns an empty string when WEAVE_SECRET_KEY is unavailable or invalid.
func BoundaryToken(tenant, agent string, idx int) string {
	return secret.BoundaryToken(tenant, agent, idx)
}

// MCPGatewayToken returns the stable-ID token for one agent/server ref.
func MCPGatewayToken(workspace, agent, serverID string) string {
	return secret.MCPGatewayToken(workspace, agent, serverID)
}

type boundaryAgentLookup interface {
	Get(ctx context.Context, tenant, name string) (*registry.AgentRecord, error)
}

const (
	upstreamMCPUnavailable = "upstream MCP unavailable"
)

func (s *Server) handleMCPBoundary(c echo.Context) error {
	return s.handleMCPBoundaryWith(
		c, s.Registry, mcphost.NewToolBroker(s.mcpAccessFactory()),
	)
}

func (s *Server) handleMCPGateway(c echo.Context) error {
	return s.handleMCPGatewayWith(c, s.Registry, s.mcpAccessFactory())
}

func (s *Server) handleMCPGatewayWith(
	c echo.Context,
	lookup boundaryAgentLookup,
	factory *mcphost.MCPAccessFactory,
) error {
	if len(boundaryKey()) == 0 {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "MCP gateway secret is unavailable"})
	}
	workspace := c.Param("workspace")
	agent := c.Param("agent")
	serverID := c.Param("serverID")
	wantToken := MCPGatewayToken(workspace, agent, serverID)
	auth := c.Request().Header.Get(echo.HeaderAuthorization)
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(auth, bearerPrefix) ||
		!hmac.Equal([]byte(strings.TrimPrefix(auth, bearerPrefix)), []byte(wantToken)) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	if lookup == nil || factory == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "MCP gateway dependencies are unavailable"})
	}
	rec, err := lookup.Get(c.Request().Context(), workspace, agent)
	if err != nil || rec == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	referenced := false
	for _, server := range rec.MCPServers {
		if server.ServerID == serverID {
			referenced = true
			break
		}
	}
	if !referenced {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	dispatcher, err := factory.BuildServer(c.Request().Context(), workspace, rec, serverID, "")
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	return handleStableGatewayRPC(c, dispatcher)
}

func handleStableGatewayRPC(c echo.Context, dispatcher contract.ToolDispatcher) error {
	request, err := mcpprotocol.Decode(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON-RPC request"})
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return handleMCPProtocolRequest(c, dispatcher, request, "weave-mcp-gateway", "method not supported by gateway")
}

func (s *Server) handleMCPBoundaryWith(
	c echo.Context,
	lookup boundaryAgentLookup,
	broker *mcphost.ToolBroker,
) error {
	key := boundaryKey()
	if len(key) == 0 {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "MCP boundary secret is unavailable"})
	}

	idx, err := strconv.Atoi(c.Param("idx"))
	if err != nil || idx < 0 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	tenant := c.Param("tenant")
	agent := c.Param("agent")
	wantToken := BoundaryToken(tenant, agent, idx)
	auth := c.Request().Header.Get(echo.HeaderAuthorization)
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(auth, bearerPrefix) ||
		!hmac.Equal([]byte(strings.TrimPrefix(auth, bearerPrefix)), []byte(wantToken)) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}

	if lookup == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "agent registry is unavailable"})
	}
	rec, err := lookup.Get(c.Request().Context(), tenant, agent)
	if err != nil || rec == nil || idx >= len(rec.MCPServers) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	if broker == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "MCP boundary dependencies are unavailable"})
	}

	request, err := mcpprotocol.Decode(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON-RPC request"})
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	dispatcher, err := broker.BuildMCPServerAt(
		c.Request().Context(),
		mcphost.ToolBrokerRequest{WorkspaceID: tenant, Agent: rec},
		idx,
	)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}

	return handleMCPProtocolRequest(c, dispatcher, request, "weave-mcp-boundary", "method not supported by boundary")
}

func handleMCPProtocolRequest(
	c echo.Context,
	dispatcher contract.ToolDispatcher,
	request mcpprotocol.Request,
	serverName string,
	unsupportedMethodMessage string,
) error {
	result, err := (mcpprotocol.Adapter{
		Dispatcher: dispatcher, ServerName: serverName,
		UnsupportedMethodMessage: unsupportedMethodMessage, RedactToolErrors: true,
	}).Handle(c.Request().Context(), request)
	switch {
	case errors.Is(err, mcpprotocol.ErrInvalidCallParams):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid tools/call params"})
	case errors.Is(err, mcpprotocol.ErrUpstreamUnavailable):
		return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
	case errors.Is(err, mcpprotocol.ErrEmptyToolResult):
		return echo.NewHTTPError(http.StatusBadGateway, "empty MCP tool result")
	case err != nil:
		return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
	case result.Notification:
		return c.NoContent(http.StatusAccepted)
	default:
		return c.JSON(http.StatusOK, result.Response)
	}
}
