package businessaction

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
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

func TestDispatcherInjectsFrozenMaterialFieldsAndHidesThemFromModel(t *testing.T) {
	host := &captureHost{}
	ids := []string{"forge:action:sales_contract.ContractSubmit"}
	catalog := contractSubmitCatalog()
	catalog["sales_contract.ContractSubmit"] = actionMetadata{
		Name: "ContractSubmit", ObjectName: "sales_contract", RequiresRecord: true,
		Params: []actionParam{
			{Name: "material_file_id", Label: "合同文件", Type: "text", Required: true},
			{Name: "material_name", Type: "string", Required: true},
			{Name: "material_sha256", Type: "string", Required: true},
		},
	}
	resources := []delegatedResource{{Type: "forge-file", ID: "frozen-file-a", Name: "合同.md", Bytes: 64, SHA256: strings.Repeat("a", 64)}, recordResourceForTest("sales_contract", "contract-1")}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: ids[0], Parameters: []frozen.BusinessCapabilityParameterBinding{
		{Name: "material_file_id", Source: frozen.BusinessSourceMaterialID},
		{Name: "material_name", Source: frozen.BusinessSourceMaterialName},
		{Name: "material_sha256", Source: frozen.BusinessSourceMaterialSHA256},
	}}}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, ids, catalog, resources, bindings)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties struct {
			Params struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			} `json:"params"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, visible := schema.Properties.Params.Properties["material_file_id"]; visible || len(schema.Properties.Params.Required) != 0 {
		t.Fatalf("mapped parameter remained model controlled: %s", tools[0].InputSchema)
	}
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "bound-file", Name: tools[0].Name, Args: `{"recordId":"contract-1"}`})
	if err != nil || result == nil || result.IsError || host.call.Name != "run_action" {
		t.Fatalf("result=%+v call=%+v err=%v", result, host.call, err)
	}
	var upstream struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil {
		t.Fatal(err)
	}
	if upstream.Params["material_file_id"] != "frozen-file-a" || upstream.Params["material_name"] != "合同.md" || upstream.Params["material_sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("Forge params were not sourced from the frozen resource: %+v", upstream.Params)
	}
}

func TestDispatcherRejectsModelOverrideOfFrozenMaterialParameter(t *testing.T) {
	host := &captureHost{}
	ids := []string{"forge:action:sales_contract.ContractSubmit"}
	catalog := contractSubmitCatalog()
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: ids[0], Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "material_file_id", Source: frozen.BusinessSourceMaterialID}}}}
	resources := []delegatedResource{{Type: "forge-file", ID: "frozen-file-a", Name: "合同.md", Bytes: 64, SHA256: strings.Repeat("a", 64)}, recordResourceForTest("sales_contract", "contract-1")}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, ids, catalog, resources, bindings)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := dispatcher.ListTools(t.Context())
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "override", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"material_file_id":"another-file","material_sha256":"digest"}}`})
	if err != nil || result == nil || !result.IsError || host.call.Name != "" {
		t.Fatalf("override reached Forge: result=%+v call=%+v err=%v", result, host.call, err)
	}
}

func TestDispatcherRejectsAmbiguousAndUnknownMaterialMappings(t *testing.T) {
	ids := []string{"forge:action:sales_contract.ContractSubmit"}
	catalog := contractSubmitCatalog()
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: ids[0], Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "material_file_id", Source: frozen.BusinessSourceMaterialID}}}}
	resources := []delegatedResource{
		{Type: "forge-file", ID: "file-a", Name: "A.md", Bytes: 1, SHA256: strings.Repeat("a", 64)},
		{Type: "forge-file", ID: "file-b", Name: "B.md", Bytes: 1, SHA256: strings.Repeat("b", 64)},
		recordResourceForTest("sales_contract", "contract-1"),
	}
	if _, err := newDispatcherWithResourcesAndBindings(&captureHost{}, ids, catalog, resources, bindings); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("ambiguous single-file mapping err=%v", err)
	}
	bindings[0].Parameters[0].Name = "missing_field"
	if _, err := newDispatcherWithResourcesAndBindings(&captureHost{}, ids, catalog, []delegatedResource{resources[0], resources[2]}, bindings); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("unknown parameter mapping err=%v", err)
	}
}

func TestDispatcherInjectsOrderedMaterialManifest(t *testing.T) {
	host := &captureHost{}
	id := "forge:action:sales_contract.ContractSubmit"
	catalog := map[string]actionMetadata{"sales_contract.ContractSubmit": {
		Name: "ContractSubmit", ObjectName: "sales_contract", RequiresRecord: true,
		Params: []actionParam{{Name: "attachment_manifest", Type: "text", Required: true}},
	}}
	resources := []delegatedResource{
		{Type: "forge-file", ID: "file-first", Name: "first.txt", Bytes: 1, SHA256: strings.Repeat("a", 64)},
		{Type: "forge-file", ID: "file-second", Name: "second.txt", Bytes: 1, SHA256: strings.Repeat("b", 64)},
		recordResourceForTest("sales_contract", "contract-1"),
	}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "attachment_manifest", Source: frozen.BusinessSourceMaterialsManifest}}}}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, []string{id}, catalog, resources, bindings)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := dispatcher.ListTools(t.Context())
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "manifest", Name: tools[0].Name, Args: `{"recordId":"contract-1"}`})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var upstream struct {
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil {
		t.Fatal(err)
	}
	var manifest []map[string]string
	if err := json.Unmarshal([]byte(upstream.Params["attachment_manifest"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 || manifest[0]["file_id"] != "file-first" || manifest[1]["file_id"] != "file-second" {
		t.Fatalf("manifest order or identity changed: %+v", manifest)
	}
}

func TestDispatcherConstrainsSingleAndMultipleFileParametersToFrozenResources(t *testing.T) {
	id := "forge:action:sales_contract.ContractSubmit"
	catalog := map[string]actionMetadata{"sales_contract.ContractSubmit": {
		Name: "ContractSubmit", ObjectName: "sales_contract", RequiresRecord: true,
		Params: []actionParam{
			{Name: "primary_file_id", Label: "合同正文", Type: "file", Required: true},
			{Name: "material_file_ids", Label: "完整材料集合", Type: "file", Multiple: true, Required: true},
		},
	}}
	rawResources, err := json.Marshal([]delegatedResource{
		{Type: "dispatch-input", ID: "input-revision-1", SHA256: strings.Repeat("c", 64)},
		{Type: "forge-file", SourceKind: "owner", MaterialID: strings.Repeat("1", 24), ID: "file-main", Name: "合同正文.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Bytes: 12, SHA256: strings.Repeat("a", 64)},
		{Type: "forge-file", SourceKind: "owner", MaterialID: strings.Repeat("2", 24), ID: "file-protocol", Name: "技术协议.pdf", MediaType: "application/pdf", Bytes: 12, SHA256: strings.Repeat("b", 64)},
		{Type: "forge-file", SourceKind: "owner", MaterialID: strings.Repeat("3", 24), ID: "file-metrics", Name: "指标附件.pdf", MediaType: "application/pdf", Bytes: 12, SHA256: strings.Repeat("d", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := decodeDelegatedResources(rawResources, "input-revision-1")
	if err != nil {
		t.Fatal(err)
	}
	resources = append(resources, recordResourceForTest("sales_contract", "contract-1"))
	if _, err := newDispatcherWithResources(&captureHost{}, []string{id}, catalog, resources); err == nil || !strings.Contains(err.Error(), "explicit materials.ids binding") {
		t.Fatalf("unbound multiple file parameter was exposed: err=%v", err)
	}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "material_file_ids", Source: frozen.BusinessSourceMaterialIDs}}}}
	host := &captureHost{}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, []string{id}, catalog, resources, bindings)
	if err != nil {
		t.Fatalf("native file parameter contract was rejected: %v", err)
	}
	tools, err := dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	var schema struct {
		Properties struct {
			Params struct {
				Required   []string `json:"required"`
				Properties map[string]struct {
					Type        string   `json:"type"`
					Enum        []string `json:"enum"`
					Description string   `json:"description"`
				} `json:"properties"`
			} `json:"params"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	primary := schema.Properties.Params.Properties["primary_file_id"]
	if primary.Type != "string" || strings.Join(primary.Enum, ",") != "file-main,file-protocol,file-metrics" ||
		!strings.Contains(primary.Description, "合同正文.docx") || strings.Join(schema.Properties.Params.Required, ",") != "primary_file_id" {
		t.Fatalf("single file selector was not limited to this frozen bundle: %s", tools[0].InputSchema)
	}
	if _, visible := schema.Properties.Params.Properties["material_file_ids"]; visible {
		t.Fatalf("multiple file parameter remained model controlled: %s", tools[0].InputSchema)
	}

	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "frozen-files", Name: tools[0].Name, Args: `{"recordId":"contract-1","params":{"primary_file_id":"file-main"}}`})
	if err != nil || result == nil || result.IsError || host.call.Name != "run_action" {
		t.Fatalf("result=%+v call=%+v err=%v", result, host.call, err)
	}
	var upstream struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil {
		t.Fatal(err)
	}
	var primaryID string
	var completeIDs []string
	if err := json.Unmarshal(upstream.Params["primary_file_id"], &primaryID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(upstream.Params["material_file_ids"], &completeIDs); err != nil {
		t.Fatal(err)
	}
	if primaryID != "file-main" || strings.Join(completeIDs, ",") != "file-main,file-protocol,file-metrics" {
		t.Fatalf("Forge did not receive selected primary and complete frozen set: primary=%q ids=%v", primaryID, completeIDs)
	}

	for _, args := range []string{
		`{"recordId":"contract-1","params":{"primary_file_id":"old-file-from-another-task"}}`,
		`{"recordId":"contract-1","params":{"primary_file_id":"file-main","material_file_ids":["old-file-from-another-task"]}}`,
	} {
		host.call = contract.ToolCall{}
		result, err = dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "invalid-file-scope", Name: tools[0].Name, Args: args})
		if err != nil || result == nil || !result.IsError || host.call.Name != "" {
			t.Fatalf("out-of-bundle file or model override reached Forge: args=%s result=%+v call=%+v err=%v", args, result, host.call, err)
		}
	}
	selection := map[string]fileParameterSelection{"primary_file_id": {Required: true, IDs: []string{"file-main", "file-protocol"}}}
	if err := validateFileParameterSelections(map[string]any{"primary_file_id": "old-file-from-another-task"}, selection); err == nil || !strings.Contains(err.Error(), "outside the current frozen material set") {
		t.Fatalf("runtime membership check accepted an old file ID: err=%v", err)
	}
}

func TestDispatcherFailsClosedForRequiredFileWithoutFrozenMaterialsAndOmitsOptionalFile(t *testing.T) {
	id := "forge:action:sales_contract.ContractRead"
	record := recordResourceForTest("sales_contract", "contract-1")
	resources := []delegatedResource{record}
	requiredCatalog := map[string]actionMetadata{"sales_contract.ContractRead": {
		Name: "ContractRead", ObjectName: "sales_contract", Params: []actionParam{{Name: "file_id", Type: "file", Required: true}},
	}}
	if _, err := newDispatcherWithResources(&captureHost{}, []string{id}, requiredCatalog, resources); err == nil || !strings.Contains(err.Error(), "has no current frozen materials") {
		t.Fatalf("required file action was exposed without materials: err=%v", err)
	}
	requiredMultiple := map[string]actionMetadata{"sales_contract.ContractRead": {
		Name: "ContractRead", ObjectName: "sales_contract", Params: []actionParam{{Name: "file_ids", Type: "file", Multiple: true, Required: true}},
	}}
	requiredMultipleBinding := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "file_ids", Source: frozen.BusinessSourceMaterialIDs}}}}
	if _, err := newDispatcherWithResourcesAndBindings(&captureHost{}, []string{id}, requiredMultiple, resources, requiredMultipleBinding); err == nil || !strings.Contains(err.Error(), "has no current frozen materials") {
		t.Fatalf("required multiple file action was exposed without materials: err=%v", err)
	}
	optionalCatalog := map[string]actionMetadata{"sales_contract.ContractRead": {
		Name: "ContractRead", ObjectName: "sales_contract", Params: []actionParam{{Name: "file_id", Type: "file"}},
	}}
	dispatcher, err := newDispatcherWithResources(&captureHost{}, []string{id}, optionalCatalog, resources)
	if err != nil {
		t.Fatalf("optional file action was rejected without materials: %v", err)
	}
	tools, err := dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "file_id") {
		t.Fatalf("optional file without resources was exposed: tools=%+v err=%v", tools, err)
	}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "file_ids", Source: frozen.BusinessSourceMaterialIDs}}}}
	optionalMultiple := map[string]actionMetadata{"sales_contract.ContractRead": {
		Name: "ContractRead", ObjectName: "sales_contract", Params: []actionParam{{Name: "file_ids", Type: "file", Multiple: true}},
	}}
	dispatcher, err = newDispatcherWithResourcesAndBindings(&captureHost{}, []string{id}, optionalMultiple, resources, bindings)
	if err != nil {
		t.Fatalf("optional multiple file binding without materials was rejected: %v", err)
	}
	tools, err = dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "file_ids") {
		t.Fatalf("optional multiple file parameter was exposed: tools=%+v err=%v", tools, err)
	}
}

func TestDispatcherPreservesExplicitSingleFileBindingForUniqueFrozenMaterial(t *testing.T) {
	id := "forge:action:sales_contract.ContractSubmit"
	catalog := map[string]actionMetadata{"sales_contract.ContractSubmit": {
		Name: "ContractSubmit", ObjectName: "sales_contract", RequiresRecord: true,
		Params: []actionParam{{Name: "file_id", Type: "file", Required: true}},
	}}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "file_id", Source: frozen.BusinessSourceMaterialID}}}}
	resources := []delegatedResource{
		{Type: "forge-file", ID: "frozen-file", Name: "合同正文.md", Bytes: 8, SHA256: strings.Repeat("a", 64)},
		recordResourceForTest("sales_contract", "contract-1"),
	}
	host := &captureHost{}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, []string{id}, catalog, resources, bindings)
	if err != nil {
		t.Fatalf("previously supported single-file binding was rejected: %v", err)
	}
	tools, _ := dispatcher.ListTools(t.Context())
	if len(tools) != 1 || strings.Contains(string(tools[0].InputSchema), "file_id") {
		t.Fatalf("explicitly bound single file remained model controlled: tools=%+v", tools)
	}
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "legacy-single-binding", Name: tools[0].Name, Args: `{"recordId":"contract-1"}`})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("explicitly bound single file did not dispatch: result=%+v err=%v", result, err)
	}
	var upstream struct {
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil || upstream.Params["file_id"] != "frozen-file" {
		t.Fatalf("single-file binding did not inject its unique frozen resource: params=%+v err=%v", upstream.Params, err)
	}

	resources = []delegatedResource{
		resources[0],
		{Type: "forge-file", ID: "second-file", Name: "补充材料.md", Bytes: 9, SHA256: strings.Repeat("b", 64)},
		resources[1],
	}
	if _, err := newDispatcherWithResourcesAndBindings(&captureHost{}, []string{id}, catalog, resources, bindings); err == nil || !strings.Contains(err.Error(), "requires exactly one") {
		t.Fatalf("ambiguous explicit single-file binding was allowed: err=%v", err)
	}
}

func TestDevelopmentDispatcherUsesTheSameFrozenFileMappingWithoutForge(t *testing.T) {
	id := "forge:action:sales_contract.ContractSubmit"
	catalog := map[string]actionMetadata{"sales_contract.ContractSubmit": {
		Name: "ContractSubmit", ObjectName: "sales_contract", Params: []actionParam{
			{Name: "primary", Type: "file", Required: true},
			{Name: "files", Type: "file", Multiple: true, Required: true},
		},
	}}
	validatedActions, err := ValidateDevelopmentActions([]string{id}, []DevelopmentAction{{
		CapabilityID: id, Name: "ContractSubmit", ObjectName: "sales_contract", Params: catalog["sales_contract.ContractSubmit"].Params,
	}})
	if err != nil || len(validatedActions) != 1 || !validatedActions[0].Params[1].Multiple {
		t.Fatalf("development action validation dropped the native multiple flag: actions=%+v err=%v", validatedActions, err)
	}
	resources := []delegatedResource{
		{Type: "forge-file", ID: "synthetic-main", Name: "合成主件.txt", Bytes: 8, SHA256: strings.Repeat("a", 64)},
		{Type: "forge-file", ID: "synthetic-support", Name: "合成附件.txt", Bytes: 9, SHA256: strings.Repeat("b", 64)},
	}
	bindings := []frozen.BusinessCapabilityBinding{{CapabilityID: id, Parameters: []frozen.BusinessCapabilityParameterBinding{{Name: "files", Source: frozen.BusinessSourceMaterialIDs}}}}
	dispatcher, err := newDispatcherWithBindings(developmentHost{syntheticMaterialCount: len(resources)}, []string{id}, developmentCatalog(validatedActions), bindings, resources, "隔离调试使用合成材料，不会访问 Forge 或写入业务数据。")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := dispatcher.ListTools(t.Context())
	if err != nil || len(tools) != 1 || !strings.Contains(tools[0].Description, "不会访问 Forge") {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	result, err := dispatcher.Dispatch(t.Context(), contract.ToolCall{ID: "trial-files", Name: tools[0].Name, Args: `{"params":{"primary":"synthetic-main"}}`})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Content, `"simulated":true`) ||
		!strings.Contains(result.Content, "使用 2 份合成材料") || !strings.Contains(result.Content, "未访问 Forge") {
		t.Fatalf("development file mapping was not clearly simulated: result=%+v err=%v", result, err)
	}
	var simulated struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(result.Content), &simulated); err != nil {
		t.Fatal(err)
	}
	var fileIDs []string
	if err := json.Unmarshal(simulated.Params["files"], &fileIDs); err != nil {
		t.Fatal(err)
	}
	if len(fileIDs) != 2 || strings.Join(fileIDs, ",") != "synthetic-main,synthetic-support" {
		t.Fatalf("simulation did not carry both frozen file IDs: %v", fileIDs)
	}
}

func TestDecodeDelegatedResourcesRejectsAnotherInput(t *testing.T) {
	raw, _ := json.Marshal([]delegatedResource{
		{Type: "dispatch-input", ID: "another-input", SHA256: strings.Repeat("a", 64)},
		{Type: "forge-file", ID: "file-1", Name: "合同.md", Bytes: 12, SHA256: strings.Repeat("b", 64)},
	})
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

func TestDispatcherPreservesScalarForgeParameterConstraintsBeforeDispatch(t *testing.T) {
	id := "forge:action:sales_quote.ChangeLinePrice"
	catalog := map[string]actionMetadata{"sales_quote.ChangeLinePrice": {
		Name: "ChangeLinePrice", ObjectName: "sales_quote",
		Params: []actionParam{
			{Name: "status", Type: "string", Required: true, Description: "Current state", Enum: []string{"draft", "approved"}},
			{Name: "unit_price", Type: "number", Required: true},
			{Name: "enabled", Type: "boolean"},
		},
	}}

	host := &captureHost{}
	value, err := newDispatcher(host, []string{id}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := value.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	var schema struct {
		Properties map[string]struct {
			Type       string   `json:"type"`
			Enum       []string `json:"enum"`
			Properties map[string]struct {
				Type        string   `json:"type"`
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	params, ok := schema.Properties["params"]
	if !ok || params.Type != "object" || strings.Join(params.Required, ",") != "status,unit_price" ||
		params.Properties["status"].Type != "string" || strings.Join(params.Properties["status"].Enum, ",") != "draft,approved" ||
		params.Properties["unit_price"].Type != "number" || params.Properties["enabled"].Type != "boolean" {
		t.Fatalf("Forge scalar constraints were not preserved: %s", tools[0].InputSchema)
	}

	for _, args := range []string{
		`{"params":{"unit_price":2.5,"enabled":true}}`,
		`{"params":{"status":"cancelled","unit_price":2.5,"enabled":true}}`,
		`{"params":{"status":"draft","unit_price":"2.5","enabled":true}}`,
		`{"params":{"status":"draft","unit_price":2.5,"enabled":"true"}}`,
		`{"params":{"status":"draft","unit_price":2.5,"enabled":true,"extra":"value"}}`,
	} {
		result, dispatchErr := value.Dispatch(t.Context(), contract.ToolCall{ID: "invalid-args", Name: tools[0].Name, Args: args})
		if dispatchErr != nil || result == nil || !result.IsError || host.call.Name != "" {
			t.Fatalf("invalid arguments reached Forge: args=%s result=%+v call=%+v err=%v", args, result, host.call, dispatchErr)
		}
	}

	result, err := value.Dispatch(t.Context(), contract.ToolCall{ID: "valid-args", Name: tools[0].Name, Args: `{"params":{"status":"draft","unit_price":2.5,"enabled":true}}`})
	if err != nil || result == nil || result.IsError || host.call.Name != "run_action" {
		t.Fatalf("valid scalar arguments were rejected: result=%+v call=%+v err=%v", result, host.call, err)
	}
	var upstream struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(host.call.Args), &upstream); err != nil || upstream.Params["status"] != "draft" || upstream.Params["unit_price"] != 2.5 || upstream.Params["enabled"] != true {
		t.Fatalf("validated values changed before the Forge call: params=%+v err=%v", upstream.Params, err)
	}
}

func TestDispatcherRejectsArrayWithoutForgeItemSchema(t *testing.T) {
	id := "forge:action:sales_quote.ReplaceLines"
	metadata := actionMetadata{Name: "ReplaceLines", ObjectName: "sales_quote", Params: []actionParam{{Name: "lines", Type: "array", Required: true}}}
	_, err := newDispatcher(&captureHost{}, []string{id}, map[string]actionMetadata{"sales_quote.ReplaceLines": metadata})
	if err == nil || !strings.Contains(err.Error(), `array parameter "lines" has no item schema`) {
		t.Fatalf("array without item schema was exposed: err=%v", err)
	}
	_, err = ValidateDevelopmentActions([]string{id}, []DevelopmentAction{{
		CapabilityID: id, Name: "ReplaceLines", ObjectName: "sales_quote", Params: metadata.Params,
	}})
	if err == nil || !strings.Contains(err.Error(), `array parameter "lines" has no item schema`) {
		t.Fatalf("array without item schema was allowed in a development trial: err=%v", err)
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
	host := &captureHost{result: &contract.ToolResult{Content: `{"actions":[{"name":"ContractSubmit","objectName":"sales_contract","description":"提交指定版本","requiresRecord":true,"params":[{"name":"primary_file_id","type":"file","required":true},{"name":"material_file_ids","type":"file","multiple":true,"required":true}]}]}`}}
	catalog, err := readActionCatalog(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := catalog["sales_contract.ContractSubmit"]
	if !ok || !item.RequiresRecord || len(item.Params) != 2 || item.Params[0].Type != "file" ||
		!item.Params[1].Multiple || item.Params[1].Type != "file" || !item.Params[1].Required ||
		host.call.Name != "list_actions" || host.call.Args != `{}` {
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
