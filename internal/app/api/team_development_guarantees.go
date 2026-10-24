package api

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// Flow guarantees are the steps a model cannot skip: a person's approval
// after a step, and the branch a step's failure goes to. Each is compiled
// from the caller's intent into machine nodes, and a flow that carries one is
// checked against the machine's structural phases before the group is kept.

const editApprovalApprove = "approve"

func editApprovalSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []any{"decision", "comment"},
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []any{editApprovalApprove, "reject"}},
			"comment":  map[string]any{"type": "string"},
		},
	}
}

// editMainOutgoing lists a step's edges other than its failure branch.
func editMainOutgoing(graph editGraph, id string) []map[string]any {
	var found []map[string]any
	for _, edge := range editEdges(graph, "from_node_id", id) {
		if editString(edge["route"]) != "failure" {
			found = append(found, edge)
		}
	}
	return found
}

func editFailureEdge(graph editGraph, id string) map[string]any {
	for _, edge := range editEdges(graph, "from_node_id", id) {
		if editString(edge["route"]) == "failure" {
			return edge
		}
	}
	return nil
}

func editDeliveries(graph editGraph) int {
	count := 0
	for _, node := range editItems(graph, "nodes") {
		if editNodeType(node) == "deliver" {
			count++
		}
	}
	return count
}

func editLabelTaken(graph editGraph, label string) bool {
	for _, node := range editItems(graph, "nodes") {
		if editString(node["label"]) == label {
			return true
		}
	}
	return false
}

// editHasGuarantees says whether a flow carries structure beyond members,
// parallel regions and one delivery, which the edits alone do not prove.
func editHasGuarantees(graph editGraph) bool {
	for _, node := range editItems(graph, "nodes") {
		if editOneOf(editNodeType(node), "wait", "condition", "loop", "transform") {
			return true
		}
	}
	for _, edge := range editItems(graph, "edges") {
		if editString(edge["route"]) == "failure" {
			return true
		}
	}
	return editDeliveries(graph) > 1
}

const editProtocolSingleDelivery = "启用“可要求补充材料”结果分类的流程只能有一个交付，不能同时设置审批或失败分支；请先关闭结果分类或移除审批、失败分支"

// editSingleDeliveryIssue names why a flow needing one delivery has more.
func editSingleDeliveryIssue(graph editGraph) string {
	if editDeliveries(graph) <= 1 {
		return ""
	}
	if _, declared := graph["result_protocol"]; declared {
		return editProtocolSingleDelivery
	}
	if completion, err := editCompletionRequirement(graph); err == nil && completion != nil {
		return "设置了必须办成的业务动作的流程只能有一个交付，不能同时设置审批或失败分支"
	}
	return ""
}

// editStructureIssue runs the machine's structural phases over a flow; the
// later phases need published proofs that a draft does not have yet. Phase 8
// also refuses a failure branch that reads the step that failed (#104).
func editStructureIssue(graph editGraph) (string, error) {
	raw, err := json.Marshal(graph)
	if err != nil {
		return "", err
	}
	decoded, report := machine.DecodeGraphDefinitionV1(raw)
	if report == nil || len(report.Issues) == 0 {
		validated := machine.Validate(machine.ValidationContext{
			Trigger: machine.TriggerConfig{SchemaVersion: machine.SchemaVersionV1, Type: machine.TriggerConversationExplicit, Config: machine.ConversationExplicitConfig{}},
			Graph:   decoded,
		})
		report = &validated
	}
	for _, issue := range report.Issues {
		if issue.Phase > machine.PhaseValuesAndSchemas {
			continue
		}
		if label := editString(editNode(graph, issue.NodeID)["label"]); label != "" {
			return fmt.Sprintf("步骤“%s”未通过流程结构检查（%s）：%s", label, issue.Code, issue.Message), nil
		}
		return fmt.Sprintf("流程未通过结构检查（%s）：%s", issue.Code, issue.Message), nil
	}
	return "", nil
}

// editGuaranteeAnchor checks the step a guarantee is attached to.
func editGuaranteeAnchor(graph editGraph, node map[string]any, types ...string) error {
	// A business completion check needs the result classification, so this
	// also refuses flows that must finish their business actions.
	if _, declared := graph["result_protocol"]; declared {
		return editFail("%s", editProtocolSingleDelivery)
	}
	if !editOneOf(editNodeType(node), types...) || editParallelBranchWorker(graph, editString(node["id"])) {
		return editFail("“%s”不能设置这项保证：只能接在负责人、成员执行（不在并行分支里）的步骤上", editFirst(editString(node["label"]), "该步骤"))
	}
	return nil
}

func (e *editSession) addApproval(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	after, err := e.step(graph, operation, "after", "审批前的步骤")
	if err != nil {
		return err
	}
	title, err := editText(operation, "title", "审批名称", 80)
	if err != nil {
		return err
	}
	instructions, err := editText(operation, "instructions", "审批说明", 2000)
	if err != nil {
		return err
	}
	audience, err := editOptionalText(operation, "audience", "审批人", 256)
	if err != nil {
		return err
	}
	if err = editGuaranteeAnchor(graph, after, "lead", "worker", "join"); err != nil {
		return err
	}
	// The machine refuses a person's wait where no conversation is waiting.
	var trigger struct {
		Type machine.TriggerType `json:"type"`
	}
	if json.Unmarshal(target.Trigger, &trigger) == nil &&
		editOneOf(string(trigger.Type), string(machine.TriggerSchedule), string(machine.TriggerAPI), string(machine.TriggerEvent)) {
		return editFail("流程“%s”由定时、接口或事件发起，没有人等待审批结果，不能设置人工审批", target.Name)
	}
	afterID := editString(after["id"])
	outgoing := editMainOutgoing(graph, afterID)
	if len(outgoing) != 1 || editString(outgoing[0]["route"]) != "success" {
		return editFail("“%s”之后不是一条普通的连接（例如在验证回路里），不能在这里设置审批", editString(after["label"]))
	}
	for _, label := range []string{title, title + "：是否批准", title + "：驳回意见"} {
		if editLabelTaken(graph, label) {
			return editFail("步骤名称“%s”已被占用，请换一个审批名称", label)
		}
	}
	task := map[string]any{"title": title, "instructions": instructions}
	if audience != "" {
		task["audience_ref"] = audience
	}
	wait := map[string]any{"id": "approval-" + uuid.NewString(), "type": "wait", "label": title,
		"config": map[string]any{"kind": "human", "resume_schema": editApprovalSchema(), "task": task}}
	waitID := editString(wait["id"])
	check := map[string]any{"id": "approval-check-" + uuid.NewString(), "type": "condition", "label": title + "：是否批准", "config": map[string]any{}}
	rejected := map[string]any{"id": "approval-rejected-" + uuid.NewString(), "type": "deliver", "label": title + "：驳回意见",
		"config": map[string]any{"result": map[string]any{"source": "node_output", "node_id": waitID, "path": "/comment"}}}
	checkID, next := editString(check["id"]), editString(outgoing[0]["to_node_id"])
	approved := editNewEdge(checkID, next, "case")
	approved["priority"] = 1
	approved["predicate"] = map[string]any{
		"left":     map[string]any{"source": "node_output", "node_id": waitID, "path": "/decision"},
		"operator": "eq",
		"right":    map[string]any{"source": "literal", "value": editApprovalApprove},
	}
	outgoing[0]["to_node_id"] = waitID
	editSetItems(graph, "edges", append(editItems(graph, "edges"),
		editNewEdge(waitID, checkID, "success"), approved, editNewEdge(checkID, editString(rejected["id"]), "default")))
	editSetItems(graph, "nodes", append(editItems(graph, "nodes"), wait, check, rejected))
	e.note("设置人工审批：%s · %s", target.Name, title)
	return nil
}

// editApprovalParts finds the nodes an approval was compiled into.
func editApprovalParts(graph editGraph, wait map[string]any) (check, rejected map[string]any, approvedEdge map[string]any, err error) {
	shapeChanged := editFail("审批“%s”的结构已被改动，不能按审批移除；请在管理端处理", editString(wait["label"]))
	if editNodeType(wait) != "wait" || editString(editObject(wait["config"])["kind"]) != "human" {
		return nil, nil, nil, editFail("“%s”不是人工审批", editString(wait["label"]))
	}
	out := editEdges(graph, "from_node_id", editString(wait["id"]))
	if len(out) != 1 || editString(out[0]["route"]) != "success" {
		return nil, nil, nil, shapeChanged
	}
	check = editNode(graph, editString(out[0]["to_node_id"]))
	if check == nil || editNodeType(check) != "condition" || len(editEdges(graph, "to_node_id", editString(check["id"]))) != 1 {
		return nil, nil, nil, shapeChanged
	}
	for _, edge := range editEdges(graph, "from_node_id", editString(check["id"])) {
		switch editString(edge["route"]) {
		case "case":
			if approvedEdge != nil {
				return nil, nil, nil, shapeChanged
			}
			approvedEdge = edge
		case "default":
			if rejected != nil {
				return nil, nil, nil, shapeChanged
			}
			rejected = editNode(graph, editString(edge["to_node_id"]))
		default:
			return nil, nil, nil, shapeChanged
		}
	}
	if approvedEdge == nil || rejected == nil || editNodeType(rejected) != "deliver" || len(editEdges(graph, "to_node_id", editString(rejected["id"]))) != 1 {
		return nil, nil, nil, shapeChanged
	}
	return check, rejected, approvedEdge, nil
}

func (e *editSession) removeApproval(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	wait, err := e.step(graph, operation, "step", "审批")
	if err != nil {
		return err
	}
	check, rejected, approvedEdge, err := editApprovalParts(graph, wait)
	if err != nil {
		return err
	}
	waitID := editString(wait["id"])
	removed := map[string]bool{waitID: true, editString(check["id"]): true, editString(rejected["id"]): true}
	for _, node := range editItems(graph, "nodes") {
		if !removed[editString(node["id"])] && editReadsNode(node, waitID) {
			return editFail("请先调整“%s”的输入来源，它读取了审批结果", editFirst(editString(node["label"]), "执行步骤"))
		}
	}
	next := editString(approvedEdge["to_node_id"])
	var edges []map[string]any
	for _, edge := range editItems(graph, "edges") {
		if removed[editString(edge["from_node_id"])] {
			continue
		}
		if editString(edge["to_node_id"]) == waitID {
			edge["to_node_id"] = next
		}
		edges = append(edges, edge)
	}
	var nodes []map[string]any
	for _, node := range editItems(graph, "nodes") {
		if !removed[editString(node["id"])] {
			nodes = append(nodes, node)
		}
	}
	editSetItems(graph, "edges", edges)
	editSetItems(graph, "nodes", nodes)
	e.note("移除人工审批：%s · %s", target.Name, editString(wait["label"]))
	return nil
}

func (e *editSession) addFailureBranch(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	step, err := e.step(graph, operation, "step", "可能失败的步骤")
	if err != nil {
		return err
	}
	actor, err := e.member(operation)
	if err != nil {
		return err
	}
	name, err := editText(operation, "name", "失败处理步骤名称", 80)
	if err != nil {
		return err
	}
	requirement, err := editText(operation, "requirement", "失败处理要求", 2000)
	if err != nil {
		return err
	}
	if err = editGuaranteeAnchor(graph, step, "lead", "worker"); err != nil {
		return err
	}
	if editFailureEdge(graph, editString(step["id"])) != nil {
		return editFail("“%s”已经设置了失败分支，请先用 on_failure_remove 移除", editString(step["label"]))
	}
	// A failure route out of a loop body would leave the loop sideways.
	if editInLoop(graph, editString(step["id"])) {
		return editFail("“%s”在检查回路里，不能再设置失败分支；需要时把失败分支设在回路之前或之后的步骤上", editString(step["label"]))
	}
	if !actor.Relationship.Enabled {
		return editFail("失败处理须选择已启用的成员")
	}
	for _, label := range []string{name, name + "：交付"} {
		if editLabelTaken(graph, label) {
			return editFail("步骤名称“%s”已被占用", label)
		}
	}
	fallback := editMemberStep(actor, nil, "consult")
	fallback["label"] = name
	// The fallback reads what the failed step was given; everything it reads
	// ran before that step, so it is there on the failure branch too.
	if inputs := editObject(editCopy(step["inputs"])); len(inputs) > 0 {
		fallback["inputs"] = inputs
	}
	if editNodeType(fallback) == "lead" {
		editConfig(fallback)["instruction"] = requirement
	} else {
		editConfig(fallback)["result_requirement"] = requirement
	}
	fallbackID := editString(fallback["id"])
	delivery := map[string]any{"id": "failure-delivery-" + uuid.NewString(), "type": "deliver", "label": name + "：交付",
		"config": map[string]any{"result": editNodeOutputRef(fallbackID)}}
	editSetItems(graph, "nodes", append(editItems(graph, "nodes"), fallback, delivery))
	editSetItems(graph, "edges", append(editItems(graph, "edges"),
		editNewEdge(editString(step["id"]), fallbackID, "failure"), editNewEdge(fallbackID, editString(delivery["id"]), "success")))
	e.note("设置失败分支：%s · %s", target.Name, editString(step["label"]))
	return nil
}

// editMemberBefore is the nearest member or join step before a step, passing
// back over approvals; "" when the way back branches.
func editMemberBefore(graph editGraph, id string) string {
	seen := map[string]bool{id: true}
	for current := id; ; {
		var incoming []map[string]any
		for _, edge := range editEdges(graph, "to_node_id", current) {
			if editString(edge["route"]) != "failure" {
				incoming = append(incoming, edge)
			}
		}
		if len(incoming) != 1 {
			return ""
		}
		current = editString(incoming[0]["from_node_id"])
		if seen[current] {
			return ""
		}
		seen[current] = true
		if node := editNode(graph, current); node != nil && editOneOf(editNodeType(node), "lead", "worker", "join") {
			return current
		}
	}
}

// editReachable lists the nodes reachable from start without crossing skip.
func editReachable(graph editGraph, start string, skip map[string]any) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range editEdges(graph, "from_node_id", current) {
			if skip != nil && editString(edge["id"]) == editString(skip["id"]) {
				continue
			}
			if next := editString(edge["to_node_id"]); !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

func (e *editSession) removeFailureBranch(operation map[string]any) error {
	target, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	step, err := e.step(graph, operation, "step", "设置了失败分支的步骤")
	if err != nil {
		return err
	}
	failure := editFailureEdge(graph, editString(step["id"]))
	if failure == nil {
		return editFail("“%s”没有设置失败分支", editString(step["label"]))
	}
	// The branch is what only the failure edge leads to; a step the main flow
	// also reaches stays.
	main := editReachable(graph, editString(graph["entry_node_id"]), failure)
	branch := editReachable(graph, editString(failure["to_node_id"]), nil)
	for id := range branch {
		if main[id] {
			return editFail("“%s”的失败分支与主流程相连，不能整体移除；请在管理端处理", editString(step["label"]))
		}
	}
	for _, node := range editItems(graph, "nodes") {
		if id := editString(node["id"]); !branch[id] {
			for removed := range branch {
				if editReadsNode(node, removed) {
					return editFail("请先调整“%s”的输入来源，它读取了失败分支的结果", editFirst(editString(node["label"]), "执行步骤"))
				}
			}
		}
	}
	var nodes []map[string]any
	for _, node := range editItems(graph, "nodes") {
		if !branch[editString(node["id"])] {
			nodes = append(nodes, node)
		}
	}
	var edges []map[string]any
	for _, edge := range editItems(graph, "edges") {
		if editString(edge["id"]) != editString(failure["id"]) && !branch[editString(edge["from_node_id"])] {
			edges = append(edges, edge)
		}
	}
	editSetItems(graph, "nodes", nodes)
	editSetItems(graph, "edges", edges)
	e.note("移除失败分支：%s · %s", target.Name, editString(step["label"]))
	return nil
}
