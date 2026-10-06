package businessaction

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

// Confirmation belongs to the native transport, after the controlled dispatcher
// has fixed the business request and its durable operation digest. It never
// changes the business parameters, operation identity or replayed receipt.
func (h *taskScopedActionHost) bindNativeConfirmations(ctx context.Context, catalog map[string]actionMetadata) error {
	required := make(map[string]struct{})
	for key, action := range catalog {
		if action.RequiresConfirmation {
			required[key] = struct{}{}
		}
	}
	if len(required) == 0 {
		return nil
	}
	tools, err := h.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("%w: native action confirmation protocol unavailable", mcphost.ErrFailClosed)
	}
	_, digest, err := nativeConfirmationContract(tools)
	if err != nil {
		return err
	}
	h.confirmations = required
	h.confirmationSchemaDigest = digest
	return nil
}

func nativeConfirmationContract(tools []contract.ToolDef) (*mcphost.ToolContract, string, error) {
	var selected []contract.ToolDef
	for _, tool := range tools {
		if tool.Name == "run_action" {
			selected = append(selected, tool)
		}
	}
	if len(selected) != 1 {
		return nil, "", fmt.Errorf("%w: native action confirmation protocol unavailable", mcphost.ErrFailClosed)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var confirmation struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(selected[0].InputSchema, &schema) != nil || json.Unmarshal(schema.Properties["confirm"], &confirmation) != nil || confirmation.Type != "boolean" {
		return nil, "", fmt.Errorf("%w: native action confirmation is not declared", mcphost.ErrFailClosed)
	}
	bound, err := mcphost.NewToolContract(selected)
	if err != nil {
		return nil, "", err
	}
	digest, err := frozen.HashCanonicalJSON(selected[0].InputSchema)
	return bound, digest, err
}

func (h *taskScopedActionHost) requiresNativeConfirmation(call contract.ToolCall) bool {
	if call.Name != "run_action" || len(h.confirmations) == 0 {
		return false
	}
	var input struct {
		ActionName string `json:"actionName"`
		ObjectName string `json:"objectName"`
	}
	if json.Unmarshal([]byte(call.Args), &input) != nil {
		return false
	}
	_, required := h.confirmations[input.ObjectName+"."+input.ActionName]
	return required
}

func validateNativeConfirmationScope(call contract.ToolCall, bound delegation) error {
	var input struct {
		ActionName string `json:"actionName"`
		ObjectName string `json:"objectName"`
		RecordID   string `json:"recordId"`
	}
	if json.Unmarshal([]byte(call.Args), &input) != nil {
		return fmt.Errorf("%w: native action confirmation scope invalid", mcphost.ErrFailClosed)
	}
	allowed := false
	for _, capability := range bound.actions {
		if capability == capabilityPrefix+input.ObjectName+"."+input.ActionName {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: native action confirmation outside task scope", mcphost.ErrFailClosed)
	}
	if input.RecordID != "" {
		for _, resource := range bound.resources {
			if resource.Type == "forge-record" && resource.ObjectName == input.ObjectName && resource.ID == input.RecordID {
				return nil
			}
		}
		return fmt.Errorf("%w: native action confirmation outside record scope", mcphost.ErrFailClosed)
	}
	return nil
}

func projectNativeConfirmation(call contract.ToolCall) (contract.ToolCall, error) {
	var input map[string]json.RawMessage
	if json.Unmarshal([]byte(call.Args), &input) != nil || input == nil {
		return call, fmt.Errorf("%w: native action confirmation input invalid", mcphost.ErrFailClosed)
	}
	if _, supplied := input["confirm"]; supplied {
		return call, fmt.Errorf("%w: native action confirmation is system-owned", mcphost.ErrFailClosed)
	}
	input["confirm"] = json.RawMessage(`true`)
	raw, err := json.Marshal(input)
	if err != nil {
		return call, err
	}
	call.Args = string(raw)
	return call, nil
}
