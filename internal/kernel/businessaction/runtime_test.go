package businessaction

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type captureHost struct {
	call   contract.ToolCall
	result *contract.ToolResult
}

func (h *captureHost) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (h *captureHost) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	h.call = call
	if h.result != nil {
		return h.result, nil
	}
	return &contract.ToolResult{CallID: call.ID, Content: `{"ok":true}`}, nil
}

func contractSubmitCatalog() map[string]actionMetadata {
	return map[string]actionMetadata{
		"sales_contract.ContractSubmit": {
			Name: "ContractSubmit", ObjectName: "sales_contract", Label: "提交指定合同版本",
			Description: "把员工授权的合同版本提交审批。", RequiresRecord: true,
			Params: []actionParam{
				{Name: "material_file_id", Type: "string", Required: true, Description: "合同文件"},
				{Name: "material_sha256", Type: "string", Required: true, Description: "文件摘要"},
			},
		},
	}
}

func TestDispatcherFixesPublishedForgeAction(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties struct {
			Params struct {
				Required []string `json:"required"`
			} `json:"params"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(schema.Required, ","), "recordId") ||
		!strings.Contains(strings.Join(schema.Properties.Params.Required, ","), "material_file_id") {
		t.Fatalf("schema=%s", tools[0].InputSchema)
	}
	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "call-1", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"material_file_id":"file-1","material_sha256":"digest-1"}}`})
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

func TestDispatcherProjectsVerifiedTaskResourcesOnlyToBusinessTool(t *testing.T) {
	host := &captureHost{}
	resources := []delegatedResource{{
		Type: "forge-file", ID: "file-contract", Name: "合同.md", Bytes: 128, SHA256: strings.Repeat("a", 64),
	}, {
		Type: "forge-file", ID: "file-quote", Name: "报价单.md", Bytes: 64, SHA256: strings.Repeat("b", 64),
	}}
	resources = append(resources, recordResourceForTest("sales_contract", "contract-1"))
	value, err := newDispatcherWithResources(host, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog(), resources)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	for _, expected := range []string{"file-contract", "合同.md", strings.Repeat("a", 64), "file-quote", "报价单.md", strings.Repeat("b", 64)} {
		if !strings.Contains(tools[0].Description, expected) {
			t.Fatalf("business tool description did not project %q: %s", expected, tools[0].Description)
		}
	}
}

func TestDecodeDelegatedResourcesRejectsAnotherInput(t *testing.T) {
	raw := []byte(`[{"type":"dispatch-input","id":"another-input","sha256":"digest"},{"type":"forge-file","id":"file-1","name":"合同.md","bytes":12,"sha256":"digest"}]`)
	if _, err := decodeDelegatedResources(raw, "current-input"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
}

func TestDispatcherRejectsActionOverride(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := value.ListTools(t.Context())
	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "call-2", Name: tools[0].Name, Args: `{"actionName":"DeleteEverything","recordId":"contract-1"}`})
	if err != nil || result == nil || !result.IsError || host.call.Name != "" {
		t.Fatalf("result=%+v captured=%+v err=%v", result, host.call, err)
	}
}

func TestDispatcherRejectsMissingRequiredForgeParamsBeforeDispatch(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := value.ListTools(t.Context())
	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "call-required", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"material_file_id":"file-1"}}`})
	if err != nil || result == nil || !result.IsError || host.call.Name != "" {
		t.Fatalf("result=%+v captured=%+v err=%v", result, host.call, err)
	}
}

func TestTaskScopeCanExposeOnlyOnePublishedForgeAction(t *testing.T) {
	host := &captureHost{}
	value, err := newDispatcher(host, []string{"forge:action:sales_contract.ContractSubmit"}, contractSubmitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 || strings.Contains(tools[0].Name, "RequestRevision") {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
}

func TestDispatcherFailsClosedWhenPublishedActionIsNotVisibleToEmployee(t *testing.T) {
	_, err := newDispatcher(&captureHost{}, []string{"forge:action:sales_contract.ContractSubmit"}, map[string]actionMetadata{})
	if err == nil || !strings.Contains(err.Error(), "unavailable to the current employee") {
		t.Fatalf("err=%v", err)
	}
}

func TestReadActionCatalogUsesEmployeeVisibleForgeMetadata(t *testing.T) {
	host := &captureHost{result: &contract.ToolResult{Content: `{"actions":[{"name":"ContractSubmit","objectName":"sales_contract","description":"提交指定版本","requiresRecord":true,"params":[{"name":"material_file_id","type":"string","required":true}]}]}`}}
	catalog, err := readActionCatalog(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := catalog["sales_contract.ContractSubmit"]
	if !ok || !item.RequiresRecord || len(item.Params) != 1 || host.call.Name != "list_actions" || host.call.Args != `{}` {
		t.Fatalf("catalog=%+v call=%+v", catalog, host.call)
	}
}

func TestTaskScopeIsIntersectedPerMember(t *testing.T) {
	member := []string{"forge:action:sales_contract.ContractSubmit"}
	task := []string{"forge:action:sales_contract.RequestRevision", "forge:action:sales_contract.ContractSubmit"}
	got := intersectActions(member, task)
	if len(got) != 1 || got[0] != member[0] {
		t.Fatalf("intersection=%v", got)
	}
	if got := intersectActions(member, nil); len(got) != 0 {
		t.Fatalf("empty task scope exposed actions: %v", got)
	}
}

func TestDevelopmentDispatcherUsesExactSchemaWithoutCallingForge(t *testing.T) {
	actions, err := ValidateDevelopmentActions([]string{"forge:action:sales_contract.ContractSubmit"}, []DevelopmentAction{{
		CapabilityID: "forge:action:sales_contract.ContractSubmit", Name: "ContractSubmit", ObjectName: "sales_contract",
		Label: "提交指定合同版本", Description: "提交冻结版本", RequiresRecord: true,
		Params: []actionParam{{Name: "material_file_id", Type: "string", Required: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := newDevelopmentDispatcher([]string{"forge:action:sales_contract.ContractSubmit"}, actions)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 || !strings.Contains(tools[0].Description, "不会访问 Forge") {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "trial-call", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"material_file_id":"file-1"}}`})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Content, `"simulated":true`) || !strings.Contains(result.Content, "未写入业务数据") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDevelopmentActionCatalogCannotWidenCandidateCapabilities(t *testing.T) {
	_, err := ValidateDevelopmentActions([]string{"forge:action:sales_contract.ContractSubmit"}, []DevelopmentAction{{
		CapabilityID: "forge:action:sales_contract.Delete", Name: "Delete", ObjectName: "sales_contract",
	}})
	if err == nil || !strings.Contains(err.Error(), "不属于当前团队配置") {
		t.Fatalf("err=%v", err)
	}
}
