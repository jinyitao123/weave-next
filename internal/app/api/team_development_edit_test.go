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
		{"kind":"step_input","flow":"复核流程","step":"汇总检查结果","source":"run_input","selected":false}
	]`)
	editKernelGraph(t, doc.Workflows[0])
	graph := editStructurallyValid(t, doc.Workflows[0])
	summary, join := editLabelled(t, graph, "汇总检查结果"), editLabelled(t, graph, "汇总分支")
	if editResultSource(editDeliveryNode(graph)) != editString(summary["id"]) || editNumber(editObject(join["config"])["success_count"]) != 2 {
		t.Fatalf("summary = %v join = %v", summary, join)
	}
	// After the join the summary reads the join's result, never a branch's own.
	inputs := editObject(summary["inputs"])
	if _, original := inputs["original"]; original || len(inputs) != 1 || !editReadsNode(summary, editString(join["id"])) {
		t.Fatalf("inputs = %v", inputs)
	}
	if kind := editString(editObject(editLabelled(t, graph, "整理员")["config"])["kind"]); kind != "dispatch" {
		t.Fatalf("first branch kind = %s", kind)
	}
	for raw, message := range map[string]string{
		`[{"kind":"step_remove","flow":"复核流程","step":"核对条款"}]`:                                                                     "至少保留两个分支",
		`[{"kind":"step_input","flow":"复核流程","step":"汇总检查结果","source":"node_output","from":"核对条款","selected":true}]`:               "汇合之后读不到",
		`[{"kind":"step_add","flow":"复核流程","after":"核对条款","member":"负责人","name":"再看一遍","requirement":"复查"}]`:                       "并行分支里的步骤只能由执行成员负责",
		`[{"kind":"join","flow":"复核流程","step":"汇总分支","policy":"quorum","successCount":3}]`:                                         "所需成功分支数无效",
		`[{"kind":"step_add","flow":"复核流程","after":"并行分工","member":"负责人","name":"并行汇总","requirement":"x","placement":"parallel"}]`: "并行分支须选择已启用的执行成员",
		`[{"kind":"delivery","flow":"复核流程","from":"交付结果"}]`:                                                                        "交付来源必须是当前流程的前序步骤",
		`[{"kind":"member_remove","member":"核对员"}]`:                                                                                "仍被流程“复核流程”的步骤“核对条款”使用",
	} {
		if refusal := editRefusal(t, doc, raw); !strings.Contains(refusal.Message, message) {
			t.Errorf("%s: %s", raw, refusal.Message)
		}
	}
}

func TestControlledEditsAddAndRemoveStepsInsideABranch(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, `[
		{"kind":"flow_add","name":"复核流程","description":"复核合同","member":"整理员"},
		{"kind":"step_add","flow":"复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"核对付款条件","placement":"parallel"},
		{"kind":"step_add","flow":"复核流程","after":"核对条款","member":"整理员","name":"复核条款","requirement":"复核上一步的结论"},
		{"kind":"step_add","flow":"复核流程","after":"并行分工","member":"负责人","name":"汇总","requirement":"汇总各分支结论"}
	]`)
	kernel := editKernelGraph(t, doc.Workflows[0])
	graph := editGraphOf(t, doc.Workflows[0])
	check, recheck := editLabelled(t, graph, "核对条款"), editLabelled(t, graph, "复核条款")
	steps, err := machine.ParallelBranchSteps(kernel, editString(check["id"]))
	if err != nil || len(steps) != 2 || steps[1].ID != editString(recheck["id"]) {
		t.Fatalf("branch steps = %+v, %v", steps, err)
	}
	if kind := editString(editObject(recheck["config"])["kind"]); kind != "dispatch" {
		t.Fatalf("a step inside a branch is %s", kind)
	}
	if previous := editObject(editObject(editObject(recheck["inputs"])["previous"])["value"]); editString(previous["node_id"]) != editString(check["id"]) {
		t.Fatalf("the second step does not read the first: %v", recheck["inputs"])
	}
	// Removing the branch's last step lets the one before join again.
	editApply(t, &doc, `[{"kind":"step_remove","flow":"复核流程","step":"复核条款"}]`)
	kernel = editKernelGraph(t, doc.Workflows[0])
	if steps, err := machine.ParallelBranchSteps(kernel, editString(check["id"])); err != nil || len(steps) != 1 {
		t.Fatalf("after removal branch steps = %+v, %v", steps, err)
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

func TestControlledEditsChooseAModelOnlyFromTheWorkspaceList(t *testing.T) {
	models := []string{"deepseek-flash", "deepseek-reasoner"}
	apply := func(doc *developmentDocument, raw string, available []string) error {
		_, err := applyDevelopmentOperationsWith(doc, editOps(t, raw), editTestCatalog, available, editTools{})
		return err
	}
	refused := func(raw string, available []string, change func(*developmentDocument)) string {
		t.Helper()
		doc := editTestDocument()
		if change != nil {
			change(&doc)
		}
		var refusal *editError
		if err := apply(&doc, raw, available); !errors.As(err, &refusal) {
			t.Fatalf("expected a refusal for %s, got %v", raw, err)
		}
		return refusal.Message
	}

	// The lead and a member each take a model from the list; choosing the same one again is not an error.
	doc := editTestDocument()
	if err := apply(&doc, `[{"kind":"member","member":"负责人","model":"deepseek-reasoner"},{"kind":"member","member":"整理员","model":"deepseek-flash"}]`, models); err != nil {
		t.Fatal(err)
	}
	if doc.Members[0].Configuration.Model != "deepseek-reasoner" || doc.Members[1].Configuration.Model != "deepseek-flash" {
		t.Fatalf("models = %q, %q", doc.Members[0].Configuration.Model, doc.Members[1].Configuration.Model)
	}
	// A new member takes the first member's model unless one is named.
	if err := apply(&doc, `[{"kind":"member_add","ref":"a","name":"协调员","duty":"协调"},{"kind":"member_add","ref":"b","name":"复核员","duty":"复核","model":"deepseek-reasoner"}]`, models); err != nil {
		t.Fatal(err)
	}
	if doc.Members[3].Configuration.Model != "deepseek-flash" || doc.Members[4].Configuration.Model != "deepseek-reasoner" {
		t.Fatalf("new members' models = %q, %q", doc.Members[3].Configuration.Model, doc.Members[4].Configuration.Model)
	}

	// A name off the list is refused and the list is named; nothing is guessed.
	if message := refused(`[{"kind":"member","member":"整理员","model":"gpt-unknown"}]`, models, nil); !strings.Contains(message, "gpt-unknown") || !strings.Contains(message, "deepseek-flash、deepseek-reasoner") {
		t.Fatalf("unknown model: %s", message)
	}
	// A workspace with no model enabled says so instead of accepting any name.
	if message := refused(`[{"kind":"member","member":"整理员","model":"deepseek-flash"}]`, nil, nil); !strings.Contains(message, "还没有可用的模型") {
		t.Fatalf("empty list: %s", message)
	}
	// A member on an external engine takes its model through the engine edit, not from this list.
	if message := refused(`[{"kind":"member","member":"整理员","model":"deepseek-flash"}]`, models, func(doc *developmentDocument) {
		doc.Members[1].Configuration.Engine = "codex"
	}); !strings.Contains(message, "member_engine") {
		t.Fatalf("external engine: %s", message)
	}
	if message := refused(`[{"kind":"member","member":"整理员","model":7}]`, models, nil); message == "" {
		t.Fatal("a model that is not text was accepted")
	}
}

func TestALeadWithAFlowToRunNeedsAModelLikeAnyMember(t *testing.T) {
	doc := editTestDocument()
	doc.Members[0].Configuration.Model = ""
	// With no flow the lead has nothing to run.
	for _, issue := range developmentIssues(doc, editTestCatalog, true) {
		if issue.Code == "member_model_missing" {
			t.Fatalf("lead without a flow: %+v", issue)
		}
	}
	editApply(t, &doc, `[{"kind":"flow_add","name":"线索跟进流程","description":"接线索，交付结论","member":"整理员"}]`)
	found := false
	for _, issue := range developmentIssues(doc, editTestCatalog, true) {
		found = found || issue.Code == "member_model_missing" && issue.Target == "负责人" && issue.Blocks == "trial"
	}
	if !found {
		t.Fatal("a lead that takes the first step of a flow was not asked for a model")
	}
}

func TestDevelopmentCapabilityNameUsesActionableTeamScope(t *testing.T) {
	catalog := []catalogCapability{
		{ID: "forge:action:opening.submit", Name: "提交审批", Status: "available", ExecutionMode: "employee_only"},
		{ID: "forge:action:contract.submit", Name: "提交审批", Status: "available", ExecutionMode: "team_delegable"},
		{ID: "forge:action:retired.submit", Name: "提交审批", Status: "unavailable", ExecutionMode: "team_delegable"},
	}
	session := editSession{catalog: catalog}
	selected, err := session.capability("提交审批")
	if err != nil || selected.ID != "forge:action:contract.submit" {
		t.Fatalf("unique actionable team capability blocked: %+v, %v", selected, err)
	}
	session.catalog = append(catalog, catalogCapability{ID: "forge:action:other.submit", Name: "提交审批", Status: "available", ExecutionMode: "team_delegable"})
	if _, err := session.capability("提交审批"); err == nil {
		t.Fatal("ambiguous team actions were silently selected")
	}
}

func TestDevelopmentCapabilityRemovalTargetsExistingBinding(t *testing.T) {
	doc := editTestDocument()
	member := &doc.Members[1]
	member.Configuration.BusinessCapabilityIDs = []string{"forge:action:retired.submit"}
	catalog := []catalogCapability{
		{ID: "forge:action:current.submit", Name: "提交审批", Status: "available", ExecutionMode: "team_delegable"},
		{ID: "forge:action:retired.submit", Name: "提交审批", Status: "unavailable", ExecutionMode: "team_delegable"},
	}
	op := editOps(t, `[{"kind":"capability","member":"整理员","capability":"提交审批","selected":false}]`)
	if _, err := applyDevelopmentOperations(&doc, op, catalog); err != nil {
		t.Fatal(err)
	}
	if len(doc.Members[1].Configuration.BusinessCapabilityIDs) != 0 {
		t.Fatal("unavailable existing binding was left behind")
	}
}

// A desktop caller opens a parallel check right after the lead's step, as the
// guide describes, then adds work after the region (issue #66).
func TestControlledEditsOpenAParallelRegionAfterTheLead(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, `[{"kind":"flow_add","name":"合同流程","description":"整理并提交合同","member":"整理员"}]`)
	refusal := editRefusal(t, doc, `[{"kind":"step_add","flow":"合同流程","after":"理解任务","member":"核对员","name":"交付检查","requirement":"检查交付条款","placement":"parallel"}]`)
	if !strings.Contains(refusal.Message, "只有一个分支") {
		t.Fatalf("a region of one branch: %s", refusal.Message)
	}
	editApply(t, &doc, `[
		{"kind":"member_add","ref":"business","name":"商务检查员","duty":"检查商务条款"},
		{"kind":"step_add","flow":"合同流程","after":"理解任务","member":"核对员","name":"交付检查","requirement":"检查交付条款","placement":"parallel"},
		{"kind":"step_add","flow":"合同流程","after":"理解任务","member":"business","name":"商务检查","requirement":"检查商务条款","placement":"parallel"},
		{"kind":"step_add","flow":"合同流程","after":"并行分工","member":"负责人","name":"汇总检查结果","requirement":"汇总两项检查"}
	]`)
	kernel := editKernelGraph(t, doc.Workflows[0])
	graph := editGraphOf(t, doc.Workflows[0])
	lead, parallel := editLabelled(t, graph, "理解任务"), editLabelled(t, graph, "并行分工")
	if next := editEdges(graph, "from_node_id", editString(lead["id"])); len(next) != 1 || editString(next[0]["to_node_id"]) != editString(parallel["id"]) {
		t.Fatalf("the lead does not lead into the region: %v", next)
	}
	for _, label := range []string{"交付检查", "商务检查"} {
		step := editLabelled(t, graph, label)
		steps, err := machine.ParallelBranchSteps(kernel, editString(step["id"]))
		if err != nil || len(steps) != 1 {
			t.Fatalf("%s branch = %+v, %v", label, steps, err)
		}
		if previous := editObject(editObject(editObject(step["inputs"])["previous"])["value"]); editString(previous["node_id"]) != editString(lead["id"]) {
			t.Fatalf("%s does not read the lead: %v", label, step["inputs"])
		}
	}
	join := editLabelled(t, graph, "汇总分支")
	summary := editLabelled(t, graph, "汇总检查结果")
	if next := editEdges(graph, "from_node_id", editString(join["id"])); len(next) != 1 || editString(next[0]["to_node_id"]) != editString(summary["id"]) {
		t.Fatalf("the summary is not after the join: %v", next)
	}

	// Right before delivery there is no step to gather the branches.
	editApply(t, &doc, `[{"kind":"step_add","flow":"合同流程","after":"整理员","member":"负责人","name":"最终汇总","requirement":"汇总全部结果"}]`)
	refusal = editRefusal(t, doc, `[{"kind":"step_add","flow":"合同流程","after":"最终汇总","member":"核对员","name":"再查一次","requirement":"复查","placement":"parallel"}]`)
	if !strings.Contains(refusal.Message, "之后就是交付") {
		t.Fatalf("parallel right before delivery: %s", refusal.Message)
	}
}

func TestDevelopmentCapabilityIDResolvesRepeatedNames(t *testing.T) {
	doc := editTestDocument()
	catalog := []catalogCapability{
		{ID: "forge:action:contract.submit", Name: "提交审批", ObjectName: "合同", Status: "available", ExecutionMode: "team_delegable"},
		{ID: "forge:action:expense.submit", Name: "提交审批", ObjectName: "报销单", Status: "available", ExecutionMode: "team_delegable"},
	}
	_, err := applyDevelopmentOperations(&doc, editOps(t, `[{"kind":"capability","member":"整理员","capability":"提交审批","selected":true}]`), catalog)
	var refusal *editError
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "合同（forge:action:contract.submit）") || !strings.Contains(refusal.Message, "报销单（forge:action:expense.submit）") {
		t.Fatalf("ambiguous name refusal does not name the candidates: %v", err)
	}
	if _, err := applyDevelopmentOperations(&doc, editOps(t, `[{"kind":"capability","member":"整理员","capability":"forge:action:contract.submit","selected":true}]`), catalog); err != nil {
		t.Fatal(err)
	}
	if ids := doc.Members[1].Configuration.BusinessCapabilityIDs; !reflect.DeepEqual(ids, []string{"forge:action:contract.submit"}) {
		t.Fatalf("bound = %v", ids)
	}
	// The bound one of two same-named actions is removed by its name.
	if _, err := applyDevelopmentOperations(&doc, editOps(t, `[{"kind":"capability","member":"整理员","capability":"提交审批","selected":false}]`), catalog); err != nil {
		t.Fatal(err)
	}
	if ids := doc.Members[1].Configuration.BusinessCapabilityIDs; len(ids) != 0 {
		t.Fatalf("left bound = %v", ids)
	}
}

// Opening a parallel region beside a step whose result a later step reads
// moves that reader to the join: after the join a branch's own result is not
// readable, and the machine refuses the graph otherwise (#113).
func TestParallelRegionMovesLaterReadersToTheJoin(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, `[
		{"kind":"flow_add","name":"复核流程","description":"复核合同","member":"整理员"},
		{"kind":"step_add","flow":"复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"核对付款条件"},
		{"kind":"step_add","flow":"复核流程","after":"核对条款","member":"核对员","name":"汇总","requirement":"汇总结论"},
		{"kind":"step_add","flow":"复核流程","after":"核对条款","member":"整理员","name":"复查","requirement":"复查条款","placement":"parallel"}
	]`)
	graph := editStructurallyValid(t, doc.Workflows[0])
	summary, join, check := editLabelled(t, graph, "汇总"), editLabelled(t, graph, "汇总分支"), editLabelled(t, graph, "核对条款")
	if !editReadsNode(summary, editString(join["id"])) || editReadsNode(summary, editString(check["id"])) {
		t.Fatalf("汇总 inputs = %v", summary["inputs"])
	}
}
