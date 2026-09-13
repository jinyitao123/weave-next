package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimehost"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func describeEngineConfiguration(capability *runtimeprotocol.EngineCapability) {
	capability.ConfigurationSource = "runtime_environment"
	if capability.Engine == engine.Claude && capability.AuthMode == runtimeprotocol.AuthModeOAuth {
		capability.ConfigurationSource = "host_default"
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			return
		}
		var settings struct {
			Model string            `json:"model"`
			Env   map[string]string `json:"env"`
		}
		if json.Unmarshal(data, &settings) != nil {
			return
		}
		capability.ConfiguredEndpoint = runtimehost.SafeEndpointOrigin(settings.Env["ANTHROPIC_BASE_URL"])
		capability.ConfiguredModel = strings.TrimSpace(settings.Env["ANTHROPIC_MODEL"])
		if capability.ConfiguredModel == "" {
			capability.ConfiguredModel = strings.TrimSpace(settings.Model)
		}
		if capability.ConfiguredEndpoint != "" || capability.ConfiguredModel != "" {
			capability.ConfigurationSource = "host_settings"
		}
		if capability.ConfiguredEndpoint != "" {
			capability.EndpointClass = "host_configured"
		}
	} else if capability.Engine == engine.Codex && capability.AuthMode == runtimeprotocol.AuthModeChatGPT {
		// The login mode alone cannot prove a model or a provider URL.
		capability.ConfigurationSource = "host_default"
	} else {
		endpoint := os.Getenv("OPENAI_BASE_URL")
		if capability.Engine == engine.Claude && os.Getenv("ANTHROPIC_BASE_URL") != "" {
			endpoint = os.Getenv("ANTHROPIC_BASE_URL")
		}
		if endpoint == "" {
			endpoint = os.Getenv("ONEAPI_BASE_URL")
		}
		capability.ConfiguredEndpoint = runtimehost.SafeEndpointOrigin(endpoint)
	}
	if len(capability.ConfiguredModel) > 200 {
		capability.ConfiguredModel = ""
	}
}
