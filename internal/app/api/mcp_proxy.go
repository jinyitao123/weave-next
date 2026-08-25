package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// handleMCPListTools proxies a tools/list JSON-RPC call to an internal MCP server.
// This allows the Console (running in the browser) to discover tools from Docker-internal URLs.
func (s *Server) handleMCPListTools(c echo.Context) error {
	var req struct {
		URL string `json:"url"`
	}
	if err := c.Bind(&req); err != nil || req.URL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "url is required"})
	}

	payload := []byte(`{"jsonrpc":"2.0","method":"tools/list","id":1}`)
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Post(req.URL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "cannot reach MCP server: " + err.Error()})
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "read response failed"})
	}

	// Parse to extract just the tools array
	var rpcResp struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "invalid MCP response"})
	}

	return c.JSONBlob(http.StatusOK, rpcResp.Result.Tools)
}
