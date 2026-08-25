package api

import (
	"net/http"
	"sort"

	"github.com/labstack/echo/v4"
)

// ── Response types ──────────────────────────────────────────

// SourceInfo describes one MCP data source aggregated from agent registrations.
type SourceInfo struct {
	URL       string         `json:"url"`
	Name      string         `json:"name"`
	Status    string         `json:"status"` // "online" | "offline" | "unknown"
	LatencyMs int64          `json:"latency_ms,omitempty"`
	Tools     []SourceTool   `json:"tools"`
	Agents    []string       `json:"agents"`
	Stats     map[string]any `json:"stats,omitempty"`
	Meta      *SourceMeta    `json:"meta,omitempty"`
}

// SourceTool is a tool exposed by an MCP server.
type SourceTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SourceMeta is self-description metadata from the MCP server.
type SourceMeta struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"display_name"`
	Description string         `json:"description"`
	Domain      string         `json:"domain"`
	Icon        string         `json:"icon"`
	SyncMode    string         `json:"sync_mode"`
	Tables      []TableMapping `json:"tables,omitempty"`
	EnumMaps    []EnumMapping  `json:"enum_maps,omitempty"`
}

type TableMapping struct {
	Source      string         `json:"source"`
	SourceLabel string         `json:"source_label"`
	Fields      []FieldMapping `json:"fields,omitempty"`
}

type FieldMapping struct {
	Src        string `json:"src"`
	Target     string `json:"target"`
	TargetProp string `json:"target_prop"`
	Type       string `json:"type"`
}

type EnumMapping struct {
	Name   string            `json:"name"`
	Label  string            `json:"label"`
	Values map[string]string `json:"values"`
}

// ── Handler ─────────────────────────────────────────────────

func (s *Server) handleListSources(c echo.Context) error {
	tenant := getTenant(c)
	if s.MCPRegistry == nil {
		return mcpRegistryUnavailable(c)
	}
	ctx := c.Request().Context()
	registryServers, err := s.MCPRegistry.List(ctx, tenant)
	if err != nil {
		return handleMCPRegistryError(c, err)
	}

	results := make([]SourceInfo, 0, len(registryServers))
	for _, server := range registryServers {
		catalog, err := s.MCPRegistry.Catalog(ctx, tenant, server.ID)
		if err != nil {
			return handleMCPRegistryError(c, err)
		}
		agentNames, err := s.MCPRegistry.ListAgentNames(ctx, tenant, server.ID)
		if err != nil {
			return handleMCPRegistryError(c, err)
		}
		tools := make([]SourceTool, 0, len(catalog.Tools))
		for _, tool := range catalog.Tools {
			tools = append(tools, SourceTool{Name: tool.Name, Description: tool.Description})
		}
		results = append(results, SourceInfo{
			URL:    server.URL,
			Name:   server.DisplayName,
			Status: server.Status,
			Tools:  tools,
			Agents: agentNames,
		})
	}

	// Legacy inline refs remain visible during the dual-read window, but they
	// are never treated as registry status and are no longer probed here.
	legacyByURL := make(map[string][]string)
	if s.Registry != nil {
		agents, err := s.Registry.List(ctx, tenant)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		for _, agent := range agents {
			for _, mcp := range agent.MCPServers {
				if mcp.URL == "" {
					continue
				}
				legacyByURL[mcp.URL] = append(legacyByURL[mcp.URL], agent.Name)
			}
		}
	}
	for legacyURL, agentNames := range legacyByURL {
		results = append(results, SourceInfo{
			URL:    legacyURL,
			Name:   inferName(legacyURL),
			Status: "unknown",
			Tools:  make([]SourceTool, 0),
			Agents: dedup(agentNames),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Name == results[j].Name {
			return results[i].URL < results[j].URL
		}
		return results[i].Name < results[j].Name
	})

	return c.JSON(http.StatusOK, results)
}

// ── Helpers ─────────────────────────────────────────────────

// inferName extracts a human-readable name from an MCP server URL.
// e.g. "http://spareparts-mcp:9090/" → "spareparts-mcp"
func inferName(url string) string {
	// Strip scheme
	s := url
	for _, prefix := range []string{"http://", "https://"} {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			s = s[len(prefix):]
			break
		}
	}
	// Strip port and path
	for i, c := range s {
		if c == ':' || c == '/' {
			s = s[:i]
			break
		}
	}
	return s
}

func dedup(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
