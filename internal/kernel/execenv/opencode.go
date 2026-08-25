package execenv

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const openCodeSchema = "https://opencode.ai/config.json"

// WriteOpenCodeConfig adds OpenCode provider and boundary-only MCP defaults.
// Unknown non-MCP keys and local MCP entries are preserved, while every remote
// MCP entry must exactly match a connection generated for the current record.
func WriteOpenCodeConfig(workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvValue string) error {
	if rec == nil {
		return fmt.Errorf("execenv: nil agent record")
	}
	path := filepath.Join(workDir, "opencode.json")
	existing := make(map[string]any)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("execenv: parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
	case err != nil:
		return fmt.Errorf("execenv: read %s: %w", path, err)
	}

	models := make(map[string]any)
	if model := openCodeModelName(rec.Model); model != "" {
		models[model] = map[string]any{
			"name":      model,
			"tool_call": true,
		}
	}
	mcp := make(map[string]any)
	for idx, server := range rec.MCPServers {
		targetURL, token, host, ok := mcpServerTarget(rec, server, idx, boundaryBase)
		if !ok {
			continue
		}
		name := uniqueMCPName(mcp, host)
		entry := map[string]any{
			"type":    "remote",
			"url":     targetURL,
			"enabled": true,
		}
		if token != "" {
			entry["headers"] = map[string]string{"Authorization": "Bearer " + token}
		}
		mcp[name] = entry
	}

	defaults := map[string]any{
		"$schema": openCodeSchema,
		"provider": map[string]any{
			openCodeProviderName(rec.Model): map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": openCodeProviderName(rec.Model),
				"options": map[string]any{
					"baseURL": normalizeOpenCodeBaseURL(oneapiBase),
					"apiKey":  apiKeyEnvValue,
				},
				"models": models,
			},
		},
		"mcp": mcp,
	}
	mergeMissing(existing, defaults)
	providers, ok := existing["provider"].(map[string]any)
	if !ok {
		providers = make(map[string]any)
		existing["provider"] = providers
	}
	for name, provider := range defaults["provider"].(map[string]any) {
		providers[name] = provider
	}
	existingMCP, ok := existing["mcp"].(map[string]any)
	if !ok {
		existingMCP = make(map[string]any)
		existing["mcp"] = existingMCP
	}
	removeDirectMCPEntries(existingMCP, mcp)
	for name, server := range mcp {
		existingMCP[name] = server
	}

	data, err = json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("execenv: encode opencode config: %w", err)
	}
	data = append(data, '\n')
	return writeFileIfChanged(path, data, 0o600)
}

func removeDirectMCPEntries(existing, generated map[string]any) {
	for name, value := range existing {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		rawURL, _ := entry["url"].(string)
		if rawURL == "" {
			continue
		}
		generatedEntry, ok := generated[name].(map[string]any)
		generatedURL, _ := generatedEntry["url"].(string)
		if !ok || generatedURL != rawURL {
			delete(existing, name)
		}
	}
}

func mergeMissing(dst, defaults map[string]any) {
	for key, defaultValue := range defaults {
		current, exists := dst[key]
		if !exists {
			dst[key] = defaultValue
			continue
		}
		currentMap, currentOK := current.(map[string]any)
		defaultMap, defaultOK := defaultValue.(map[string]any)
		if currentOK && defaultOK {
			mergeMissing(currentMap, defaultMap)
		}
	}
}

// Model/provider naming is owned by engine.OpenCodeModelRef so the generated
// config and the CLI --model argument can never drift apart.
func openCodeModelName(model string) string {
	_, name := engine.OpenCodeModelRef(model)
	return name
}

func openCodeProviderName(model string) string {
	provider, _ := engine.OpenCodeModelRef(model)
	return provider
}

func normalizeOpenCodeBaseURL(base string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}

func allowedRemoteURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || host == "metadata" || strings.HasPrefix(host, "metadata.") || strings.HasSuffix(host, ".metadata") {
		return nil, false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return nil, false
	}
	return parsed, true
}

func uniqueMCPName(mcp map[string]any, host string) string {
	name := sanitizeMCPName(strings.ToLower(host))
	if _, exists := mcp[name]; !exists {
		return name
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", name, suffix)
		if _, exists := mcp[candidate]; !exists {
			return candidate
		}
	}
}

func sanitizeMCPName(host string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, host)
	if name == "" {
		return "mcp"
	}
	return name
}
