package execenv

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// codexReservedProviders are built-in provider IDs the Codex CLI refuses to
// let model_providers override ("Built-in providers cannot be overridden").
var codexReservedProviders = map[string]string{
	"openai":     "openai-custom",
	"openrouter": "openrouter-custom",
}

// codexProviderName remaps reserved built-in provider IDs to non-reserved
// aliases so the generated config.toml loads under the Codex CLI. Non-reserved
// providers pass through unchanged.
func codexProviderName(provider string) string {
	if alias, ok := codexReservedProviders[provider]; ok {
		return alias
	}
	return provider
}

// WriteCodexHome writes the isolated Codex CLI configuration for one workdir.
func WriteCodexHome(workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvName string) error {
	return WriteCodexHomeWithAuthMode(workDir, rec, oneapiBase, boundaryBase, apiKeyEnvName, "")
}

func WriteCodexHomeWithAuthMode(workDir string, rec *registry.AgentRecord, oneapiBase, boundaryBase, apiKeyEnvName, authMode string) error {
	if codexUsesHostChatGPTAuth(authMode) {
		return nil
	}
	return writeCodexHomeWithExternalSkillRoots(
		workDir,
		rec,
		oneapiBase,
		boundaryBase,
		apiKeyEnvName,
		codexExternalSkillRoots(workDir),
	)
}

func codexUsesHostChatGPTAuth(authMode string) bool {
	return strings.EqualFold(strings.TrimSpace(authMode), "chatgpt")
}

func writeCodexHomeWithExternalSkillRoots(
	workDir string,
	rec *registry.AgentRecord,
	oneapiBase, boundaryBase, apiKeyEnvName string,
	externalSkillRoots []string,
) error {
	if rec == nil {
		return fmt.Errorf("execenv: nil agent record")
	}

	var config strings.Builder
	if model := openCodeModelName(rec.Model); model != "" {
		fmt.Fprintf(&config, "model = %q\n", model)
	}
	provider := codexProviderName(openCodeProviderName(rec.Model))
	fmt.Fprintf(&config, "model_provider = %q\n", provider)
	config.WriteString("approval_policy = \"never\"\n\n")
	fmt.Fprintf(&config, "[model_providers.%s]\n", provider)
	fmt.Fprintf(&config, "name = %q\n", provider)
	fmt.Fprintf(&config, "base_url = %q\n", normalizeOpenCodeBaseURL(oneapiBase))
	config.WriteString("env_key = \"ONEAPI_API_KEY\"\n")
	config.WriteString("wire_api = \"responses\"\n")

	mcp := make(map[string]any)
	for idx, server := range rec.MCPServers {
		targetURL, token, host, ok := mcpServerTarget(rec, server, idx, boundaryBase)
		if !ok {
			continue
		}
		name := uniqueMCPName(mcp, host)
		mcp[name] = struct{}{}
		fmt.Fprintf(&config, "\n[mcp_servers.%s]\n", name)
		fmt.Fprintf(&config, "url = %q\n", targetURL)
		if token != "" {
			fmt.Fprintf(&config, "bearer_token_env_var = %q\n", fmt.Sprintf("WEAVE_MCP_BOUNDARY_TOKEN_%d", idx))
		}
	}

	config.WriteString("\n[features]\n")
	config.WriteString("plugins = false\n")
	config.WriteString("apps = false\n")
	config.WriteString("multi_agent = false\n")
	config.WriteString("skill_mcp_dependency_install = false\n")
	for _, skillPath := range codexSkillFiles(externalSkillRoots) {
		config.WriteString("\n[[skills.config]]\n")
		fmt.Fprintf(&config, "path = %q\n", skillPath)
		config.WriteString("enabled = false\n")
	}

	_ = apiKeyEnvName
	path := filepath.Join(workDir, ".codex-home", "config.toml")
	return writeFileIfChanged(path, []byte(config.String()), 0o600)
}

func codexExternalSkillRoots(workDir string) []string {
	roots := []string{
		filepath.Join(workDir, ".codex-home", "skills", ".system"),
		"/etc/codex/skills",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".agents", "skills"))
	}
	return roots
}

func codexSkillFiles(roots []string) []string {
	seen := make(map[string]struct{})
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name(), "SKILL.md")
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				seen[path] = struct{}{}
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
