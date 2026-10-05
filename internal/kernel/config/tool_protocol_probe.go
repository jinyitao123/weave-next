package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// ToolProtocolProbe is an operator-only, expiring sampling policy. It is not
// part of any frozen workflow, user authorization, or provider request.
type ToolProtocolProbe struct {
	WorkspaceID, WorkflowID, RunID string
	ExpiresAt                      time.Time
	MaxModelCalls                  int
}

func loadToolProtocolProbe() (*ToolProtocolProbe, error) {
	enabled, err := boolEnv("WEAVE_TOOL_PROTOCOL_PROBE_ENABLED", false)
	if err != nil || !enabled {
		return nil, err
	}
	scope := func(key string, required bool) (string, error) {
		value := strings.TrimSpace(os.Getenv(key))
		if required && value == "" || len(value) > 160 || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New(key + " must identify one exact scope")
		}
		return value, nil
	}
	p := &ToolProtocolProbe{}
	if p.WorkspaceID, err = scope("WEAVE_TOOL_PROTOCOL_PROBE_WORKSPACE_ID", true); err != nil {
		return nil, err
	}
	if p.WorkflowID, err = scope("WEAVE_TOOL_PROTOCOL_PROBE_WORKFLOW_ID", true); err != nil {
		return nil, err
	}
	if p.RunID, err = scope("WEAVE_TOOL_PROTOCOL_PROBE_RUN_ID", false); err != nil {
		return nil, err
	}
	p.ExpiresAt, err = time.Parse(time.RFC3339, os.Getenv("WEAVE_TOOL_PROTOCOL_PROBE_EXPIRES_AT"))
	if err != nil || p.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
		return nil, errors.New("WEAVE_TOOL_PROTOCOL_PROBE_EXPIRES_AT must be an explicit RFC3339 deadline within 24 hours")
	}
	p.MaxModelCalls, err = strconv.Atoi(os.Getenv("WEAVE_TOOL_PROTOCOL_PROBE_MAX_MODEL_CALLS"))
	if err != nil || p.MaxModelCalls < 1 || p.MaxModelCalls > 64 {
		return nil, errors.New("WEAVE_TOOL_PROTOCOL_PROBE_MAX_MODEL_CALLS must be between 1 and 64")
	}
	// An expired policy stays inert across restarts; it must not prevent ordinary
	// business execution from starting or silently renew its sampling window.
	return p, nil
}
