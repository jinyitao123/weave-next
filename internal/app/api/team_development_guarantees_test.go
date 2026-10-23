package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const editGuaranteeFlow = `[
	{"kind":"flow_add","name":"合同复核流程","description":"接合同材料，交付复核结论","member":"整理员"},
	{"kind":"step_add","flow":"合同复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"逐条核对条款并给出依据"}
]`

// editStructurallyValid proves the machine's structural phases accept a flow.
func editStructurallyValid(t *testing.T, flow developmentWorkflow) editGraph {
	t.Helper()
	editKernelGraph(t, flow)
	graph := editGraphOf(t, flow)
	issue, err := editStructureIssue(graph)
	if err != nil || issue != "" {
		t.Fatalf("structure: %q %v\n%s", issue, err, flow.Graph)
	}
	return graph
}

func editEdgeShape(graph editGraph) []string {
	var shape []string
	for _, edge := range editItems(graph, "edges") {
		shape = append(shape, editString(editNode(graph, editString(edge["from_node_id"]))["label"])+" -"+editString(edge["route"])+"-> "+editString(editNode(graph, editString(edge["to_node_id"]))["label"]))
	}
	return shape
}

func TestApprovalGatesTheStepsAfterItAndDeliversTheReasonOnRejection(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	before := editGraphOf(t, doc.Workflows[0])
	changes := editApply(t, &doc, `[{"kind":"approval","flow":"合同复核流程","after":"整理员","title":"法务审批","instructions":"核对材料是否齐全","audience":"法务总监"}]`)
	if !reflect.DeepEqual(changes, []string{"设置人工审批：合同复核流程 · 法务审批"}) {
		t.Fatalf("changes = %v", changes)
	}
	graph := editStructurallyValid(t, doc.Workflows[0])
	wait := editLabelled(t, graph, "法务审批")
	config := editObject(wait["config"])
	task := editObject(config["task"])
	if editNodeType(wait) != "wait" || config["kind"] != "human" || task["title"] != "法务审批" || task["audience_ref"] != "法务总监" {
		t.Fatalf("approval node = %v", wait)
	}
	want := []string{
		"整理员 -success-> 法务审批", "法务审批 -success-> 法务审批：是否批准",
		"法务审批：是否批准 -case-> 核对条款", "法务审批：是否批准 -default-> 法务审批：驳回意见",
	}
	shape := strings.Join(editEdgeShape(graph), "\n")
	for _, edge := range want {
		if !strings.Contains(shape, edge) {
			t.Fatalf("missing %q in\n%s", edge, shape)
		}
	}
	// The approved route is the only way on, and it needs an explicit approve.
	kernel := editKernelGraph(t, doc.Workflows[0])
	for _, edge := range kernel.Edges {
		if edge.Route == machine.RouteCase && (edge.Predicate == nil || string(edge.Predicate.Right.Value) != `"approve"` || edge.Predicate.Left.Path != "/decision") {
			t.Fatalf("approval case = %+v", edge.Predicate)
		}
	}
	if source := editResultSource(editDeliveryNode(graph)); editString(editNode(graph, source)["label"]) != "核对条款" {
		t.Fatalf("main delivery reads %s", source)
	}
	rejected := editObject(editObject(editLabelled(t, graph, "法务审批：驳回意见")["config"])["result"])
	if rejected["node_id"] != wait["id"] || rejected["path"] != "/comment" {
		t.Fatalf("rejection delivers %v", rejected)
	}

	editApply(t, &doc, `[{"kind":"approval_remove","flow":"合同复核流程","step":"法务审批"}]`)
	after := editStructurallyValid(t, doc.Workflows[0])
	if !reflect.DeepEqual(editEdgeShape(after), editEdgeShape(before)) || !reflect.DeepEqual(editLabels(after), editLabels(before)) {
		t.Fatalf("removal left %v, want %v", editEdgeShape(after), editEdgeShape(before))
	}
}

func TestStepsAroundAnApprovalKeepItsRoutes(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	editApply(t, &doc, `[
		{"kind":"approval","flow":"合同复核流程","after":"整理员","title":"法务审批","instructions":"核对材料是否齐全"},
		{"kind":"step_add","flow":"合同复核流程","after":"整理员","member":"核对员","name":"补充材料","requirement":"补齐缺少的材料"}
	]`)
	graph := editStructurallyValid(t, doc.Workflows[0])
	if shape := strings.Join(editEdgeShape(graph), "\n"); !strings.Contains(shape, "整理员 -success-> 补充材料") || !strings.Contains(shape, "补充材料 -success-> 法务审批") {
		t.Fatalf("a step added before the approval is approved too:\n%s", shape)
	}
	// Removing the step right after the approval keeps the approved route and
	// delivers the last result the approver saw.
	editApply(t, &doc, `[{"kind":"step_remove","flow":"合同复核流程","step":"核对条款"}]`)
	graph = editStructurallyValid(t, doc.Workflows[0])
	if shape := strings.Join(editEdgeShape(graph), "\n"); !strings.Contains(shape, "法务审批：是否批准 -case-> 交付结果") {
		t.Fatalf("approved route lost:\n%s", shape)
	}
	if source := editResultSource(editDeliveryNode(graph)); editString(editNode(graph, source)["label"]) != "补充材料" {
		t.Fatalf("delivery reads %s", source)
	}
}

func TestApprovalIsRefusedWhereItCannotBeEnforced(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	editApply(t, &doc, `[
		{"kind":"step_add","flow":"合同复核流程","after":"核对条款","member":"核对员","name":"汇总","requirement":"汇总结论"},
		{"kind":"step_add","flow":"合同复核流程","after":"核对条款","member":"整理员","name":"复查","requirement":"复查条款","placement":"parallel"}
	]`)
	cases := map[string]string{
		"parallel branch":  `[{"kind":"approval","flow":"合同复核流程","after":"复查","title":"审批","instructions":"确认"}]`,
		"taken name":       `[{"kind":"approval","flow":"合同复核流程","after":"汇总","title":"整理员","instructions":"确认"}]`,
		"missing guidance": `[{"kind":"approval","flow":"合同复核流程","after":"汇总","title":"审批"}]`,
		"not an approval":  `[{"kind":"approval_remove","flow":"合同复核流程","step":"汇总"}]`,
		"protocol after":   `[{"kind":"approval","flow":"合同复核流程","after":"汇总","title":"审批","instructions":"确认"},{"kind":"result_protocol","flow":"合同复核流程","enabled":true}]`,
	}
	for name, raw := range cases {
		if refusal := editRefusal(t, doc, raw); refusal.Message == "" {
			t.Fatalf("%s: empty refusal", name)
		}
	}
	if refusal := editRefusal(t, doc, cases["protocol after"]); !strings.Contains(refusal.Message, "只能有一个交付") {
		t.Fatalf("protocol refusal = %s", refusal.Message)
	}

	scheduled := editTestDocument()
	editApply(t, &scheduled, editGuaranteeFlow)
	scheduled.Workflows[0].Trigger = json.RawMessage(`{"schema_version":1,"type":"schedule","config":{"schedule_id":"daily"},"delivery":{"kind":"job_record"}}`)
	if refusal := editRefusal(t, scheduled, `[{"kind":"approval","flow":"合同复核流程","after":"核对条款","title":"审批","instructions":"确认"}]`); !strings.Contains(refusal.Message, "定时") {
		t.Fatalf("scheduled flow refusal = %s", refusal.Message)
	}

	// Inside the code team's verify loop a rejection would leave the loop
	// sideways; the machine's structural check refuses it.
	code := editTestDocument()
	code.Members = append(code.Members, editTestMember("00000000-0000-4000-8000-000000000004", "编码", "worker"), editTestMember("00000000-0000-4000-8000-000000000005", "验证", "worker"))
	flow, err := codeStarterWorkflow(code, "修复缺陷")
	if err != nil {
		t.Fatal(err)
	}
	flow.Name = "开发与验证"
	code.Workflows = append(code.Workflows, flow)
	if refusal := editRefusal(t, code, `[{"kind":"approval","flow":"开发与验证","after":"编码","title":"代码审批","instructions":"确认"}]`); !strings.Contains(refusal.Message, "结构检查") {
		t.Fatalf("approval inside the loop: %s", refusal.Message)
	}

	business := editTestDocument()
	editApply(t, &business, editBusinessTeam)
	if refusal := editRefusal(t, business, `[{"kind":"approval","flow":"线索跟进流程","after":"整理员","title":"审批","instructions":"确认"}]`); !strings.Contains(refusal.Message, "只能有一个交付") {
		t.Fatalf("business flow refusal = %s", refusal.Message)
	}
}

func TestFailureBranchHandsTheFailedStepsWorkToAFallback(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	before := editGraphOf(t, doc.Workflows[0])
	editApply(t, &doc, `[{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"整理员","name":"核对失败说明","requirement":"说明核对没有完成的原因"}]`)
	graph := editStructurallyValid(t, doc.Workflows[0])
	shape := strings.Join(editEdgeShape(graph), "\n")
	if !strings.Contains(shape, "核对条款 -failure-> 核对失败说明") || !strings.Contains(shape, "核对失败说明 -success-> 核对失败说明：交付") {
		t.Fatalf("failure branch:\n%s", shape)
	}
	failed, fallback := editLabelled(t, graph, "核对条款"), editLabelled(t, graph, "核对失败说明")
	if !reflect.DeepEqual(fallback["inputs"], failed["inputs"]) || editString(editObject(fallback["config"])["result_requirement"]) != "说明核对没有完成的原因" {
		t.Fatalf("fallback = %v", fallback)
	}

	// The failed step has no result on the failure branch.
	if refusal := editRefusal(t, doc, `[{"kind":"step_input","flow":"合同复核流程","step":"核对失败说明","source":"node_output","from":"核对条款","selected":true}]`); !strings.Contains(refusal.Message, "核对失败说明") {
		t.Fatalf("reading the failed step: %s", refusal.Message)
	}
	for _, raw := range []string{
		`[{"kind":"step_remove","flow":"合同复核流程","step":"核对条款"}]`,
		`[{"kind":"step_remove","flow":"合同复核流程","step":"核对失败说明"}]`,
		`[{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"整理员","name":"再说明","requirement":"说明"}]`,
	} {
		if refusal := editRefusal(t, doc, raw); !strings.Contains(refusal.Message, "失败分支") {
			t.Fatalf("%s: %s", raw, refusal.Message)
		}
	}

	// A step added after the failing one follows its success; the branch stays.
	editApply(t, &doc, `[{"kind":"step_add","flow":"合同复核流程","after":"核对条款","member":"核对员","name":"汇总","requirement":"汇总结论"}]`)
	graph = editStructurallyValid(t, doc.Workflows[0])
	if shape := strings.Join(editEdgeShape(graph), "\n"); !strings.Contains(shape, "核对条款 -success-> 汇总") || !strings.Contains(shape, "核对条款 -failure-> 核对失败说明") {
		t.Fatalf("after adding a step:\n%s", shape)
	}
	editApply(t, &doc, `[{"kind":"step_remove","flow":"合同复核流程","step":"汇总"},{"kind":"on_failure_remove","flow":"合同复核流程","step":"核对条款"}]`)
	after := editStructurallyValid(t, doc.Workflows[0])
	if !reflect.DeepEqual(editLabels(after), editLabels(before)) || len(editItems(after, "edges")) != len(editItems(before, "edges")) {
		t.Fatalf("removal left %v", editEdgeShape(after))
	}
}

// Approval stays accepted but is not offered to agents until the approver is
// enforced and a rejection ends the run without success.
func TestApprovalIsNotOfferedInTheGuide(t *testing.T) {
	for _, topic := range developmentOperationTopics {
		for _, operation := range topic.Operations {
			if strings.HasPrefix(operation.Kind, "approval") {
				t.Fatalf("guide documents %s", operation.Kind)
			}
		}
	}
	if strings.Contains(mcpEntryInstructions, "同意") {
		t.Fatal("MCP instructions still steer agents to approvals")
	}
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	editApply(t, &doc, `[{"kind":"approval","flow":"合同复核流程","after":"核对条款","title":"法务审批","instructions":"核对结论"}]`)
}

func TestDraftViewShowsApprovalsAndFailureBranches(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editGuaranteeFlow)
	editApply(t, &doc, `[
		{"kind":"approval","flow":"合同复核流程","after":"核对条款","title":"法务审批","instructions":"核对结论","audience":"法务总监"},
		{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"整理员","name":"核对失败说明","requirement":"说明原因"}
	]`)
	raw, _ := json.Marshal(mcpDraftView(developmentDraft{Document: doc}, nil))
	view := string(raw)
	for _, want := range []string{`"type":"人工审批"`, `"approver":"法务总监"`, `"type":"条件分支"`, `核对失败说明（失败时）`, `法务审批：驳回意见（其他情况）`} {
		if !strings.Contains(view, want) {
			t.Fatalf("draft view lacks %s:\n%s", want, view)
		}
	}
}
