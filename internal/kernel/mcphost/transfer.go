package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

type TransferDispatcher struct {
	selfName string
	routes   *compiler.SubAgentRouteTable
}

func NewTransferDispatcher(selfName string, routes *compiler.SubAgentRouteTable) *TransferDispatcher {
	return &TransferDispatcher{selfName: selfName, routes: routes}
}
func (d *TransferDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	if d == nil || len(d.routes.Routes()) == 0 {
		return nil, nil
	}
	var catalog strings.Builder
	for _, route := range d.routes.Routes() {
		fmt.Fprintf(&catalog, "\n- %s: %s", route.Canonical, route.Ref.Description)
	}
	return []contract.ToolDef{{Name: "transfer_to", Description: "Transfer this conversation to a listed sub-agent. After acceptance, the target takes over the final response. Available targets:" + catalog.String(), InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"message":{"type":"string"}},"required":["agent"]}`), ReadOnly: false}}, nil
}
func (d *TransferDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != "transfer_to" {
		return nil, fmt.Errorf("unsupported tool %q", call.Name)
	}
	var input struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return nil, fmt.Errorf("transfer_to: invalid arguments: %w", err)
	}
	route, ok := d.routes.Resolve(input.Agent)
	if !ok {
		return nil, fmt.Errorf("transfer_to: agent %q does not exist in the authorized route table", input.Agent)
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "Transfer accepted.", StatePatch: map[string]any{"__delegate_to": route.Canonical}, StopLoop: true}, nil
}

var _ contract.ToolDispatcher = (*TransferDispatcher)(nil)
