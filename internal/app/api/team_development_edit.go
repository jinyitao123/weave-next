package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// Controlled edits of a team draft. A caller names members, flows, steps and
// business actions as the draft and the catalog show them; nothing here writes
// an arbitrary JSON path. The whole group is applied to a copy and discarded
// on the first invalid item.

const (
	workbenchResultProtocol = "workbench_result_v1"
	receiptVerifierID       = "weave.business-action-receipts"
	receiptCheckID          = "business-action-receipts"
)

// editError is a refusal the caller can act on; Index is the failing item.
type editError struct {
	Index   int
	Message string
}

func (e *editError) Error() string { return e.Message }

func editFail(format string, args ...any) error {
	return &editError{Index: -1, Message: fmt.Sprintf(format, args...)}
}

type editGraph = map[string]any

func decodeEditGraph(raw json.RawMessage) (editGraph, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var graph editGraph
	if err := decoder.Decode(&graph); err != nil || graph == nil {
		return nil, editFail("流程定义无法读取")
	}
	return graph, nil
}

func editObject(value any) map[string]any { object, _ := value.(map[string]any); return object }
func editString(value any) string         { text, _ := value.(string); return text }
func editOneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func editItems(graph editGraph, key string) []map[string]any {
	raw, _ := graph[key].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object := editObject(item); object != nil {
			items = append(items, object)
		}
	}
	return items
}

func editSetItems(graph editGraph, key string, items []map[string]any) {
	raw := make([]any, len(items))
	for i, item := range items {
		raw[i] = item
	}
	graph[key] = raw
}

func editNode(graph editGraph, id string) map[string]any {
	for _, node := range editItems(graph, "nodes") {
		if editString(node["id"]) == id {
			return node
		}
	}
	return nil
}

func editNodeType(node map[string]any) string { return editString(node["type"]) }

func editConfig(node map[string]any) map[string]any {
	config := editObject(node["config"])
	if config == nil {
		config = map[string]any{}
		node["config"] = config
	}
	return config
}

func editEdges(graph editGraph, key, id string) []map[string]any {
	var found []map[string]any
	for _, edge := range editItems(graph, "edges") {
		if editString(edge[key]) == id {
			found = append(found, edge)
		}
	}
	return found
}

func editNewEdge(from, to, route string) map[string]any {
	return map[string]any{"id": uuid.NewString(), "from_node_id": from, "to_node_id": to, "route": route}
}

func editDeliveryNode(graph editGraph) map[string]any {
	for _, node := range editItems(graph, "nodes") {
		if editNodeType(node) == "deliver" {
			return node
		}
	}
	return nil
}

func editResultSource(node map[string]any) string {
	return editString(editObject(editObject(node["config"])["result"])["node_id"])
}

func editNodeOutputRef(id string) map[string]any {
	return map[string]any{"source": "node_output", "node_id": id, "path": ""}
}

func editOriginalBinding() map[string]any {
	return map[string]any{"value": map[string]any{"source": "run_input", "path": ""}, "expected_type": "text"}
}

func editTextOutput() map[string]any { return map[string]any{"type": "text"} }

func editResultSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []any{"disposition", "summary", "missing_items"},
		"properties": map[string]any{
			"disposition":   map[string]any{"type": "string", "enum": []any{"complete", "needs_input"}},
			"summary":       map[string]any{"type": "string"},
			"missing_items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
}

func editProtocolOutput(output map[string]any) bool {
	return output != nil && output["type"] == "json" && reflect.DeepEqual(output["schema"], any(editResultSchema()))
}

func editPlainText(value any) bool {
	output := editObject(value)
	_, hasSchema := output["schema"]
	return output != nil && output["type"] == "text" && !hasSchema
}

// editPredecessors lists the member and join steps that run before a step.
func editPredecessors(graph editGraph, id string) []map[string]any {
	found := map[string]bool{}
	var visit func(string)
	visit = func(to string) {
		for _, edge := range editEdges(graph, "to_node_id", to) {
			from := editString(edge["from_node_id"])
			if !found[from] {
				found[from] = true
				visit(from)
			}
		}
	}
	visit(id)
	var steps []map[string]any
	for _, node := range editItems(graph, "nodes") {
		if found[editString(node["id"])] && editOneOf(editNodeType(node), "lead", "worker", "join") {
			steps = append(steps, node)
		}
	}
	return steps
}

func editParallelBranchWorker(graph editGraph, id string) bool {
	incoming, outgoing := editEdges(graph, "to_node_id", id), editEdges(graph, "from_node_id", id)
	return len(incoming) == 1 && editString(incoming[0]["route"]) == "branch" && len(outgoing) == 1 && editString(outgoing[0]["route"]) == "join"
}

func editFirst(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func editMemberStep(member *developmentMember, previous map[string]any, kind string) map[string]any {
	inputs := map[string]any{"original": editOriginalBinding()}
	if previous != nil && editOneOf(editNodeType(previous), "lead", "worker", "join") {
		expected := "text"
		if editNodeType(previous) == "join" {
			expected = "json"
		}
		inputs["previous"] = map[string]any{"value": editNodeOutputRef(editString(previous["id"])), "expected_type": expected}
	}
	node := map[string]any{"id": "step-" + uuid.NewString(), "label": member.Configuration.DisplayName, "inputs": inputs, "output": editTextOutput()}
	if member.Configuration.Role == "avatar" {
		node["type"] = "lead"
		node["config"] = map[string]any{"instruction": editFirst(member.Configuration.SystemPrompt, member.Relationship.ResultRequirement, "汇总前序结果，按团队交付要求形成最终结果。")}
		return node
	}
	node["type"] = "worker"
	node["config"] = map[string]any{"kind": kind, "agent_id": member.ID, "agent_version": 1, "result_requirement": editFirst(member.Relationship.ResultRequirement, "按照成员职责处理任务输入，返回结果与依据。")}
	return node
}

// editInitialGraph is the flow a team starts with: the lead reads the task,
// one member works on it, and the member's result is delivered.
func editInitialGraph(member *developmentMember) editGraph {
	return editGraph{
		"schema_version": 1, "entry_node_id": "lead", "input_contract": editTextOutput(), "output_contract": editTextOutput(),
		"nodes": []any{
			map[string]any{"id": "lead", "type": "lead", "label": "理解任务", "config": map[string]any{"instruction": "理解任务输入与团队目标，明确分工和输出要求。"}, "inputs": map[string]any{"original": editOriginalBinding()}, "output": editTextOutput()},
			map[string]any{"id": "work", "type": "worker", "label": member.Configuration.DisplayName, "config": map[string]any{"kind": "consult", "agent_id": member.ID, "agent_version": 1, "result_requirement": editFirst(member.Relationship.ResultRequirement, "按照成员职责处理任务输入，返回结果和依据。")}, "inputs": map[string]any{"original": editOriginalBinding(), "brief": map[string]any{"value": editNodeOutputRef("lead"), "expected_type": "text"}}, "output": editTextOutput()},
			map[string]any{"id": "deliver", "type": "deliver", "label": "交付结果", "config": map[string]any{"result": editNodeOutputRef("work")}},
		},
		"edges": []any{
			map[string]any{"id": "a", "from_node_id": "lead", "to_node_id": "work", "route": "success"},
			map[string]any{"id": "b", "from_node_id": "work", "to_node_id": "deliver", "route": "success"},
		},
	}
}

func editWithoutEdges(graph editGraph, removed ...map[string]any) []map[string]any {
	var kept []map[string]any
	for _, edge := range editItems(graph, "edges") {
		drop := false
		for _, candidate := range removed {
			drop = drop || reflect.ValueOf(edge).Pointer() == reflect.ValueOf(candidate).Pointer()
		}
		if !drop {
			kept = append(kept, edge)
		}
	}
	return kept
}

func editJoinOf(graph editGraph, parallel map[string]any) (string, error) {
	join := editString(editObject(parallel["config"])["join_node_id"])
	if node := editNode(graph, join); node == nil || editNodeType(node) != "join" {
		return "", editFail("并行区域缺少汇合步骤")
	}
	return join, nil
}

// editInsertStep adds a step after the named one and returns the new step's id.
func editInsertStep(graph editGraph, id string, member *developmentMember) (string, error) {
	previous := editNode(graph, id)
	if previous != nil && editNodeType(previous) == "parallel" {
		join, err := editJoinOf(graph, previous)
		if err != nil {
			return "", err
		}
		return editInsertStep(graph, join, member)
	}
	if editParallelBranchWorker(graph, id) {
		return "", editFail("并行分支由单个成员直接进入汇合；请把前一步指向“并行分工”，在汇合后添加串行步骤")
	}
	outgoing := editEdges(graph, "from_node_id", id)
	if previous == nil || !editOneOf(editNodeType(previous), "lead", "worker", "join") || len(outgoing) != 1 {
		return "", editFail("请选择连接完整的步骤后添加下一步")
	}
	next, after := editMemberStep(member, previous, "consult"), editString(outgoing[0]["to_node_id"])
	route := "success"
	if editString(outgoing[0]["route"]) == "join" {
		route = "join"
	}
	nextID := editString(next["id"])
	if target := editNode(graph, after); target != nil && editNodeType(target) == "deliver" {
		editConfig(target)["result"] = editNodeOutputRef(nextID)
	}
	editSetItems(graph, "edges", append(editWithoutEdges(graph, outgoing[0]), editNewEdge(id, nextID, "success"), editNewEdge(nextID, after, route)))
	editSetItems(graph, "nodes", append(editItems(graph, "nodes"), next))
	return nextID, nil
}

// editAddParallelBranch turns a member step into a parallel region, or adds
// one more branch to an existing region.
func editAddParallelBranch(graph editGraph, id string, member *developmentMember) (string, error) {
	if member.Configuration.Role != "worker" || !member.Relationship.Enabled {
		return "", editFail("并行分支只能由已启用的执行成员负责")
	}
	node := editNode(graph, id)
	if node != nil && editNodeType(node) == "parallel" {
		join, err := editJoinOf(graph, node)
		if err != nil {
			return "", err
		}
		next := editMemberStep(member, nil, "dispatch")
		nextID := editString(next["id"])
		editSetItems(graph, "nodes", append(editItems(graph, "nodes"), next))
		editSetItems(graph, "edges", append(editItems(graph, "edges"), editNewEdge(id, nextID, "branch"), editNewEdge(nextID, join, "join")))
		return nextID, nil
	}
	if node == nil || editNodeType(node) != "worker" {
		return "", editFail("并行分支须接在成员执行的步骤或“并行分工”之后")
	}
	incoming, outgoing := editEdges(graph, "to_node_id", id), editEdges(graph, "from_node_id", id)
	if len(incoming) != 1 || len(outgoing) != 1 {
		return "", editFail("该步骤连接不完整，请先调整流程")
	}
	before := editString(incoming[0]["from_node_id"])
	if editString(incoming[0]["route"]) == "branch" {
		return editAddParallelBranch(graph, before, member)
	}
	parallel, join := "parallel-"+uuid.NewString(), "join-"+uuid.NewString()
	next := editMemberStep(member, editNode(graph, before), "dispatch")
	nextID := editString(next["id"])
	editConfig(node)["kind"] = "dispatch"
	// Existing readers of this member's result stay bound to it; a later step
	// may read the join result explicitly.
	editSetItems(graph, "edges", append(editWithoutEdges(graph, incoming[0], outgoing[0]),
		editNewEdge(before, parallel, "success"), editNewEdge(parallel, id, "branch"), editNewEdge(parallel, nextID, "branch"),
		editNewEdge(id, join, "join"), editNewEdge(nextID, join, "join"), editNewEdge(join, editString(outgoing[0]["to_node_id"]), "success")))
	editSetItems(graph, "nodes", append(editItems(graph, "nodes"), next,
		map[string]any{"id": parallel, "type": "parallel", "label": "并行分工", "config": map[string]any{"join_node_id": join}},
		map[string]any{"id": join, "type": "join", "label": "汇总分支", "config": map[string]any{"policy": "all_success"}}))
	return nextID, nil
}

func editNumber(value any) float64 {
	switch number := value.(type) {
	case json.Number:
		parsed, _ := number.Float64()
		return parsed
	case float64:
		return number
	case int:
		return float64(number)
	}
	return 0
}

func editReadsNode(node map[string]any, id string) bool {
	for _, binding := range editObject(node["inputs"]) {
		if editString(editObject(editObject(binding)["value"])["node_id"]) == id {
			return true
		}
	}
	return false
}

func editRemoveStep(graph editGraph, id string) error {
	node := editNode(graph, id)
	incoming, outgoing := editEdges(graph, "to_node_id", id), editEdges(graph, "from_node_id", id)
	if node == nil || !editOneOf(editNodeType(node), "worker", "lead") || id == editString(graph["entry_node_id"]) || len(incoming) != 1 || len(outgoing) != 1 {
		return editFail("只能移除连接完整、且不是第一步的负责人或成员步骤")
	}
	branch := editString(incoming[0]["route"]) == "branch"
	var dependent []string
	for _, other := range editItems(graph, "nodes") {
		if editString(other["id"]) != id && (editReadsNode(other, id) || branch && editResultSource(other) == id) {
			dependent = append(dependent, editFirst(editString(other["label"]), "执行步骤"))
		}
	}
	if len(dependent) > 0 {
		return editFail("请先调整“%s”的输入来源", strings.Join(dependent, "、"))
	}
	var nodes []map[string]any
	for _, other := range editItems(graph, "nodes") {
		if editString(other["id"]) != id {
			nodes = append(nodes, other)
		}
	}
	var edges []map[string]any
	for _, edge := range editItems(graph, "edges") {
		if editString(edge["from_node_id"]) != id && editString(edge["to_node_id"]) != id {
			edges = append(edges, edge)
		}
	}
	previous := editString(incoming[0]["from_node_id"])
	if branch {
		if editNodeType(node) != "worker" {
			return editFail("并行分支只能移除执行成员")
		}
		siblings := editEdges(graph, "from_node_id", previous)
		if len(siblings) <= 2 {
			return editFail("并行区域至少保留两个分支")
		}
		if join := editNode(graph, editString(outgoing[0]["to_node_id"])); join != nil && editNumber(editObject(join["config"])["success_count"]) > float64(len(siblings)-1) {
			return editFail("请先降低汇合所需的成功分支数")
		}
	} else {
		for _, other := range nodes {
			if editNodeType(other) == "deliver" && editResultSource(other) == id {
				editConfig(other)["result"] = editNodeOutputRef(previous)
			}
		}
		edges = append(edges, editNewEdge(previous, editString(outgoing[0]["to_node_id"]), "success"))
	}
	editSetItems(graph, "nodes", nodes)
	editSetItems(graph, "edges", edges)
	return nil
}

// editResultProtocolIssue says why a flow's result classification cannot run.
func editResultProtocolIssue(graph editGraph) string {
	protocol, declared := graph["result_protocol"]
	if !declared {
		return ""
	}
	if protocol != workbenchResultProtocol {
		return "流程使用了暂不支持的结果分类协议。"
	}
	if !editProtocolOutput(editObject(graph["output_contract"])) {
		return "结果分类与流程输出格式不一致，请重新选择结果分类。"
	}
	delivery := editDeliveryNode(graph)
	if delivery == nil {
		return "流程缺少交付步骤。"
	}
	sourceID := editResultSource(delivery)
	source := editNode(graph, sourceID)
	if source == nil || !editOneOf(editNodeType(source), "lead", "worker") {
		return "当前交付来源不能生成结果分类。请在并行汇合后添加负责人或成员汇总步骤，并选择该步骤作为交付来源。"
	}
	outgoing := editEdges(graph, "from_node_id", sourceID)
	if len(outgoing) != 1 || editString(outgoing[0]["to_node_id"]) != editString(delivery["id"]) {
		return "结果分类只能由直接连接交付步骤的负责人或成员生成。请把最终汇总成员设为交付来源；中间步骤仍需保持原有输出格式。"
	}
	if !editProtocolOutput(editObject(source["output"])) {
		return "最终交付成员的输出格式与结果分类不一致，请重新选择结果分类。"
	}
	return ""
}

// editConfigureResultProtocol sets the delivery source and whether the flow
// may hand a "needs input" result back. previousSource names the source the
// flow had before a step was added or removed in the same edit.
func editConfigureResultProtocol(graph editGraph, enabled bool, deliverySource, previousSource string) error {
	protocol, declared := graph["result_protocol"]
	if declared && protocol != workbenchResultProtocol {
		return editFail("流程使用了暂不支持的结果分类协议。")
	}
	delivery := editDeliveryNode(graph)
	if delivery == nil {
		return editFail("流程缺少交付步骤。")
	}
	current := editResultSource(delivery)
	previousID, nextID := editFirst(previousSource, current), editFirst(deliverySource, current)
	next := editNode(graph, nextID)
	reachable := false
	for _, step := range editPredecessors(graph, editString(delivery["id"])) {
		reachable = reachable || editString(step["id"]) == nextID
	}
	if next == nil || !reachable {
		return editFail("交付来源必须是当前流程的前序步骤。")
	}
	contractIsProtocol := editProtocolOutput(editObject(graph["output_contract"]))
	if declared && (deliverySource != "" && deliverySource != current || previousSource != "" && previousSource != current) {
		if !contractIsProtocol {
			return editFail("当前结果分类格式已被其他配置修改，保留现有格式并停止切换。")
		}
		if previous := editNode(graph, previousID); previous != nil && editOneOf(editNodeType(previous), "lead", "worker") && editProtocolOutput(editObject(previous["output"])) {
			previous["output"] = editTextOutput()
		}
	}
	editConfig(delivery)["result"] = editNodeOutputRef(nextID)

	if enabled {
		if !editOneOf(editNodeType(next), "lead", "worker") {
			return editFail("当前交付来源不能生成结果分类。请在并行汇合后添加负责人或成员汇总步骤，并选择该步骤作为交付来源。")
		}
		if !declared && !editPlainText(graph["output_contract"]) {
			return editFail("当前流程已有其他输出格式，不会覆盖它；请先恢复普通文本输出再启用结果分类。")
		}
		output, hasOutput := next["output"]
		outputIsProtocol := editProtocolOutput(editObject(output))
		if !declared && hasOutput && !editPlainText(output) ||
			declared && nextID != previousID && outputIsProtocol ||
			declared && nextID == previousID && hasOutput && !editPlainText(output) && !outputIsProtocol {
			return editFail("当前交付成员已有其他输出格式，不会覆盖它；请先恢复普通文本输出再启用结果分类。")
		}
		if declared && editObject(graph["output_contract"]) != nil && !contractIsProtocol {
			return editFail("结果分类与流程输出格式不一致，请重新选择结果分类。")
		}
		graph["result_protocol"] = workbenchResultProtocol
		graph["output_contract"] = map[string]any{"type": "json", "schema": editResultSchema()}
		for _, node := range editItems(graph, "nodes") {
			if editString(node["id"]) == nextID {
				node["output"] = map[string]any{"type": "json", "schema": editResultSchema()}
			} else if declared && editProtocolOutput(editObject(node["output"])) {
				node["output"] = editTextOutput()
			}
		}
		if issue := editResultProtocolIssue(graph); issue != "" {
			return editFail("%s", issue)
		}
		return nil
	}
	if declared {
		if !contractIsProtocol {
			return editFail("当前结果分类格式已被其他配置修改，保留现有格式并停止切换。")
		}
		source := editNode(graph, current)
		member := source != nil && editOneOf(editNodeType(source), "lead", "worker")
		if member {
			if output, has := source["output"]; has && !editPlainText(output) && !editProtocolOutput(editObject(output)) {
				return editFail("当前交付成员已有其他输出格式，保留现有格式并停止切换。")
			}
		}
		delete(graph, "result_protocol")
		graph["output_contract"] = editTextOutput()
		if member && editProtocolOutput(editObject(source["output"])) {
			source["output"] = editTextOutput()
		}
	}
	return nil
}

// editFlowMembers lists the enabled members a flow actually uses.
func editFlowMembers(doc *developmentDocument, graph editGraph) []*developmentMember {
	var used []*developmentMember
	for i := range doc.Members {
		member := &doc.Members[i]
		if !member.Relationship.Enabled {
			continue
		}
		for _, node := range editItems(graph, "nodes") {
			kind := editNodeType(node)
			if kind == "lead" && member.Configuration.Role == "avatar" || kind == "worker" && member.Configuration.Role == "worker" && editString(editObject(node["config"])["agent_id"]) == member.ID {
				used = append(used, member)
				break
			}
		}
	}
	return used
}

func editStringList(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	seen, list := map[string]bool{}, make([]string, 0, len(raw))
	for _, item := range raw {
		text, isText := item.(string)
		if !isText || strings.TrimSpace(text) == "" || seen[text] {
			return nil, false
		}
		seen[text] = true
		list = append(list, text)
	}
	return list, true
}

type editCompletion struct {
	Capabilities    []string
	AllowNeedsInput bool
}

// editCompletionRequirement reads the flow's "these business actions must have
// succeeded" check; nil when the flow declares none.
func editCompletionRequirement(graph editGraph) (*editCompletion, error) {
	contract := editObject(graph["delivery_contract"])
	checks, _ := contract["required_checks"].([]any)
	var matching []map[string]any
	for _, item := range checks {
		if check := editObject(item); check != nil && check["verifier_id"] == receiptVerifierID {
			matching = append(matching, check)
		}
	}
	if len(matching) == 0 {
		return nil, nil
	}
	invalid := editFail("已有业务动作回执检查参数或引用无效，请先核对原配置")
	check := matching[0]
	params := editObject(check["parameters"])
	id := editString(check["id"])
	if len(matching) != 1 || check["verifier_version"] != "v1" || strings.TrimSpace(id) == "" || contract["external_effects_check_id"] != id || params == nil {
		return nil, invalid
	}
	for key := range params {
		if !editOneOf(key, "required_capability_ids", "when_authorized", "allow_needs_input") {
			return nil, invalid
		}
	}
	allow, isBool := params["allow_needs_input"].(bool)
	capabilities, isList := editStringList(params["required_capability_ids"])
	if params["when_authorized"] != true || !isBool || !isList || len(capabilities) < 1 || len(capabilities) > 16 {
		return nil, invalid
	}
	return &editCompletion{Capabilities: capabilities, AllowNeedsInput: allow}, nil
}

func editCapability(catalog []catalogCapability, id string) *catalogCapability {
	var found *catalogCapability
	for i := range catalog {
		if catalog[i].ID == id {
			if found != nil {
				return nil
			}
			found = &catalog[i]
		}
	}
	return found
}

func editRequireCompletionBindings(doc *developmentDocument, graph editGraph, ids []string, catalog []catalogCapability) error {
	if len(ids) < 1 || len(ids) > 16 {
		return editFail("完成检查须指定 1 至 16 个不重复的业务动作")
	}
	members := editFlowMembers(doc, graph)
	for _, id := range ids {
		action := editCapability(catalog, id)
		if action == nil || action.Status != "available" {
			return editFail("完成检查引用了当前目录中不存在或不可用的业务动作")
		}
		bound := false
		for _, member := range members {
			bound = bound || editOneOf(id, member.Configuration.BusinessCapabilityIDs...)
		}
		if !bound {
			return editFail("业务动作“%s”尚未绑定到该流程的已启用成员；请先绑定，本操作不会授予业务动作", action.Name)
		}
	}
	return nil
}

func editRequireCompletionOutput(graph editGraph) (map[string]any, error) {
	if graph["result_protocol"] != workbenchResultProtocol {
		return nil, editFail("业务动作回执完成检查须先启用“可要求补充材料”的结果分类")
	}
	if issue := editResultProtocolIssue(graph); issue != "" {
		return nil, editFail("业务动作回执完成检查需要合法的结果分类：%s", issue)
	}
	output := editObject(graph["output_contract"])
	if existing, has := editObject(graph["delivery_contract"])["output"]; has && !reflect.DeepEqual(existing, any(output)) {
		return nil, editFail("既有交付要求与流程输出格式不一致，请先核对；不能覆盖原输出要求")
	}
	return output, nil
}

func editCopy(value any) any {
	raw, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var copied any
	_ = decoder.Decode(&copied)
	return copied
}

func editConfigureCompletion(doc *developmentDocument, graph editGraph, ids []string, allowNeedsInput bool, catalog []catalogCapability) error {
	if err := editRequireCompletionBindings(doc, graph, ids, catalog); err != nil {
		return err
	}
	raw, declared := graph["delivery_contract"]
	existing := editObject(raw)
	if declared && existing == nil {
		return editFail("现有交付要求格式无法识别，不能覆盖")
	}
	if existing != nil && editNumber(existing["version"]) != 1 {
		return editFail("现有交付要求版本无法识别，不能覆盖")
	}
	var checks []any
	if rawChecks, has := existing["required_checks"]; has {
		list, isList := rawChecks.([]any)
		if !isList {
			return editFail("现有交付检查格式无法识别，不能覆盖")
		}
		checks = list
	}
	var prior map[string]any
	for _, item := range checks {
		check := editObject(item)
		if check == nil {
			return editFail("现有交付检查格式无法识别，不能覆盖")
		}
		if check["verifier_id"] == receiptVerifierID {
			if prior != nil || check["verifier_version"] != "v1" {
				return editFail("已有业务回执检查存在重复或版本冲突，请先核对原配置")
			}
			prior = check
		}
	}
	id := receiptCheckID
	if prior != nil {
		if _, err := editCompletionRequirement(graph); err != nil {
			return err
		}
		id = editString(prior["id"])
	}
	if strings.TrimSpace(id) == "" {
		return editFail("已有业务回执检查缺少有效引用，不能覆盖")
	}
	if effects, has := existing["external_effects_check_id"]; has && effects != "" && (prior == nil || effects != id) {
		return editFail("流程已有另一项外部业务效果检查，不能覆盖；请先核对现有交付要求")
	}
	taken := false
	for _, item := range checks {
		check := editObject(item)
		taken = taken || check["id"] == id && check["verifier_id"] != receiptVerifierID
	}
	artifacts, _ := existing["required_artifacts"].([]any)
	for _, item := range artifacts {
		taken = taken || editObject(item)["id"] == id
	}
	if taken {
		return editFail("业务回执检查名称已被其他交付要求占用，不能覆盖")
	}
	if prior == nil && len(checks) >= 128 {
		return editFail("现有交付检查已达上限，不能追加")
	}
	output, err := editRequireCompletionOutput(graph)
	if err != nil {
		return err
	}
	check := map[string]any{}
	for key, value := range prior {
		check[key] = value
	}
	if check["title"] == nil {
		check["title"] = "已授权业务动作具备成功回执"
	}
	required := make([]any, len(ids))
	for i, capability := range ids {
		required[i] = capability
	}
	check["id"], check["verifier_id"], check["verifier_version"] = id, receiptVerifierID, "v1"
	check["parameters"] = map[string]any{"required_capability_ids": required, "when_authorized": true, "allow_needs_input": allowNeedsInput}
	contract := map[string]any{"version": 1, "coverage": "explicit", "output": editCopy(output)}
	if existing != nil {
		contract = map[string]any{}
		for key, value := range existing {
			contract[key] = value
		}
		if _, has := existing["output"]; !has {
			contract["output"] = editCopy(output)
		}
	}
	if prior == nil {
		checks = append(checks, check)
	} else {
		replaced := make([]any, len(checks))
		for i, item := range checks {
			replaced[i] = item
			if editObject(item)["verifier_id"] == receiptVerifierID {
				replaced[i] = check
			}
		}
		checks = replaced
	}
	contract["required_checks"], contract["external_effects"], contract["external_effects_check_id"] = checks, "required", id
	graph["delivery_contract"] = contract
	return nil
}

var editBindingKey = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func editSystemParameter(name string) bool {
	return name == "idempotency_key" || name == "idempotencyKey"
}

// editParameterSources checks where a business action's parameters take their
// materials from against the action's declared parameter types.
func editParameterSources(capability *catalogCapability, raw any) ([]frozen.BusinessCapabilityParameterBinding, error) {
	var supplied []any
	if raw != nil {
		list, isList := raw.([]any)
		if !isList {
			return nil, editFail("业务参数材料来源映射格式无效")
		}
		supplied = list
	}
	declared := map[string]catalogParameter{}
	for _, parameter := range capability.Params {
		declared[parameter.Name] = parameter
	}
	chosen, mappings := map[string]string{}, []frozen.BusinessCapabilityParameterBinding{}
	for _, item := range supplied {
		mapping := editObject(item)
		name, hasName := mapping["name"].(string)
		source, hasSource := mapping["source"].(string)
		if _, repeated := chosen[name]; mapping == nil || !hasName || !hasSource || repeated {
			return nil, editFail("业务参数材料来源映射缺少唯一参数或来源")
		}
		if editSystemParameter(name) {
			return nil, editFail("系统托管的防重复提交参数不能绑定材料，请移除该映射")
		}
		parameter, known := declared[name]
		if !known {
			return nil, editFail("材料来源映射引用了当前动作中不存在的参数")
		}
		valid := parameter.Type == "string" && source != "materials.ids"
		if parameter.Type == "file" {
			valid = parameter.Multiple && source == "materials.ids" || !parameter.Multiple && source == "materials.single.id"
		}
		if !valid || !editOneOf(source, "materials.single.id", "materials.single.name", "materials.single.sha256", "materials.manifest_json", "materials.ids") {
			return nil, editFail("参数 %s 的材料来源与原生类型不匹配", name)
		}
		chosen[name] = source
		mappings = append(mappings, frozen.BusinessCapabilityParameterBinding{Name: name, Source: source})
	}
	for _, parameter := range capability.Params {
		if !editSystemParameter(parameter.Name) && parameter.Type == "file" && parameter.Multiple && chosen[parameter.Name] != "materials.ids" {
			return nil, editFail("多文件参数 %s 必须绑定本轮完整文件集合", parameter.Name)
		}
	}
	return mappings, nil
}

func editText(operation map[string]any, key, label string, max int) (string, error) {
	text, isText := operation[key].(string)
	if !isText || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > max {
		return "", editFail("请为%s填写有效内容", label)
	}
	return strings.TrimSpace(text), nil
}

func editOptionalText(operation map[string]any, key, label string, max int) (string, error) {
	if _, given := operation[key]; !given {
		return "", nil
	}
	return editText(operation, key, label, max)
}
