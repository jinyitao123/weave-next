package businessaction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

const nativeConfirmationSchema = `{"type":"object","properties":{"actionName":{"type":"string"},"objectName":{"type":"string"},"recordId":{"type":"string"},"params":{"type":"object"},"confirm":{"type":"boolean"}},"required":["actionName"],"additionalProperties":false}`

func TestNativeConfirmationRequiresAnExplicitBooleanProtocol(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{"confirm":{"type":"string"}}}`,
		`{"type":"object","properties":{"confirm":true}}`,
		`{"type":"object","properties":{"confirm":{"type":["boolean","null"]}}}`,
	} {
		if _, _, err := nativeConfirmationContract([]contract.ToolDef{{Name: "run_action", InputSchema: json.RawMessage(schema)}}); !errors.Is(err, mcphost.ErrFailClosed) {
			t.Fatalf("unsupported confirmation protocol accepted: %v", err)
		}
	}
	tools := []contract.ToolDef{{Name: "run_action", InputSchema: json.RawMessage(nativeConfirmationSchema)}}
	bound, digest, err := nativeConfirmationContract(tools)
	if err != nil || digest == "" {
		t.Fatal("declared native boolean confirmation rejected", err)
	}
	call, err := projectNativeConfirmation(contract.ToolCall{ID: "original", Name: "run_action", Args: `{"actionName":"AdjustPrice","objectName":"sales_quote","recordId":"record-a","params":{}}`})
	if err != nil || bound.Validate(call) != nil || call.ID != "original" || !strings.Contains(call.Args, `"confirm":true`) {
		t.Fatal("system confirmation was not projected into the declared native transport", err)
	}
	if _, _, err := nativeConfirmationContract(append(tools, tools[0])); !errors.Is(err, mcphost.ErrFailClosed) {
		t.Fatal("ambiguous native confirmation tool accepted")
	}
	for _, value := range []string{"true", "false", "null"} {
		if _, err := projectNativeConfirmation(contract.ToolCall{Args: `{"confirm":` + value + `}`}); !errors.Is(err, mcphost.ErrFailClosed) {
			t.Fatal("caller confirmation was accepted")
		}
	}
}

func TestNativeConfirmationCannotBeBoundFromModelOrMaterials(t *testing.T) {
	id := "forge:action:sales_quote.AdjustPrice"
	metadata := actionMetadata{Name: "AdjustPrice", ObjectName: "sales_quote", RequiresConfirmation: true}
	for _, param := range []actionParam{{Name: "confirm", Type: "boolean"}, {Field: "confirm", Type: "text"}} {
		metadata.Params = []actionParam{param}
		if _, err := newDispatcher(&captureHost{}, []string{id}, map[string]actionMetadata{"sales_quote.AdjustPrice": metadata}); !errors.Is(err, mcphost.ErrFailClosed) {
			t.Fatal("native confirmation became a model parameter")
		}
		bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "confirm", Source: frozen.BusinessSourceMaterialID}}}}
		if _, err := newDispatcherWithResourcesAndBindings(&captureHost{}, []string{id}, map[string]actionMetadata{"sales_quote.AdjustPrice": metadata}, nil, bindings); !errors.Is(err, mcphost.ErrFailClosed) {
			t.Fatal("native confirmation became a material binding")
		}
	}
}

func TestNativeConfirmationMetadataIsStrictAndSimulationStaysIsolated(t *testing.T) {
	for _, value := range []string{"null", `"true"`, "1"} {
		host := &captureHost{result: &contract.ToolResult{Content: `{"actions":[{"name":"AdjustPrice","objectName":"sales_quote","requiresRecord":false,"requiresConfirmation":` + value + `,"params":[]}]}`}}
		if _, err := readActionCatalog(context.Background(), host, []string{"forge:action:sales_quote.AdjustPrice"}, nil); !errors.Is(err, mcphost.ErrFailClosed) {
			t.Fatal("invalid confirmation metadata accepted", err)
		}
	}
	id := "forge:action:sales_quote.AdjustPrice"
	d, err := newDevelopmentDispatcherWithBindings([]string{id}, []DevelopmentAction{{CapabilityID: id, Name: "AdjustPrice", ObjectName: "sales_quote", RequiresConfirmation: true, SimulationAuthorized: true}}, nil, "simulation-input")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := d.ListTools(t.Context())
	if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "confirm") {
		t.Fatal("confirmation leaked into the simulation tool")
	}
	var events []ActionOutcomeEvent
	ctx := outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
		return ActionOutcomeReplay{}, nil
	}, nil)
	result, err := d.Dispatch(ctx, contract.ToolCall{ID: "simulation", Name: tools[0].Name, Args: `{}`})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Content, `"simulated":true`) || strings.Contains(result.Content, "confirm") || len(events) != 2 || events[1].Source != ActionOutcomeSourceDevelopmentSimulation {
		t.Fatal("native confirmation reached the isolated simulation", err)
	}
}

func TestObjectStack173ConversionAndPriceRequestsKeepExactBytes(t *testing.T) {
	// Public @objectstack/mcp@17.3.0 registerActionTools capture. The two action
	// definitions in Forge 689bf3c4 declare ai.requiresConfirmation=false.
	raw, err := os.ReadFile("testdata/objectstack-17.3-run-action.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	native, err := mcphost.NewToolContract([]contract.ToolDef{{Name: "run_action", InputSchema: raw}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		object, action, args, want string
		params                     []actionParam
	}{
		{object: "forge_sales_lead", action: "sales_lead_convert_to_opportunity", args: `{"params":{"amount":2300}}`,
			params: []actionParam{{Name: "amount", Type: "number", Required: true}, {Name: "expected_close_on", Type: "string"}},
			want:   `{"actionName":"sales_lead_convert_to_opportunity","objectName":"forge_sales_lead","params":{"amount":2300},"recordId":"record-a"}`},
		{object: "forge_quotation", action: "quotation_adjust_line_price", args: `{"params":{"line_id":"line-a","expected_pricing_version":0,"taxed_unit_price":900}}`,
			params: []actionParam{{Name: "line_id", Type: "text", Required: true}, {Name: "expected_pricing_version", Type: "number", Required: true}, {Name: "taxed_unit_price", Type: "number", Required: true}, {Name: "idempotency_key", Type: "text", Required: true}},
			want:   `{"actionName":"quotation_adjust_line_price","objectName":"forge_quotation","params":{"expected_pricing_version":0,"idempotency_key":"SYSTEM_OPERATION","line_id":"line-a","taxed_unit_price":900},"recordId":"record-a"}`},
	} {
		t.Run(test.action, func(t *testing.T) {
			host := &captureHost{}
			id := capabilityPrefix + test.object + "." + test.action
			catalog := map[string]actionMetadata{test.object + "." + test.action: {Name: test.action, ObjectName: test.object, RequiresRecord: true, RequiresConfirmation: false, Params: test.params}}
			d, err := newDispatcherWithResources(host, []string{id}, catalog, []delegatedResource{recordResourceForTest(test.object, "record-a")})
			if err != nil {
				t.Fatal(err)
			}
			d.trackOutcomes, d.inputRevisionID = true, "revision-1"
			var events []ActionOutcomeEvent
			ctx := outcomeTestContext(&events, func(context.Context, ActionOutcomeEvent) (ActionOutcomeReplay, error) {
				return ActionOutcomeReplay{}, nil
			}, nil)
			want := strings.ReplaceAll(test.want, "SYSTEM_OPERATION", execution.EngineOperationID("revision-1", execution.InvocationID(ctx), execution.OperationID(ctx), id))
			tools, _ := d.ListTools(t.Context())
			result, err := d.Dispatch(ctx, contract.ToolCall{ID: "old-native-call", Name: tools[0].Name, Args: test.args})
			if err != nil || result == nil || result.IsError || host.call.Args != want || native.Validate(host.call) != nil {
				t.Fatal("17.3 action transport bytes or closed native schema compatibility changed", err)
			}
		})
	}
}
