package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func editTestMember(id, name, role string) developmentMember {
	member := developmentMember{ID: id}
	member.Configuration = teamMemberAgentConfiguration{DisplayName: name, Role: role, Engine: "loom", Model: "deepseek-flash", SystemPrompt: name + "的工作方法"}
	member.Relationship = teamMemberRelationshipDraft{Duty: name + "的职责", AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "consult", Enabled: true}
	return member
}

func editTestDocument() developmentDocument {
	return developmentDocument{Name: "线索跟进团队", Objective: "整理销售线索材料，判断是否值得跟进", Members: []developmentMember{
		editTestMember("00000000-0000-4000-8000-000000000001", "负责人", "avatar"),
		editTestMember("00000000-0000-4000-8000-000000000002", "整理员", "worker"),
		editTestMember("00000000-0000-4000-8000-000000000003", "核对员", "worker"),
	}, Workflows: []developmentWorkflow{}}
}

var editTestCatalog = []catalogCapability{
	{ID: "forge:action:crm_lead.convert", Name: "线索转商机", Effect: "write", Status: "available"},
	{ID: "forge:action:crm_lead.read", Name: "读取线索", Effect: "read", Status: "available"},
	{ID: "forge:action:crm_approval.approve", Name: "审批同意", Effect: "write", Status: "available", ExecutionMode: "employee_only"},
	{ID: "forge:action:crm_pack.submit", Name: "提交材料包", Effect: "write", Status: "available", Params: []catalogParameter{
		{Name: "primary_file_id", Type: "file"}, {Name: "material_file_ids", Type: "file", Multiple: true},
		{Name: "note", Type: "string"}, {Name: "idempotency_key", Type: "string"},
	}},
}

func editOps(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var operations []map[string]any
	if err := json.Unmarshal([]byte(raw), &operations); err != nil {
		t.Fatalf("operations: %v", err)
	}
	return operations
}

func editApply(t *testing.T, doc *developmentDocument, raw string) []string {
	t.Helper()
	changes, err := applyDevelopmentOperations(doc, editOps(t, raw), editTestCatalog)
	if err != nil {
		t.Fatalf("apply %s: %v", raw, err)
	}
	return changes
}

func editRefusal(t *testing.T, doc developmentDocument, raw string) *editError {
	t.Helper()
	encoded, _ := json.Marshal(doc)
	var copied developmentDocument
	_ = json.Unmarshal(encoded, &copied)
	_, err := applyDevelopmentOperations(&copied, editOps(t, raw), editTestCatalog)
	var refusal *editError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a refusal for %s, got %v", raw, err)
	}
	return refusal
}

// editKernelGraph proves the kernel accepts the graph the edits produced.
func editKernelGraph(t *testing.T, flow developmentWorkflow) machine.GraphDefinition {
	t.Helper()
	graph, report := machine.DecodeGraphDefinitionV1(flow.Graph)
	if report != nil && len(report.Issues) > 0 {
		t.Fatalf("kernel rejects the edited graph: %s\n%s", report.Issues[0].Message, flow.Graph)
	}
	return graph
}

func editGraphOf(t *testing.T, flow developmentWorkflow) editGraph {
	t.Helper()
	graph, err := decodeEditGraph(flow.Graph)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func editLabels(graph editGraph) []string {
	var labels []string
	for _, node := range editItems(graph, "nodes") {
		labels = append(labels, editString(node["label"]))
	}
	return labels
}

func editLabelled(t *testing.T, graph editGraph, label string) map[string]any {
	t.Helper()
	for _, node := range editItems(graph, "nodes") {
		if editString(node["label"]) == label {
			return node
		}
	}
	t.Fatalf("no step %q in %v", label, editLabels(graph))
	return nil
}

const editBusinessTeam = `[
	{"kind":"flow_add","name":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论","member":"整理员"},
	{"kind":"member_add","ref":"submitter","name":"提交员","duty":"把值得跟进的线索转为商机"},
	{"kind":"step_add","flow":"线索跟进流程","after":"整理员","member":"submitter","name":"提交转化","requirement":"按结论办理线索转商机"},
	{"kind":"capability","member":"submitter","capability":"线索转商机","selected":true},
	{"kind":"result_protocol","flow":"线索跟进流程","enabled":true},
	{"kind":"business_completion","flow":"线索跟进流程","capabilities":["线索转商机"],"allowNeedsInput":true}
]`

func TestControlledEditsBuildABusinessTeamTheKernelAccepts(t *testing.T) {
	doc := editTestDocument()
	changes := editApply(t, &doc, editBusinessTeam)
	want := []string{"新增流程：线索跟进流程", "新增成员：提交员", "新增串行步骤：提交转化", "添加业务动作：提交员 · 线索转商机", "启用团队结果分类：线索跟进流程", "设置必须办成的业务动作：线索跟进流程"}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v", changes)
	}
	if err := validateDevelopmentDocument(doc); err != nil {
		t.Fatalf("document: %v", err)
	}
	added := doc.Members[3]
	if added.Configuration.DisplayName != "提交员" || added.Configuration.Model != "deepseek-flash" || !reflect.DeepEqual(added.Configuration.BusinessCapabilityIDs, []string{"forge:action:crm_lead.convert"}) || !added.Relationship.Enabled {
		t.Fatalf("added member = %+v", added)
	}
	kernel := editKernelGraph(t, doc.Workflows[0])
	check, err := deliverycheck.BusinessReceiptCheck(kernel.DeliveryContract)
	if err != nil || check == nil {
		t.Fatalf("receipt check = %v, %v", check, err)
	}
	params, err := deliverycheck.ParseBusinessReceiptParameters(check.Parameters)
	if err != nil || !reflect.DeepEqual(params.RequiredCapabilityIDs, []string{"forge:action:crm_lead.convert"}) {
		t.Fatalf("receipt parameters = %+v, %v", params, err)
	}
	graph := editGraphOf(t, doc.Workflows[0])
	if !reflect.DeepEqual(editLabels(graph), []string{"理解任务", "整理员", "交付结果", "提交转化"}) {
		t.Fatalf("steps = %v", editLabels(graph))
	}
	last := editLabelled(t, graph, "提交转化")
	if editResultSource(editDeliveryNode(graph)) != editString(last["id"]) || !editProtocolOutput(editObject(last["output"])) || editString(editObject(last["config"])["agent_id"]) != added.ID {
		t.Fatalf("delivery source = %v", last)
	}
	if issue := editResultProtocolIssue(graph); issue != "" {
		t.Fatal(issue)
	}

	// A step added after the classified one takes over the classification.
	editApply(t, &doc, `[{"kind":"step_add","flow":"线索跟进流程","after":"提交转化","member":"负责人","name":"汇总结论","requirement":"汇总并说明是否需要补充材料"}]`)
	graph = editGraphOf(t, doc.Workflows[0])
	summary := editLabelled(t, graph, "汇总结论")
	if editNodeType(summary) != "lead" || !editProtocolOutput(editObject(summary["output"])) || !editPlainText(editLabelled(t, graph, "提交转化")["output"]) {
		t.Fatalf("classification did not move: %v", summary)
	}
	editKernelGraph(t, doc.Workflows[0])

	// Unbinding the action while the flow still requires it is refused.
	refusal := editRefusal(t, doc, `[{"kind":"capability","member":"提交员","capability":"线索转商机","selected":false}]`)
	if !strings.Contains(refusal.Message, "尚未绑定到该流程的已启用成员") {
		t.Fatalf("refusal = %s", refusal.Message)
	}
}

func TestControlledEditsKeepParallelRegionsWhole(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, `[
		{"kind":"flow_add","name":"复核流程","description":"复核合同","member":"整理员"},
		{"kind":"step_add","flow":"复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"核对付款条件","placement":"parallel"},
		{"kind":"step_add","flow":"复核流程","after":"并行分工","member":"负责人","name":"汇总检查结果","requirement":"汇总分支结论和原文依据"},
		{"kind":"join","flow":"复核流程","step":"汇总分支","policy":"quorum","successCount":2},
		{"kind":"step_input","flow":"复核流程","step":"汇总检查结果","source":"node_output","from":"核对条款","selected":true},
		{"kind":"step_input","flow":"复核流程","step":"汇总检查结果","source":"run_input","selected":false}
	]`)
	editKernelGraph(t, doc.Workflows[0])
	graph := editGraphOf(t, doc.Workflows[0])
	summary, join := editLabelled(t, graph, "汇总检查结果"), editLabelled(t, graph, "汇总分支")
	if editResultSource(editDeliveryNode(graph)) != editString(summary["id"]) || editNumber(editObject(join["config"])["success_count"]) != 2 {
		t.Fatalf("summary = %v join = %v", summary, join)
	}
	inputs := editObject(summary["inputs"])
	if _, original := inputs["original"]; original || len(inputs) != 2 {
		t.Fatalf("inputs = %v", inputs)
	}
	if kind := editString(editObject(editLabelled(t, graph, "整理员")["config"])["kind"]); kind != "dispatch" {
		t.Fatalf("first branch kind = %s", kind)
	}
	for raw, message := range map[string]string{
		`[{"kind":"step_remove","flow":"复核流程","step":"核对条款"}]`:                                                                     "输入来源",
		`[{"kind":"step_add","flow":"复核流程","after":"核对条款","member":"负责人","name":"再看一遍","requirement":"复查"}]`:                       "并行分支由单个成员直接进入汇合",
		`[{"kind":"join","flow":"复核流程","step":"汇总分支","policy":"quorum","successCount":3}]`:                                         "所需成功分支数无效",
		`[{"kind":"step_add","flow":"复核流程","after":"并行分工","member":"负责人","name":"并行汇总","requirement":"x","placement":"parallel"}]`: "并行分支须选择已启用的执行成员",
		`[{"kind":"delivery","flow":"复核流程","from":"交付结果"}]`:                                                                        "交付来源必须是当前流程的前序步骤",
		`[{"kind":"member_remove","member":"核对员"}]`:                                                                                "仍被流程“复核流程”的步骤“核对条款”使用",
	} {
		if refusal := editRefusal(t, doc, raw); !strings.Contains(refusal.Message, message) {
			t.Errorf("%s: %s", raw, refusal.Message)
		}
	}
	// Once nothing reads the branch, it still cannot leave a region of two.
	editApply(t, &doc, `[{"kind":"step_input","flow":"复核流程","step":"汇总检查结果","source":"node_output","from":"核对条款","selected":false}]`)
	if refusal := editRefusal(t, doc, `[{"kind":"step_remove","flow":"复核流程","step":"核对条款"}]`); !strings.Contains(refusal.Message, "至少保留两个分支") {
		t.Fatalf("refusal = %s", refusal.Message)
	}
}

func TestControlledEditsRefuseTheWholeGroupAndNameTheItem(t *testing.T) {
	doc := editTestDocument()
	refusal := editRefusal(t, doc, `[{"kind":"team","objective":"新的目标"},{"kind":"member","member":"不存在的人","duty":"x"}]`)
	if refusal.Index != 1 || !strings.Contains(refusal.Message, "不在当前团队草稿中") {
		t.Fatalf("refusal = %+v", refusal)
	}
	for raw, message := range map[string]string{
		`[{"kind":"graph_patch","path":"/nodes/0"}]`:                                  "不支持“graph_patch”",
		`[{"kind":"member","member":"负责人","enabled":false}]`:                          "负责人不能停用",
		`[{"kind":"member","member":"整理员"}]`:                                          "成员修改缺少目标字段",
		`[{"kind":"member","member":"整理员","name":"核对员"}]`:                             "已被占用",
		`[{"kind":"member","member":"整理员","outputSchema":"[1]"}]`:                     "JSON Schema 对象",
		`[{"kind":"member_remove","member":"负责人"}]`:                                   "必须保留负责人",
		`[{"kind":"capability","member":"整理员","capability":"审批同意","selected":true}]`:  "不能绑定给团队成员",
		`[{"kind":"capability","member":"整理员","capability":"没有的动作","selected":true}]`: "不在当前业务动作目录中",
		`[{"kind":"capability","member":"整理员","capability":"提交材料包","selected":true}]`: "多文件参数 material_file_ids 必须绑定",
		`[{"kind":"capability","member":"整理员","capability":"提交材料包","selected":true,"parameterSources":[{"name":"material_file_ids","source":"materials.ids"},{"name":"idempotency_key","source":"materials.single.name"}]}]`: "系统托管",
		`[{"kind":"capability","member":"整理员","capability":"提交材料包","selected":true,"parameterSources":[{"name":"material_file_ids","source":"materials.ids"},{"name":"primary_file_id","source":"materials.single.name"}]}]`: "材料来源与原生类型不匹配",
		`[{"kind":"flow","flow":"没有的流程","name":"x"}]`:                                          "不在当前团队草稿中",
		`[{"kind":"flow_add","name":"流程","description":"说明","member":"负责人"}]`:                  "已启用的执行成员",
		`[{"kind":"business_completion","flow":"x","capabilities":[],"allowNeedsInput":true}]`: "不在当前团队草稿中",
	} {
		if refusal := editRefusal(t, doc, raw); !strings.Contains(refusal.Message, message) {
			t.Errorf("%s: %s", raw, refusal.Message)
		}
	}
	if _, err := applyDevelopmentOperations(&doc, nil, editTestCatalog); err == nil {
		t.Fatal("an empty group must be refused")
	}

	editApply(t, &doc, `[
		{"kind":"flow_add","name":"提交流程","description":"提交材料包","member":"整理员"},
		{"kind":"capability","member":"整理员","capability":"提交材料包","selected":true,"parameterSources":[{"name":"material_file_ids","source":"materials.ids"},{"name":"note","source":"materials.manifest_json"}]},
		{"kind":"skill","member":"整理员","name":"材料清点","selected":true,"body":"逐件清点材料"},
		{"kind":"member","member":"核对员","enabled":false,"outputSchema":"{\"type\":\"object\"}"},
		{"kind":"step","flow":"提交流程","step":"整理员","name":"清点并提交","requirement":"清点材料后提交"},
		{"kind":"flow","flow":"提交流程","description":"接材料包，交付提交回执"}
	]`)
	editKernelGraph(t, doc.Workflows[0])
	worker := doc.Members[1].Configuration
	if len(worker.BusinessCapabilityBindings) != 1 || len(worker.BusinessCapabilityBindings[0].Parameters) != 2 || len(worker.Skills) != 1 || doc.Members[2].Relationship.Enabled || string(doc.Members[2].Configuration.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("members = %+v", doc.Members)
	}
	graph := editGraphOf(t, doc.Workflows[0])
	step := editLabelled(t, graph, "清点并提交")
	if editString(editObject(step["config"])["result_requirement"]) != "清点材料后提交" || doc.Workflows[0].Description != "接材料包，交付提交回执" {
		t.Fatalf("step = %v", step)
	}
	// Without the result classification the completion check has nothing to read.
	if refusal := editRefusal(t, doc, `[{"kind":"business_completion","flow":"提交流程","capabilities":["提交材料包"],"allowNeedsInput":false}]`); !strings.Contains(refusal.Message, "须先启用") {
		t.Fatalf("refusal = %s", refusal.Message)
	}
	editApply(t, &doc, `[{"kind":"result_protocol","flow":"提交流程","enabled":true},{"kind":"result_protocol","flow":"提交流程","enabled":false}]`)
	if graph = editGraphOf(t, doc.Workflows[0]); graph["result_protocol"] != nil || !editPlainText(graph["output_contract"]) {
		t.Fatalf("classification left behind: %v", graph["output_contract"])
	}
	// Handing a step to the lead keeps what the step was asked to do.
	editApply(t, &doc, `[{"kind":"step","flow":"提交流程","step":"清点并提交","member":"负责人"}]`)
	step = editLabelled(t, editGraphOf(t, doc.Workflows[0]), "清点并提交")
	if editNodeType(step) != "lead" || editString(editObject(step["config"])["instruction"]) != "清点材料后提交" {
		t.Fatalf("reassigned step = %v", step)
	}
	editKernelGraph(t, doc.Workflows[0])
}
