package api

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jinyitao123/loom/contract"
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

type boundaryRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type boundaryResponse struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Result  any               `json:"result,omitempty"`
	Error   *boundaryRPCError `json:"error,omitempty"`
}

type boundaryRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type boundaryTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

const (
	upstreamMCPUnavailable = "upstream MCP unavailable"
	upstreamMCPError       = "upstream MCP error"
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
	var request boundaryRequest
	decoder := json.NewDecoder(c.Request().Body)
	if err := decoder.Decode(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON-RPC request"})
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON-RPC request"})
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	switch request.Method {
	case "initialize":
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0", ID: request.ID,
			Result: map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "weave-mcp-gateway", "version": "1"},
			},
		})
	case "notifications/initialized":
		return c.NoContent(http.StatusAccepted)
	case "tools/list":
		tools, err := dispatcher.ListTools(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
		}
		resultTools := make([]boundaryTool, 0, len(tools))
		for _, tool := range tools {
			resultTools = append(resultTools, boundaryTool{
				Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
			})
		}
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0", ID: request.ID, Result: map[string]any{"tools": resultTools},
		})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid tools/call params"})
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		result, err := dispatcher.Dispatch(c.Request().Context(), contract.ToolCall{
			ID: boundaryRequestID(request.ID), Name: params.Name, Args: string(params.Arguments),
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
		}
		if result == nil {
			return echo.NewHTTPError(http.StatusBadGateway, "empty MCP tool result")
		}
		content := result.Content
		if result.IsError {
			content = upstreamMCPError
		}
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0", ID: request.ID,
			Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": content}},
				"isError": result.IsError,
			},
		})
	default:
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0", ID: request.ID,
			Error: &boundaryRPCError{Code: -32601, Message: "method not supported by gateway"},
		})
	}
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

	var request boundaryRequest
	decoder := json.NewDecoder(c.Request().Body)
	if err := decoder.Decode(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON-RPC request"})
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
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

	switch request.Method {
	case "initialize":
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result: map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]string{
					"name":    "weave-mcp-boundary",
					"version": "1",
				},
			},
		})
	case "notifications/initialized":
		return c.NoContent(http.StatusAccepted)
	case "tools/list":
		tools, err := dispatcher.ListTools(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
		}
		resultTools := make([]boundaryTool, 0, len(tools))
		for _, tool := range tools {
			resultTools = append(resultTools, boundaryTool{
				Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
			})
		}
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result:  map[string]any{"tools": resultTools},
		})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid tools/call params"})
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		result, err := dispatcher.Dispatch(c.Request().Context(), contract.ToolCall{
			ID:   boundaryRequestID(request.ID),
			Name: params.Name,
			Args: string(params.Arguments),
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, upstreamMCPUnavailable)
		}
		if result == nil {
			return echo.NewHTTPError(http.StatusBadGateway, "empty MCP tool result")
		}
		content := result.Content
		if result.IsError {
			content = upstreamMCPError
		}
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": content}},
				"isError": result.IsError,
			},
		})
	default:
		return c.JSON(http.StatusOK, boundaryResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Error: &boundaryRPCError{
				Code:    -32601,
				Message: "method not supported by boundary",
			},
		})
	}
}

func boundaryRequestID(raw json.RawMessage) string {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id
	}
	return string(raw)
}
