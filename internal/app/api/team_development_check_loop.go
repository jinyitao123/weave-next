package api

import (
	"github.com/google/uuid"
)

// A check loop sends a step's result to a checking member and back to the
// step while the check fails, at most maxRounds times. The loop's latch packs
// the verdict with the step's own result, so later steps read that result
// instead of a copy the checker would have to make.

const editMaxCheckRounds = 5

func editVerdictSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"passed", "feedback"},
		"properties": map[string]any{"passed": map[string]any{"type": "boolean"}, "feedback": map[string]any{"type": "string"}},
	}
}

func editCurrentRef(node, path string) map[string]any {
	return map[string]any{"source": "node_output", "node_id": node, "path": path, "iteration": "current_iteration"}
}

// editResultProperty is the schema of a step's result inside the packed latch.
func editResultProperty(step map[string]any) map[string]any {
	output := editObject(step["output"])
	if output["type"] == "json" && editObject(output["schema"]) != nil {
		return editObject(editCopy(output["schema"]))
	}
	return map[string]any{"type": "string"}
}

func editExpectedType(step map[string]any) string {
	if editObject(step["output"])["type"] == "json" {
		return "json"
	}
	return "text"
}

// editInLoop says whether a step is the body of a loop or reached from one
// before its back edge, which a second loop may not overlap.
func editInLoop(graph editGraph, id string) bool {
	for _, node := range editItems(graph, "nodes") {
		if editNodeType(node) != "loop" {
			continue
		}
		for _, edge := range editEdges(graph, "from_node_id", editString(node["id"])) {
			if editString(edge["route"]) != "body" {
				continue
			}
			latch := editString(editObject(node["config"])["latch_node_id"])
			seen := map[string]bool{}
			for queue := []string{editString(edge["to_node_id"])}; len(queue) > 0; queue = queue[1:] {
				current := queue[0]
				if seen[current] {
					continue
				}
				seen[current] = true
				if current == id {
					return true
				}
				if current == latch {
					continue
				}
				for _, next := range editMainOutgoing(graph, current) {
					queue = append(queue, editString(next["to_node_id"]))
				}
			}
		}
	}
	return false
}

func (e *editSession) addCheckLoop(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	step, err := e.step(graph, operation, "step", "要检查的步骤")
	if err != nil {
		return err
	}
	checker, err := e.member(map[string]any{"member": operation["checker"]})
	if err != nil {
		return err
	}
	criteria, err := editText(operation, "criteria", "检查标准", 2000)
	if err != nil {
		return err
	}
	rounds := editNumber(operation["maxRounds"])
	if rounds != float64(int(rounds)) || rounds < 1 || rounds > editMaxCheckRounds {
		return editFail("最多轮数须是 1 至 %d 的整数", editMaxCheckRounds)
	}
	id := editString(step["id"])
	label := editString(step["label"])
	if !editOneOf(editNodeType(step), "lead", "worker") || editParallelBranchWorker(graph, id) || editInLoop(graph, id) {
		return editFail("“%s”不能加检查回路：只能加在不在并行分支、也不在已有回路里的负责人或成员执行步骤上", label)
	}
	if editFailureEdge(graph, id) != nil {
		return editFail("“%s”设置了失败分支，请先用 on_failure_remove 移除", label)
	}
	if checker.Configuration.Role != "worker" || !checker.Relationship.Enabled {
		return editFail("检查须由已启用的执行成员负责")
	}
	outgoing := editMainOutgoing(graph, id)
	if len(outgoing) != 1 || editString(outgoing[0]["route"]) != "success" {
		return editFail("“%s”之后不是一条普通的连接，不能在这里加检查回路", label)
	}
	if _, declared := graph["result_protocol"]; declared && editResultSource(editDeliveryNode(graph)) == id {
		return editFail("“%s”是“可要求补充材料”结果分类的交付来源，不能加检查回路；请在它之前的步骤上设置", label)
	}
	checkLabel, packLabel, loopLabel := label+"：检查", label+"：检查结论", label+"：检查未通过时退回"
	for _, name := range []string{checkLabel, packLabel, loopLabel} {
		if editLabelTaken(graph, name) {
			return editFail("步骤名称“%s”已被占用", name)
		}
	}

	loopID, checkID, packID := "check-loop-"+uuid.NewString(), "check-"+uuid.NewString(), "check-result-"+uuid.NewString()
	check := editMemberStep(checker, nil, "consult")
	check["id"], check["label"] = checkID, checkLabel
	check["inputs"] = map[string]any{"original": editOriginalBinding(),
		"previous": map[string]any{"value": editCurrentRef(id, ""), "expected_type": editExpectedType(step)}}
	check["output"] = map[string]any{"type": "json", "schema": editVerdictSchema()}
	editConfig(check)["result_requirement"] = "按以下标准检查上一步的结果：" + criteria +
		"。全部符合时 passed 为 true、feedback 为空字符串；否则 passed 为 false，feedback 逐条写出需要修改的地方。"
	pack := map[string]any{"id": packID, "type": "transform", "label": packLabel,
		"output": map[string]any{"type": "json", "schema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"passed", "feedback", "result"},
			"properties": map[string]any{"passed": map[string]any{"type": "boolean"}, "feedback": map[string]any{"type": "string"}, "result": editResultProperty(step)},
		}},
		"config": map[string]any{"operation": "object", "fields": map[string]any{
			"passed": editCurrentRef(checkID, "/passed"), "feedback": editCurrentRef(checkID, "/feedback"), "result": editCurrentRef(id, ""),
		}}}
	loop := map[string]any{"id": loopID, "type": "loop", "label": loopLabel, "config": map[string]any{
		"max_iterations": int(rounds), "latch_node_id": packID,
		"continue_predicate": map[string]any{"left": editCurrentRef(packID, "/passed"), "operator": "eq", "right": map[string]any{"source": "literal", "value": false}},
	}}

	// The step reads the last round's feedback; the first round has none.
	inputs := editObject(step["inputs"])
	if inputs == nil {
		inputs = map[string]any{}
	}
	inputs["review"] = map[string]any{"expected_type": "text", "value": map[string]any{"source": "node_output", "node_id": packID, "path": "/feedback",
		"iteration": "previous_iteration", "default": map[string]any{"source": "literal", "value": ""}}}
	step["inputs"] = inputs

	// Whatever read the step's result now reads the loop's last round.
	result := map[string]any{"source": "node_output", "node_id": loopID, "path": "/latch_result/result"}
	for _, node := range editItems(graph, "nodes") {
		for _, binding := range editObject(node["inputs"]) {
			if value := editObject(editObject(binding)["value"]); editString(value["node_id"]) == id {
				editObject(binding)["value"] = editObject(editCopy(result))
			}
		}
		if editNodeType(node) == "deliver" && editResultSource(node) == id {
			editConfig(node)["result"] = editObject(editCopy(result))
		}
	}
	next := editString(outgoing[0]["to_node_id"])
	edges := []map[string]any{}
	for _, edge := range editItems(graph, "edges") {
		switch {
		case editString(edge["id"]) == editString(outgoing[0]["id"]):
			continue
		case editString(edge["to_node_id"]) == id:
			edge["to_node_id"] = loopID
		}
		edges = append(edges, edge)
	}
	edges = append(edges, editNewEdge(loopID, id, "body"), editNewEdge(id, checkID, "success"), editNewEdge(checkID, packID, "success"),
		editNewEdge(packID, loopID, "back"), editNewEdge(loopID, next, "exit"))
	if editString(graph["entry_node_id"]) == id {
		graph["entry_node_id"] = loopID
	}
	editSetItems(graph, "edges", edges)
	editSetItems(graph, "nodes", append(editItems(graph, "nodes"), loop, check, pack))
	e.note("设置检查回路：%s · %s", target.Name, label)
	return nil
}

// editCheckLoopParts finds the loop, checker and packed latch around a step.
func editCheckLoopParts(graph editGraph, step map[string]any) (loop, check, pack map[string]any, err error) {
	id := editString(step["id"])
	for _, edge := range editEdges(graph, "to_node_id", id) {
		if from := editNode(graph, editString(edge["from_node_id"])); editString(edge["route"]) == "body" && editNodeType(from) == "loop" {
			loop = from
		}
	}
	if loop == nil {
		return nil, nil, nil, editFail("“%s”没有设置检查回路", editString(step["label"]))
	}
	pack = editNode(graph, editString(editObject(loop["config"])["latch_node_id"]))
	out := editMainOutgoing(graph, id)
	if len(out) == 1 {
		check = editNode(graph, editString(out[0]["to_node_id"]))
	}
	shapeChanged := editFail("“%s”的检查回路结构已被改动，不能按检查回路移除；请在管理端处理", editString(step["label"]))
	if pack == nil || editNodeType(pack) != "transform" || check == nil || editNodeType(check) != "worker" {
		return nil, nil, nil, shapeChanged
	}
	if toPack := editMainOutgoing(graph, editString(check["id"])); len(toPack) != 1 || editString(toPack[0]["to_node_id"]) != editString(pack["id"]) {
		return nil, nil, nil, shapeChanged
	}
	return loop, check, pack, nil
}

func (e *editSession) removeCheckLoop(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	step, err := e.step(graph, operation, "step", "设置了检查回路的步骤")
	if err != nil {
		return err
	}
	loop, check, pack, err := editCheckLoopParts(graph, step)
	if err != nil {
		return err
	}
	id, loopID := editString(step["id"]), editString(loop["id"])
	removed := map[string]bool{loopID: true, editString(check["id"]): true, editString(pack["id"]): true}
	var next string
	for _, edge := range editEdges(graph, "from_node_id", loopID) {
		if editString(edge["route"]) == "exit" {
			next = editString(edge["to_node_id"])
		}
	}
	delete(editObject(step["inputs"]), "review")
	for _, node := range editItems(graph, "nodes") {
		for _, binding := range editObject(node["inputs"]) {
			if value := editObject(editObject(binding)["value"]); editString(value["node_id"]) == loopID {
				editObject(binding)["value"] = editNodeOutputRef(id)
			}
		}
		if editNodeType(node) == "deliver" && editResultSource(node) == loopID {
			editConfig(node)["result"] = editNodeOutputRef(id)
		}
	}
	edges := []map[string]any{}
	for _, edge := range editItems(graph, "edges") {
		from, to := editString(edge["from_node_id"]), editString(edge["to_node_id"])
		if removed[from] || from == id && removed[to] {
			continue
		}
		if to == loopID {
			edge["to_node_id"] = id
		}
		edges = append(edges, edge)
	}
	edges = append(edges, editNewEdge(id, next, "success"))
	if editString(graph["entry_node_id"]) == loopID {
		graph["entry_node_id"] = id
	}
	var nodes []map[string]any
	for _, node := range editItems(graph, "nodes") {
		if !removed[editString(node["id"])] {
			nodes = append(nodes, node)
		}
	}
	editSetItems(graph, "edges", edges)
	editSetItems(graph, "nodes", nodes)
	e.note("移除检查回路：%s · %s", target.Name, editString(step["label"]))
	return nil
}
