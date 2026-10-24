package api

import (
	"fmt"
	"strings"
)

// Flow guarantees as assertions. The caller writes what must hold, by step;
// every read of the draft proves each one on the graph, and publishing is
// refused while one does not hold. The flow's summary also lists what the
// graph itself enforces, so an auditor reads one list.

const (
	guaranteePassesThrough  = "passes_through"
	guaranteeChecked        = "checked"
	guaranteeFailureHandled = "failure_handled"
	editMaxGuarantees       = 16
)

// developmentGuarantee is one declared assertion; Step is a node id, so a
// renamed step keeps its assertion.
type developmentGuarantee struct {
	Type string `json:"type"`
	Step string `json:"step"`
}

// developmentGuaranteeView is one line of a flow's guarantee summary. Proven
// is set for declared assertions; derived facts hold by construction.
type developmentGuaranteeView struct {
	Declared bool   `json:"declared"`
	Proven   bool   `json:"proven"`
	Text     string `json:"text"`
}

type developmentFlowGuarantees struct {
	WorkflowID string                     `json:"workflow_id"`
	Workflow   string                     `json:"workflow"`
	Items      []developmentGuaranteeView `json:"items"`
}

func editStepName(graph editGraph, id string) string {
	return editFirst(editString(editNode(graph, id)["label"]), "该步骤")
}

// editRunSuccessors lists where a run can go from a step. A loop runs its body
// at least once and leaves through its exit only after the latch, so the exit
// is reached from the latch, never straight from the loop.
func editRunSuccessors(graph editGraph, id string) []string {
	var next []string
	node := editNode(graph, id)
	for _, edge := range editEdges(graph, "from_node_id", id) {
		route := editString(edge["route"])
		if editNodeType(node) == "loop" && route == "exit" || route == "back" {
			continue
		}
		next = append(next, editString(edge["to_node_id"]))
	}
	for _, loop := range editItems(graph, "nodes") {
		if editNodeType(loop) == "loop" && editString(editObject(loop["config"])["latch_node_id"]) == id {
			for _, edge := range editEdges(graph, "from_node_id", editString(loop["id"])) {
				if editString(edge["route"]) == "exit" {
					next = append(next, editString(edge["to_node_id"]))
				}
			}
		}
	}
	return next
}

// editPathAvoiding finds a route from the entry to the main delivery that never
// visits avoid, as the labels along it; nil when every route visits it.
func editPathAvoiding(graph editGraph, avoid string) []string {
	delivery := editDeliveryNode(graph)
	entry := editString(graph["entry_node_id"])
	if delivery == nil || entry == avoid {
		return nil
	}
	target := editString(delivery["id"])
	parent := map[string]string{entry: ""}
	for queue := []string{entry}; len(queue) > 0; queue = queue[1:] {
		current := queue[0]
		if current == target {
			var path []string
			for at := current; at != ""; at = parent[at] {
				path = append([]string{editStepName(graph, at)}, path...)
			}
			return path
		}
		for _, next := range editRunSuccessors(graph, current) {
			if _, seen := parent[next]; !seen && next != avoid {
				parent[next] = current
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func editMemberName(doc *developmentDocument, id string) string {
	for _, member := range doc.Members {
		if member.ID == id {
			return member.Configuration.DisplayName
		}
	}
	return "已移出的成员"
}

// editProveGuarantee says whether an assertion holds and, in words, why.
func editProveGuarantee(doc *developmentDocument, graph editGraph, guarantee developmentGuarantee) (bool, string) {
	step := editNode(graph, guarantee.Step)
	if step == nil {
		return false, "断言引用的步骤已不在流程中，请重新声明"
	}
	name := editStepName(graph, guarantee.Step)
	switch guarantee.Type {
	case guaranteePassesThrough:
		if path := editPathAvoiding(graph, guarantee.Step); path != nil {
			return false, fmt.Sprintf("要求每条通往交付的路径都经过“%s”，但存在不经过它的路径：%s", name, strings.Join(path, " → "))
		}
		return true, fmt.Sprintf("每条通往交付的路径都经过“%s”", name)
	case guaranteeChecked:
		if loop, check, _, err := editCheckLoopParts(graph, step); err == nil {
			return true, fmt.Sprintf("“%s”由“%s”检查，不通过就退回重做，最多 %v 轮", name,
				editMemberName(doc, editString(editObject(check["config"])["agent_id"])), editObject(loop["config"])["max_iterations"])
		}
		return false, fmt.Sprintf("要求“%s”有检查回路，但它没有；请用 check_loop 设置", name)
	case guaranteeFailureHandled:
		if failure := editFailureEdge(graph, guarantee.Step); failure != nil {
			return true, fmt.Sprintf("“%s”失败时交给“%s”处理", name, editStepName(graph, editString(failure["to_node_id"])))
		}
		return false, fmt.Sprintf("要求“%s”失败时有处理分支，但它没有；请用 on_failure 设置", name)
	}
	return false, fmt.Sprintf("不认识的断言类型“%s”", guarantee.Type)
}

// editDerivedGuarantees lists what a flow's structure enforces by itself.
func editDerivedGuarantees(doc *developmentDocument, graph editGraph) []string {
	var facts []string
	for _, node := range editItems(graph, "nodes") {
		id, config := editString(node["id"]), editObject(node["config"])
		switch editNodeType(node) {
		case "loop":
			for _, edge := range editEdges(graph, "from_node_id", id) {
				if editString(edge["route"]) != "body" {
					continue
				}
				body := editNode(graph, editString(edge["to_node_id"]))
				if _, check, _, err := editCheckLoopParts(graph, body); err == nil {
					facts = append(facts, fmt.Sprintf("“%s”由“%s”检查，不通过就退回重做，最多 %v 轮", editStepName(graph, editString(body["id"])),
						editMemberName(doc, editString(editObject(check["config"])["agent_id"])), config["max_iterations"]))
				} else {
					facts = append(facts, fmt.Sprintf("“%s”是一个回路，最多 %v 轮", editStepName(graph, id), config["max_iterations"]))
				}
			}
		case "wait":
			if editString(config["kind"]) == "human" {
				facts = append(facts, fmt.Sprintf("“%s”需要人工处理后才能继续", editStepName(graph, id)))
			}
		}
		if failure := editFailureEdge(graph, id); failure != nil {
			facts = append(facts, fmt.Sprintf("“%s”失败时交给“%s”处理，处理结果单独交付", editStepName(graph, id), editStepName(graph, editString(failure["to_node_id"]))))
		}
	}
	return facts
}

// developmentGuaranteeSummaries proves every flow's assertions; an unreadable
// flow has no summary, and its own issue already says so.
func developmentGuaranteeSummaries(doc *developmentDocument) []developmentFlowGuarantees {
	summaries := []developmentFlowGuarantees{}
	for _, flow := range doc.Workflows {
		graph, err := decodeEditGraph(flow.Graph)
		if err != nil {
			continue
		}
		summary := developmentFlowGuarantees{WorkflowID: flow.ID, Workflow: flow.Name, Items: []developmentGuaranteeView{}}
		for _, guarantee := range flow.Guarantees {
			proven, text := editProveGuarantee(doc, graph, guarantee)
			summary.Items = append(summary.Items, developmentGuaranteeView{Declared: true, Proven: proven, Text: text})
		}
		for _, fact := range editDerivedGuarantees(doc, graph) {
			summary.Items = append(summary.Items, developmentGuaranteeView{Proven: true, Text: fact})
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

// developmentUnprovenGuarantees names each declared assertion that does not
// hold, as the issue and the publish refusal word it.
func developmentUnprovenGuarantees(doc *developmentDocument) []developmentIssue {
	issues := []developmentIssue{}
	for _, summary := range developmentGuaranteeSummaries(doc) {
		for _, item := range summary.Items {
			if item.Declared && !item.Proven {
				issues = append(issues, developmentIssue{Scope: "workflow", Target: summary.Workflow, Workflow: summary.Workflow,
					Code: "guarantee_unproven", Blocks: "publish", Message: "流程保证不成立：" + item.Text})
			}
		}
	}
	return issues
}

func (e *editSession) changeGuarantees(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	raw, isList := operation["require"].([]any)
	if !isList || len(raw) > editMaxGuarantees {
		return editFail("require 须是至多 %d 项断言的列表；给空列表表示清除", editMaxGuarantees)
	}
	guarantees := []developmentGuarantee{}
	seen := map[developmentGuarantee]bool{}
	for _, item := range raw {
		object := editObject(item)
		kind := editString(object["type"])
		if !editOneOf(kind, guaranteePassesThrough, guaranteeChecked, guaranteeFailureHandled) {
			return editFail("断言类型须为 passes_through、checked 或 failure_handled")
		}
		step, err := e.step(graph, object, "step", "断言的步骤")
		if err != nil {
			return err
		}
		if editNodeType(step) == "deliver" {
			return editFail("断言须指向交付之前的步骤")
		}
		guarantee := developmentGuarantee{Type: kind, Step: editString(step["id"])}
		if !seen[guarantee] {
			seen[guarantee] = true
			guarantees = append(guarantees, guarantee)
		}
	}
	target.Guarantees = guarantees
	e.note("设置流程保证断言：%s · %d 项", target.Name, len(guarantees))
	return nil
}
