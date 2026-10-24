package api

import (
	"reflect"
	"strings"
	"testing"
)

const editCheckedFlow = `[
	{"kind":"flow_add","name":"合同复核流程","description":"接合同材料，交付复核结论","member":"整理员"},
	{"kind":"step_add","flow":"合同复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"逐条核对条款并给出依据"},
	{"kind":"member_add","ref":"checker","name":"格式检查员","duty":"按标准检查结果"}
]`

func TestCheckLoopSendsAStepBackUntilItsCheckPasses(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editCheckedFlow)
	before := editGraphOf(t, doc.Workflows[0])
	changes := editApply(t, &doc, `[{"kind":"check_loop","flow":"合同复核流程","step":"核对条款","checker":"格式检查员","criteria":"每条结论引用条款原文","maxRounds":3}]`)
	if !reflect.DeepEqual(changes, []string{"设置检查回路：合同复核流程 · 核对条款"}) {
		t.Fatalf("changes = %v", changes)
	}
	graph := editStructurallyValid(t, doc.Workflows[0])
	shape := strings.Join(editEdgeShape(graph), "\n")
	for _, edge := range []string{
		"整理员 -success-> 核对条款：检查未通过时退回", "核对条款：检查未通过时退回 -body-> 核对条款",
		"核对条款 -success-> 核对条款：检查", "核对条款：检查 -success-> 核对条款：检查结论",
		"核对条款：检查结论 -back-> 核对条款：检查未通过时退回", "核对条款：检查未通过时退回 -exit-> 交付结果",
	} {
		if !strings.Contains(shape, edge) {
			t.Fatalf("missing %q in\n%s", edge, shape)
		}
	}
	loop := editLabelled(t, graph, "核对条款：检查未通过时退回")
	if editNumber(editObject(loop["config"])["max_iterations"]) != 3 {
		t.Fatalf("loop = %v", loop)
	}
	// The delivery reads the checked step's own last result, not a copy.
	if result := editObject(editDeliveryNode(graph)["config"])["result"]; editObject(result)["node_id"] != loop["id"] || editObject(result)["path"] != "/latch_result/result" {
		t.Fatalf("delivery reads %v", result)
	}
	review := editObject(editObject(editLabelled(t, graph, "核对条款")["inputs"])["review"])
	if value := editObject(review["value"]); value["iteration"] != "previous_iteration" || value["path"] != "/feedback" {
		t.Fatalf("review input = %v", review)
	}
	if requirement := editString(editObject(editLabelled(t, graph, "核对条款：检查")["config"])["result_requirement"]); !strings.Contains(requirement, "每条结论引用条款原文") {
		t.Fatalf("checker requirement = %s", requirement)
	}

	editApply(t, &doc, `[{"kind":"check_loop_remove","flow":"合同复核流程","step":"核对条款"}]`)
	after := editStructurallyValid(t, doc.Workflows[0])
	if !reflect.DeepEqual(editLabels(after), editLabels(before)) || !reflect.DeepEqual(editEdgeShape(after), editEdgeShape(before)) ||
		!reflect.DeepEqual(editLabelled(t, after, "核对条款")["inputs"], editLabelled(t, before, "核对条款")["inputs"]) ||
		!reflect.DeepEqual(editDeliveryNode(after)["config"], editDeliveryNode(before)["config"]) {
		t.Fatalf("removal left %v", editEdgeShape(after))
	}
}

// The first step of a flow can be checked too: the loop becomes the entry, and
// the steps that read it read its last round.
func TestCheckLoopOnTheFirstStepBecomesTheEntry(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editCheckedFlow)
	editApply(t, &doc, `[{"kind":"check_loop","flow":"合同复核流程","step":"理解任务","checker":"格式检查员","criteria":"列出全部需要核对的条款","maxRounds":2}]`)
	graph := editStructurallyValid(t, doc.Workflows[0])
	loop := editLabelled(t, graph, "理解任务：检查未通过时退回")
	if graph["entry_node_id"] != loop["id"] {
		t.Fatalf("entry = %v", graph["entry_node_id"])
	}
	for _, binding := range editObject(editLabelled(t, graph, "整理员")["inputs"]) {
		if value := editObject(editObject(binding)["value"]); editString(value["source"]) == "node_output" && (value["node_id"] != loop["id"] || value["path"] != "/latch_result/result") {
			t.Fatalf("整理员 reads %v", value)
		}
	}
	editApply(t, &doc, `[{"kind":"check_loop_remove","flow":"合同复核流程","step":"理解任务"}]`)
	if graph = editStructurallyValid(t, doc.Workflows[0]); graph["entry_node_id"] != editLabelled(t, graph, "理解任务")["id"] {
		t.Fatalf("entry after removal = %v", graph["entry_node_id"])
	}
}

func TestCheckLoopIsRefusedWhereItCannotWrapTheStep(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editCheckedFlow)
	editApply(t, &doc, `[
		{"kind":"step_add","flow":"合同复核流程","after":"核对条款","member":"核对员","name":"汇总","requirement":"汇总结论"},
		{"kind":"step_add","flow":"合同复核流程","after":"核对条款","member":"整理员","name":"复查","requirement":"复查条款","placement":"parallel"},
		{"kind":"step_input","flow":"合同复核流程","step":"汇总","source":"node_output","from":"核对条款","selected":false},
		{"kind":"step_input","flow":"合同复核流程","step":"汇总","source":"node_output","from":"汇总分支","selected":true},
		{"kind":"check_loop","flow":"合同复核流程","step":"汇总","checker":"格式检查员","criteria":"结论完整","maxRounds":2}
	]`)
	for name, raw := range map[string]string{
		"parallel branch": `[{"kind":"check_loop","flow":"合同复核流程","step":"复查","checker":"格式检查员","criteria":"完整","maxRounds":2}]`,
		"already checked": `[{"kind":"check_loop","flow":"合同复核流程","step":"汇总","checker":"格式检查员","criteria":"完整","maxRounds":2}]`,
		"rounds":          `[{"kind":"check_loop","flow":"合同复核流程","step":"整理员","checker":"格式检查员","criteria":"完整","maxRounds":6}]`,
		"lead checker":    `[{"kind":"check_loop","flow":"合同复核流程","step":"整理员","checker":"负责人","criteria":"完整","maxRounds":2}]`,
		"no criteria":     `[{"kind":"check_loop","flow":"合同复核流程","step":"整理员","checker":"格式检查员","maxRounds":2}]`,
		"not checked":     `[{"kind":"check_loop_remove","flow":"合同复核流程","step":"整理员"}]`,
		"remove checked":  `[{"kind":"step_remove","flow":"合同复核流程","step":"汇总"}]`,
	} {
		if refusal := editRefusal(t, doc, raw); refusal.Message == "" {
			t.Fatalf("%s: empty refusal", name)
		}
	}
	if refusal := editRefusal(t, doc, `[{"kind":"step_remove","flow":"合同复核流程","step":"汇总"}]`); !strings.Contains(refusal.Message, "check_loop_remove") {
		t.Fatalf("removing a checked step: %s", refusal.Message)
	}

	failing := editTestDocument()
	editApply(t, &failing, editCheckedFlow)
	editApply(t, &failing, `[{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"整理员","name":"失败说明","requirement":"说明原因"}]`)
	if refusal := editRefusal(t, failing, `[{"kind":"check_loop","flow":"合同复核流程","step":"核对条款","checker":"格式检查员","criteria":"完整","maxRounds":2}]`); !strings.Contains(refusal.Message, "失败分支") {
		t.Fatalf("checking a step with a failure branch: %s", refusal.Message)
	}
}
