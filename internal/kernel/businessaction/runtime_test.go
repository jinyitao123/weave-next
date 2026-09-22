package businessaction

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type captureHost struct{ call contract.ToolCall }

func (h *captureHost) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (h *captureHost) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	h.call = call
	return &contract.ToolResult{CallID: call.ID, Content: `{"ok":true}`}, nil
}

func TestDispatcherFixesPublishedForgeAction(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "call-1", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"note":"ready"}}`})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if host.call.Name != "run_action" {
		t.Fatalf("upstream tool=%q", host.call.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(host.call.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args["actionName"] != "ContractSubmit" || args["objectName"] != "sales_contract" || args["recordId"] != "contract-1" {
		t.Fatalf("upstream args=%v", args)
	}
}

func TestDispatcherRejectsActionOverride(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := value.ListTools(t.Context())
	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "call-2", Name: tools[0].Name, Args: `{"actionName":"DeleteEverything","recordId":"contract-1"}`})
	if err != nil || result == nil || !result.IsError || host.call.Name != "" {
		t.Fatalf("result=%+v captured=%+v err=%v", result, host.call, err)
	}
}
