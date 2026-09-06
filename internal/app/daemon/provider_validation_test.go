package daemon

import "testing"

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
