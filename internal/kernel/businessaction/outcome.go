package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

const (
	ActionOutcomeStatusSucceeded             = "succeeded"
	ActionOutcomeStatusFailed                = "failed"
	ActionOutcomeStatusUnknown               = "unknown"
	ActionOutcomeSourceForgeMCP              = "forge_mcp.run_action"
	ActionOutcomeSourceDevelopmentSimulation = "development_simulation"
)

var (
	ErrActionOutcomeUnresolved = errors.New("business action outcome remains unresolved")
	ErrActionOperationConflict = errors.New("business action content conflicts with its durable operation identity")
	ErrActionAlreadyRecorded   = errors.New("business action operation already recorded")
)

// ActionOutcomeEvent associates one controlled durable tool slot with its
// frozen business scope. OperationID identifies the operation across retries;
// ParamsSHA256 detects changed content within that operation and is never an
// identity for two distinct operations. Result is a bounded, sanitized receipt,
// not a copy of request parameters, credentials or model-private state.
type ActionOutcomeEvent struct {
	Source             string               `json:"source"`
	RunSnapshotID      string               `json:"run_snapshot_id,omitempty"`
	ActorID            string               `json:"actor_id,omitempty"`
	OperationID        string               `json:"operation_id,omitempty"`
	OperationSlot      string               `json:"operation_slot,omitempty"`
	Phase              string               `json:"phase"`
	InvocationID       string               `json:"invocation_id"`
	CallID             string               `json:"tool_call_id"`
	CapabilityID       string               `json:"capability_id"`
	ActionKey          string               `json:"action_key"`
	ActionLabel        string               `json:"action_label,omitempty"`
	ActionName         string               `json:"action_name"`
	ObjectName         string               `json:"object_name"`
	InputRevisionID    string               `json:"input_revision_id"`
	RecordID           string               `json:"record_id,omitempty"`
	FrozenRecordSHA256 string               `json:"frozen_record_sha256,omitempty"`
	ParamsSHA256       string               `json:"params_sha256,omitempty"`
	Status             string               `json:"status,omitempty"`
	Result             *contract.ToolResult `json:"result,omitempty"`
}

type ActionOutcomeRecorder func(context.Context, ActionOutcomeEvent) error

// ActionOutcomeReplay returns the original receipt for the same durable
// operation. SameParams remains a compatibility field, not a replay identity.
type ActionOutcomeReplay struct {
	Blocked       bool
	Status        string
	SameOperation bool
	SameParams    bool
	Result        *contract.ToolResult
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
		return errors.New("business action outcome recorder is unavailable")
	}
	return recorder(ctx, event)
}

func checkActionOutcomeReplay(ctx context.Context, event ActionOutcomeEvent) (ActionOutcomeReplay, error) {
	guard, _ := ctx.Value(actionOutcomeGuardContextKey{}).(ActionOutcomeGuard)
	if guard == nil {
		return ActionOutcomeReplay{}, errors.New("business action replay guard is unavailable")
	}
	return guard(ctx, event)
}

func classifyNativeActionResult(result *contract.ToolResult) string {
	if result == nil {
		return ActionOutcomeStatusUnknown
	}
	if result.IsError {
		var envelope map[string]json.RawMessage
		if json.Unmarshal([]byte(result.Content), &envelope) == nil {
			if raw, exists := envelope["ok"]; exists {
				var marker *bool
				if json.Unmarshal(raw, &marker) != nil || marker == nil || *marker {
					return ActionOutcomeStatusUnknown
				}
			}
		}
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
	var ok *bool
	if raw, exists := envelope["ok"]; exists {
		if err := json.Unmarshal(raw, &ok); err != nil || ok == nil {
			return ActionOutcomeStatusUnknown
		}
		if *ok {
			if rawError, exists := envelope["error"]; exists && !isEmptyNativeError(rawError) {
				return ActionOutcomeStatusUnknown
			}
			return ActionOutcomeStatusSucceeded
		}
		return ActionOutcomeStatusFailed
	}
	if rawError, exists := envelope["error"]; exists && isExplicitNativeError(rawError) {
		return ActionOutcomeStatusFailed
	}
	return ActionOutcomeStatusUnknown
}

// ValidateActionOutcomeResultStatus rejects inconsistent cached receipts. The
// authoritative Forge marker and MCP error flag must support the saved status.
func ValidateActionOutcomeResultStatus(result *contract.ToolResult, status string) error {
	if result == nil || (status != ActionOutcomeStatusSucceeded && status != ActionOutcomeStatusFailed) ||
		classifyNativeActionResult(result) != status {
		return errors.New("Forge cached business receipt does not match its recorded outcome")
	}
	return nil
}

// Without an authoritative boolean ok marker, only a nonempty error message
// or a structured error with a textual message/code establishes rejection.
// Booleans, numbers, arrays and opaque objects do not establish an outcome.
func isExplicitNativeError(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case map[string]any:
		for _, field := range []string{"message", "code"} {
			if text, ok := value[field].(string); ok && strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
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
