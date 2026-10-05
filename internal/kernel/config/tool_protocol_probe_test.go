package config

import (
	"testing"
	"time"
)

func probeEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"WEAVE_TOOL_PROTOCOL_PROBE_ENABLED": "true", "WEAVE_TOOL_PROTOCOL_PROBE_WORKSPACE_ID": "workspace",
		"WEAVE_TOOL_PROTOCOL_PROBE_WORKFLOW_ID": "workflow", "WEAVE_TOOL_PROTOCOL_PROBE_RUN_ID": "run",
		"WEAVE_TOOL_PROTOCOL_PROBE_EXPIRES_AT":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"WEAVE_TOOL_PROTOCOL_PROBE_MAX_MODEL_CALLS": "2",
	} {
		t.Setenv(key, value)
	}
}

func TestToolProtocolProbeRequiresExplicitBoundedEnablement(t *testing.T) {
	probeEnvironment(t)
	t.Setenv("WEAVE_TOOL_PROTOCOL_PROBE_ENABLED", "")
	if policy, err := loadToolProtocolProbe(); err != nil || policy != nil {
		t.Fatal("probe is not disabled by default")
	}
	t.Setenv("WEAVE_TOOL_PROTOCOL_PROBE_ENABLED", "true")
	if policy, err := loadToolProtocolProbe(); err != nil || policy == nil || policy.RunID != "run" || policy.MaxModelCalls != 2 {
		t.Fatal("explicit policy was not loaded")
	}
	for key, value := range map[string]string{
		"WEAVE_TOOL_PROTOCOL_PROBE_WORKSPACE_ID": "", "WEAVE_TOOL_PROTOCOL_PROBE_WORKFLOW_ID": "",
		"WEAVE_TOOL_PROTOCOL_PROBE_EXPIRES_AT":      time.Now().Add(25 * time.Hour).Format(time.RFC3339),
		"WEAVE_TOOL_PROTOCOL_PROBE_MAX_MODEL_CALLS": "65",
	} {
		t.Run(key, func(t *testing.T) {
			probeEnvironment(t)
			t.Setenv(key, value)
			if _, err := loadToolProtocolProbe(); err == nil {
				t.Fatal("unbounded probe scope was accepted")
			}
		})
	}
	probeEnvironment(t)
	t.Setenv("WEAVE_TOOL_PROTOCOL_PROBE_RUN_ID", "")
	t.Setenv("WEAVE_TOOL_PROTOCOL_PROBE_EXPIRES_AT", time.Now().Add(-time.Minute).Format(time.RFC3339))
	if policy, err := loadToolProtocolProbe(); err != nil || policy == nil || policy.ExpiresAt.After(time.Now()) {
		t.Fatal("expired policy must remain expired without blocking restart")
	}
}
