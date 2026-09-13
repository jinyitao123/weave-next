package daemon

import (
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestProviderPreflightRespectsEngineAuthentication(t *testing.T) {
	for _, tt := range []struct {
		engine, auth, key string
		wantErr           bool
	}{
		{"codex", "", "", true},
		{"codex", "", "  ", true},
		{"codex", "chatgpt", "", false},
		{"codex", "", "configured", false},
		{"opencode", "", "", false},
		{"claude", "", "", false},
	} {
		if err := validateRuntimeProviderConfig(tt.engine, tt.auth, tt.key); (err != nil) != tt.wantErr {
			t.Errorf("engine=%s auth=%s: error=%v, want error=%v", tt.engine, tt.auth, err, tt.wantErr)
		}
	}
}

func TestProviderAvailabilityRejectsCodexWithoutCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ONEAPI_API_KEY", "")
	capability := runtimeprotocol.EngineCapability{Engine: "codex", AuthMode: runtimes.AuthModeProvider}
	describeEngineAvailability(&capability)
	if capability.Availability != runtimes.EngineAvailabilityUnavailable || capability.UnavailableReason != "provider_credentials_missing" {
		t.Fatalf("availability = %#v", capability)
	}

	t.Setenv("ONEAPI_API_KEY", "configured")
	describeEngineAvailability(&capability)
	if capability.Availability != runtimes.EngineAvailabilityReady || capability.UnavailableReason != "" {
		t.Fatalf("availability with credential = %#v", capability)
	}
}

func TestHostAuthenticationIsReadyWithoutProviderCredential(t *testing.T) {
	for _, authMode := range []string{runtimes.AuthModeChatGPT, runtimes.AuthModeOAuth} {
		capability := runtimeprotocol.EngineCapability{Engine: "codex", AuthMode: authMode}
		describeEngineAvailability(&capability)
		if capability.Availability != runtimes.EngineAvailabilityReady || capability.UnavailableReason != "" {
			t.Fatalf("auth_mode=%s availability = %#v", authMode, capability)
		}
	}
}
