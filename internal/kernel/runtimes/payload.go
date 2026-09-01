package runtimes

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// EngineLoom is the canonical engine literal for in-process loom execution.
// Agent records historically use "" as an alias; enqueue and claim always
// write the literal so downstream consumers never re-guess the default.
const EngineLoom = "loom"

// CanonicalEngine maps a record-level engine value to its canonical form.
func CanonicalEngine(engine string) string {
	if engine == "" {
		return EngineLoom
	}
	return engine
}

// EngineExecRequest is the payload of a kind="engine_exec" task (plan D3).
// The server builds it (skills resolved, boundary tokens computed); the daemon
// decodes it and runs the engine locally. The MCP-boundary HMAC secret never
// leaves the server — only the derived per-server tokens travel in Env.
type EngineExecRequest struct {
	Agent          string                 `json:"agent"`
	Engine         string                 `json:"engine"`
	Model          string                 `json:"model"`
	Prompt         string                 `json:"prompt"`
	OutputSchema   json.RawMessage        `json:"output_schema,omitempty"`
	Record         *registry.AgentRecord  `json:"record"`
	Env            map[string]string      `json:"env,omitempty"`
	OneAPIBase     string                 `json:"oneapi_base,omitempty"`
	OneAPIKey      string                 `json:"oneapi_key,omitempty"`
	TimeoutSeconds int                    `json:"timeout_seconds,omitempty"`
	EngineVersion  string                 `json:"engine_version,omitempty"`
	Attachments    []EngineExecAttachment `json:"attachments,omitempty"`
	Loom           *LoomExecInput         `json:"loom,omitempty"`
}

// LoomExecInput is the loom-specific slice of an engine_exec payload. The
// full session messages must travel with the task — sending only the last
// prompt would make a runtime loom turn diverge from a server loom turn.
type LoomExecInput struct {
	Messages        []contract.Message   `json:"messages"`
	LastUserMessage string               `json:"last_user_message"`
	SessionID       string               `json:"session_id,omitempty"`
	UserID          string               `json:"user_id,omitempty"`
	ConversationID  string               `json:"conversation_id,omitempty"`
	Profile         string               `json:"profile,omitempty"`
	Effort          contract.EffortLevel `json:"effort,omitempty"`
	Context         map[string]any       `json:"context,omitempty"`
	NoDispatch      bool                 `json:"no_dispatch,omitempty"`
	// MCPServerCount is filled at claim time: the daemon learns how many
	// task-scoped MCP gateway endpoints to dial without ever seeing the
	// upstream URLs or headers (those stay in the server-side snapshot).
	MCPServerCount int `json:"mcp_server_count,omitempty"`
}

// EngineExecAttachment names one uploaded file; the daemon fetches its bytes
// through GET /v1/runtime/tasks/:id/attachments/:aid while holding the lease.
type EngineExecAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

// EngineExecResult is the terminal result JSON of an engine_exec task.
// Usage is a pointer because encoding/json's omitempty cannot elide a zero
// struct — CLI daemons keep producing the historical {"output": ...} shape.
type EngineExecResult struct {
	Output       string               `json:"output"`
	StopReason   string               `json:"stop_reason,omitempty"`
	Usage        *contract.Usage      `json:"usage,omitempty"`
	RunID        string               `json:"run_id,omitempty"`
	Status       string               `json:"status,omitempty"`
	Error        string               `json:"error,omitempty"`
	UsageReceipt *engine.UsageReceipt `json:"usage_receipt,omitempty"`
	Diagnostics  []engine.Diagnostic  `json:"diagnostics,omitempty"`
	Events       []engine.Event       `json:"events,omitempty"`
	Artifacts    []engine.Artifact    `json:"artifacts,omitempty"`
}

// CLIEngineExecResult preserves the complete external-engine outcome across
// the daemon/server task boundary.
func CLIEngineExecResult(result engine.RunResult) EngineExecResult {
	return EngineExecResult{
		Output:       result.Output,
		Status:       result.Status,
		Error:        result.Err,
		UsageReceipt: result.Usage,
		Diagnostics:  append([]engine.Diagnostic(nil), result.Diagnostics...),
		Events:       append([]engine.Event(nil), result.Events...),
		Artifacts:    append([]engine.Artifact(nil), result.Artifacts...),
	}
}

// EngineRunResult maps a remote task result back to the engine carrier.
func (result EngineExecResult) EngineRunResult() engine.RunResult {
	status := result.Status
	if status == "" {
		status = "completed" // compatibility with pre-receipt daemons
	}
	return engine.RunResult{
		Output: result.Output, Status: status, Err: result.Error,
		Usage:       result.UsageReceipt,
		Diagnostics: append([]engine.Diagnostic(nil), result.Diagnostics...),
		Events:      append([]engine.Event(nil), result.Events...),
		Artifacts:   append([]engine.Artifact(nil), result.Artifacts...),
	}
}

// RedactClaimPayload returns the payload bytes a daemon may see for one
// claimed engine_exec task. CLI payloads never expose server-held model
// credentials or boundary-token Env values; the daemon derives those from its
// local runtime process environment and the task identity. Legacy inline MCP
// entries also lose headers and all URL components except the origin needed by
// the daemon's remote-URL allowlist. Loom payloads
// lose every server-held secret: Record.MCPServers (upstream URLs and headers
// stay in the server-side task snapshot for the task-scoped gateway), Env
// (boundary tokens), and the OneAPI credentials; the daemon only learns the
// server count via Loom.MCPServerCount. The input bytes are never mutated —
// redaction decodes into a fresh value and re-encodes.
func RedactClaimPayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var payload EngineExecRequest
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Foreign/legacy payload shape: redaction only applies to payloads
		// this package marshalled, which always decode.
		return raw, nil
	}
	if CanonicalEngine(payload.Engine) != EngineLoom {
		return redactCLIClaimPayload(raw, payload)
	}
	count := 0
	if payload.Record != nil {
		count = len(payload.Record.MCPServers)
		recordCopy := *payload.Record
		recordCopy.MCPServers = nil
		payload.Record = &recordCopy
	}
	if payload.Loom == nil {
		payload.Loom = &LoomExecInput{}
	} else {
		loomCopy := *payload.Loom
		payload.Loom = &loomCopy
	}
	payload.Loom.MCPServerCount = count
	payload.Engine = EngineLoom
	payload.Env = nil
	payload.OneAPIBase = ""
	payload.OneAPIKey = ""
	redacted, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("redact loom payload: %w", err)
	}
	return redacted, nil
}

func redactCLIClaimPayload(raw json.RawMessage, payload EngineExecRequest) (json.RawMessage, error) {
	payload.Env = nil
	payload.OneAPIBase = ""
	payload.OneAPIKey = ""
	if payload.Record == nil {
		redacted, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("redact CLI payload: %w", err)
		}
		return redacted, nil
	}
	servers := append([]registry.MCPServerConfig(nil), payload.Record.MCPServers...)
	for idx := range servers {
		if servers[idx].ServerID != "" {
			continue
		}
		servers[idx].Headers = nil
		servers[idx].URL = inlineMCPOrigin(servers[idx].URL)
	}
	recordCopy := *payload.Record
	recordCopy.MCPServers = servers
	payload.Record = &recordCopy
	redacted, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("redact CLI payload: %w", err)
	}
	return redacted, nil
}

func inlineMCPOrigin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}
