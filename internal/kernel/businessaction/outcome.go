package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

const (
	ActionOutcomeStatusSucceeded = "succeeded"
	ActionOutcomeStatusFailed    = "failed"
	ActionOutcomeStatusUnknown   = "unknown"
	ActionOutcomeSourceForgeMCP  = "forge_mcp.run_action"
)

var ErrActionOutcomeUnresolved = errors.New("Forge action outcome remains unresolved")

// ActionOutcomeEvent contains only the scoped provenance needed to establish
// which frozen business action ran. It deliberately excludes action params,
// credentials, response bodies, and model text.
type ActionOutcomeEvent struct {
	Source             string `json:"source"`
	Phase              string `json:"phase"`
	InvocationID       string `json:"invocation_id"`
	CallID             string `json:"tool_call_id"`
	CapabilityID       string `json:"capability_id"`
	ActionKey          string `json:"action_key"`
	ActionLabel        string `json:"action_label,omitempty"`
	ActionName         string `json:"action_name"`
	ObjectName         string `json:"object_name"`
	InputRevisionID    string `json:"input_revision_id"`
	RecordID           string `json:"record_id,omitempty"`
	FrozenRecordSHA256 string `json:"frozen_record_sha256,omitempty"`
	Status             string `json:"status,omitempty"`
}

type ActionOutcomeRecorder func(context.Context, ActionOutcomeEvent) error

type ActionOutcomeReplay struct {
	Blocked bool
	Status  string
}

type ActionOutcomeGuard func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error)

func boundedActionOutcomeLabel(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 128 {
		return string(runes[:127]) + "…"
	}
	return value
}

type actionOutcomeRecorderContextKey struct{}
type actionOutcomeGuardContextKey struct{}

func WithActionOutcomeRecorder(ctx context.Context, recorder ActionOutcomeRecorder) context.Context {
	return context.WithValue(ctx, actionOutcomeRecorderContextKey{}, recorder)
}

func WithActionOutcomeGuard(ctx context.Context, guard ActionOutcomeGuard) context.Context {
	return context.WithValue(ctx, actionOutcomeGuardContextKey{}, guard)
}

func recordActionOutcome(ctx context.Context, event ActionOutcomeEvent) error {
	recorder, _ := ctx.Value(actionOutcomeRecorderContextKey{}).(ActionOutcomeRecorder)
	if recorder == nil {
		return errors.New("Forge action outcome recorder is unavailable")
	}
	return recorder(ctx, event)
}

func checkActionOutcomeReplay(ctx context.Context, event ActionOutcomeEvent) (ActionOutcomeReplay, error) {
	guard, _ := ctx.Value(actionOutcomeGuardContextKey{}).(ActionOutcomeGuard)
	if guard == nil {
		return ActionOutcomeReplay{}, errors.New("Forge action replay guard is unavailable")
	}
	return guard(ctx, event)
}

func classifyNativeActionResult(result *contract.ToolResult) string {
	if result == nil {
		return ActionOutcomeStatusUnknown
	}
	if result.IsError {
		return ActionOutcomeStatusFailed
	}
	content := strings.TrimSpace(result.Content)
	if content == "" || len(content) > 64*1024 {
		return ActionOutcomeStatusUnknown
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &envelope); err != nil || envelope == nil {
		return ActionOutcomeStatusUnknown
	}
	var ok bool
	if raw, exists := envelope["ok"]; exists {
		if err := json.Unmarshal(raw, &ok); err != nil {
			return ActionOutcomeStatusUnknown
		}
		if ok {
			if rawError, exists := envelope["error"]; exists && !isEmptyNativeError(rawError) {
				return ActionOutcomeStatusUnknown
			}
			return ActionOutcomeStatusSucceeded
		}
		return ActionOutcomeStatusFailed
	}
	if rawError, exists := envelope["error"]; exists && !isEmptyNativeError(rawError) {
		return ActionOutcomeStatusFailed
	}
	return ActionOutcomeStatusUnknown
}

func isEmptyNativeError(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == `""` {
		return true
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return strings.TrimSpace(message) == ""
	}
	return false
}
