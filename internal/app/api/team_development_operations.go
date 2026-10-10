package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// editSession applies one group of controlled edits to a copy of the draft.
type editSession struct {
	doc     *developmentDocument
	catalog []catalogCapability
	graphs  map[string]editGraph
	added   map[string]string
	changes []string
}

func (e *editSession) note(format string, args ...any) {
	e.changes = append(e.changes, fmt.Sprintf(format, args...))
}

func (e *editSession) memberByID(id string) *developmentMember {
	for i := range e.doc.Members {
		if e.doc.Members[i].ID == id {
			return &e.doc.Members[i]
		}
	}
	return nil
}

// member resolves a member by its exact name, or by the ref an earlier item
// of the same group gave to a member it added.
func (e *editSession) member(operation map[string]any) (*developmentMember, error) {
	name, err := editText(operation, "member", "成员", 128)
	if err != nil {
		return nil, err
	}
	if id, added := e.added[name]; added {
		return e.memberByID(id), nil
	}
	var found *developmentMember
	for i := range e.doc.Members {
		if e.doc.Members[i].Configuration.DisplayName == name {
			if found != nil {
				return nil, editFail("成员“%s”名称重复，请先在管理端改名", name)
			}
			found = &e.doc.Members[i]
		}
	}
	if found == nil {
		return nil, editFail("成员“%s”不在当前团队草稿中", name)
	}
	return found, nil
}

func (e *editSession) graphOf(flow *developmentWorkflow) (editGraph, error) {
	if graph, decoded := e.graphs[flow.ID]; decoded {
		return graph, nil
	}
	graph, err := decodeEditGraph(flow.Graph)
	if err != nil {
		return nil, err
	}
	e.graphs[flow.ID] = graph
	return graph, nil
}

func (e *editSession) flow(operation map[string]any) (*developmentWorkflow, editGraph, error) {
	name, err := editText(operation, "flow", "流程", 128)
	if err != nil {
		return nil, nil, err
	}
	var found *developmentWorkflow
	for i := range e.doc.Workflows {
		if e.doc.Workflows[i].Name == name {
			if found != nil {
				return nil, nil, editFail("流程“%s”名称重复，请先在管理端改名", name)
			}
			found = &e.doc.Workflows[i]
		}
	}
	if found == nil {
		return nil, nil, editFail("流程“%s”不在当前团队草稿中", name)
	}
	graph, err := e.graphOf(found)
	return found, graph, err
}

func (e *editSession) step(graph editGraph, operation map[string]any, key, label string) (map[string]any, error) {
	name, err := editText(operation, key, label, 128)
	if err != nil {
		return nil, err
	}
	var found map[string]any
	for _, node := range editItems(graph, "nodes") {
		if editString(node["label"]) == name {
			if found != nil {
				return nil, editFail("步骤“%s”名称重复，请先在管理端改名", name)
			}
			found = node
		}
	}
	if found == nil {
		return nil, editFail("步骤“%s”不在该流程中", name)
	}
	return found, nil
}

func (e *editSession) capability(name string) (*catalogCapability, error) {
	var found *catalogCapability
	for i := range e.catalog {
		if e.catalog[i].Name == name {
			if found != nil {
				return nil, editFail("业务动作“%s”在目录中名称重复，无法确定是哪一个", name)
			}
			found = &e.catalog[i]
		}
	}
	if found == nil {
		return nil, editFail("业务动作“%s”不在当前业务动作目录中", name)
	}
	return found, nil
}

func editProtocolOn(graph editGraph) bool { return graph["result_protocol"] == workbenchResultProtocol }

func (e *editSession) apply(operation map[string]any) error {
	switch editString(operation["kind"]) {
	case "team":
		name, err := editOptionalText(operation, "name", "团队名称", 80)
		if err != nil {
			return err
		}
		objective, err := editOptionalText(operation, "objective", "团队目标", 2000)
		if err != nil {
			return err
		}
		if name == "" && objective == "" {
			return editFail("团队修改缺少目标字段")
		}
		if name != "" {
			e.doc.Name = name
		}
		if objective != "" {
			e.doc.Objective = objective
		}
		e.note("修改团队说明")
	case "member_add":
		return e.addMember(operation)
	case "member":
		return e.changeMember(operation)
	case "member_remove":
		target, err := e.member(operation)
		if err != nil {
			return err
		}
		workers := 0
		for _, member := range e.doc.Members {
			if member.Configuration.Role == "worker" {
				workers++
			}
		}
		if target.Configuration.Role == "avatar" || workers <= 1 {
			return editFail("团队必须保留负责人和执行成员")
		}
		for i := range e.doc.Workflows {
			graph, err := e.graphOf(&e.doc.Workflows[i])
			if err != nil {
				return err
			}
			for _, node := range editItems(graph, "nodes") {
				if editString(editObject(node["config"])["agent_id"]) == target.ID {
					return editFail("成员仍被流程“%s”的步骤“%s”使用，请先调整该步骤的执行者", e.doc.Workflows[i].Name, editString(node["label"]))
				}
			}
		}
		name, id := target.Configuration.DisplayName, target.ID
		kept := e.doc.Members[:0:0]
		for _, member := range e.doc.Members {
			if member.ID != id {
				kept = append(kept, member)
			}
		}
		e.doc.Members = kept
		e.note("移出成员：%s", name)
	case "skill":
		return e.changeSkill(operation)
	case "capability":
		return e.changeCapability(operation)
	case "flow_add":
		if len(e.doc.Workflows) > 0 {
			return editFail("当前团队已有流程，请修改现有流程")
		}
		actor, err := e.member(operation)
		if err != nil {
			return err
		}
		if actor.Configuration.Role != "worker" || !actor.Relationship.Enabled {
			return editFail("新流程须选择已启用的执行成员")
		}
		name, err := editText(operation, "name", "流程名称", 80)
		if err != nil {
			return err
		}
		description, err := editText(operation, "description", "流程说明", 2000)
		if err != nil {
			return err
		}
		flow := developmentWorkflow{ID: uuid.NewString(), Name: name, Description: description, Trigger: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)}
		e.graphs[flow.ID] = editInitialGraph(actor)
		e.doc.Workflows = append(e.doc.Workflows, flow)
		e.note("新增流程：%s", name)
	case "flow":
		target, _, err := e.flow(operation)
		if err != nil {
			return err
		}
		name, err := editOptionalText(operation, "name", "流程名称", 80)
		if err != nil {
			return err
		}
		description, err := editOptionalText(operation, "description", "流程说明", 2000)
		if err != nil {
			return err
		}
		if name == "" && description == "" {
			return editFail("流程修改缺少目标字段")
		}
		if name != "" {
			target.Name = name
		}
		if description != "" {
			target.Description = description
		}
		e.note("修改流程：%s", target.Name)
	case "step_add":
		return e.addStep(operation)
	case "step":
		return e.changeStep(operation)
	case "step_remove":
		_, graph, err := e.flow(operation)
		if err != nil {
			return err
		}
		node, err := e.step(graph, operation, "step", "步骤")
		if err != nil {
			return err
		}
		previousSource := editResultSource(editDeliveryNode(graph))
		if err = editRemoveStep(graph, editString(node["id"])); err != nil {
			return err
		}
		if err = editConfigureResultProtocol(graph, editProtocolOn(graph), "", previousSource); err != nil {
			return err
		}
		e.note("移除流程步骤：%s", editString(node["label"]))
	case "step_input":
		return e.changeStepInput(operation)
	case "delivery":
		_, graph, err := e.flow(operation)
		if err != nil {
			return err
		}
		from, err := e.step(graph, operation, "from", "交付来源")
		if err != nil {
			return err
		}
		if err = editConfigureResultProtocol(graph, editProtocolOn(graph), editString(from["id"]), ""); err != nil {
			return err
		}
		e.note("调整流程交付来源")
	case "result_protocol":
		target, graph, err := e.flow(operation)
		if err != nil {
			return err
		}
		enabled, isBool := operation["enabled"].(bool)
		if !isBool {
			return editFail("结果分类须明确选择普通结果或可要求补充材料。")
		}
		source := ""
		if _, given := operation["from"]; given {
			from, err := e.step(graph, operation, "from", "交付来源")
			if err != nil {
				return err
			}
			source = editString(from["id"])
		}
		if err = editConfigureResultProtocol(graph, enabled, source, ""); err != nil {
			return err
		}
		if enabled {
			e.note("启用团队结果分类：%s", target.Name)
		} else {
			e.note("关闭团队结果分类：%s", target.Name)
		}
	case "business_completion":
		target, graph, err := e.flow(operation)
		if err != nil {
			return err
		}
		names, isList := editStringList(operation["capabilities"])
		if !isList || len(names) < 1 || len(names) > 16 {
			return editFail("完成检查须指定 1 至 16 个不重复的准确业务动作名称")
		}
		allow, isBool := operation["allowNeedsInput"].(bool)
		if !isBool {
			return editFail("完成检查须明确是否允许缺件结果交还员工")
		}
		ids := make([]string, len(names))
		for i, name := range names {
			capability, err := e.capability(name)
			if err != nil {
				return err
			}
			ids[i] = capability.ID
		}
		if err = editConfigureCompletion(e.doc, graph, ids, allow, e.catalog); err != nil {
			return err
		}
		e.note("设置必须办成的业务动作：%s", target.Name)
	case "join":
		_, graph, err := e.flow(operation)
		if err != nil {
			return err
		}
		node, err := e.step(graph, operation, "step", "汇合步骤")
		if err != nil {
			return err
		}
		if editNodeType(node) != "join" {
			return editFail("所选步骤不是并行汇合点")
		}
		policy := editString(operation["policy"])
		config := map[string]any{"policy": policy}
		switch policy {
		case "all_success", "fail_fast":
		case "quorum":
			count, branches := editNumber(operation["successCount"]), len(editEdges(graph, "to_node_id", editString(node["id"])))
			if count != float64(int(count)) || count < 1 || int(count) > branches {
				return editFail("所需成功分支数无效")
			}
			config["success_count"] = int(count)
		case "deadline":
			seconds := editNumber(operation["deadlineSeconds"])
			if seconds != float64(int(seconds)) || seconds < 1 || seconds > 86400 {
				return editFail("最长等待秒数无效")
			}
			config["deadline_seconds"] = int(seconds)
		default:
			return editFail("汇合条件无效")
		}
		node["config"] = config
		e.note("调整并行汇合条件")
	default:
		return editFail("不支持“%s”这类团队修改；可用的类别见修改类别目录", editString(operation["kind"]))
	}
	return nil
}

func (e *editSession) addMember(operation map[string]any) error {
	ref, err := editText(operation, "ref", "新成员引用", 64)
	if err != nil {
		return err
	}
	name, err := editText(operation, "name", "成员名称", 80)
	if err != nil {
		return err
	}
	duty, err := editText(operation, "duty", "成员职责", 2000)
	if err != nil {
		return err
	}
	if _, repeated := e.added[ref]; repeated {
		return editFail("新成员引用重复")
	}
	model := ""
	for _, member := range e.doc.Members {
		if member.Configuration.DisplayName == name || member.Configuration.DisplayName == ref {
			return editFail("成员名称或引用“%s”已被占用", member.Configuration.DisplayName)
		}
		if model == "" && member.Configuration.Role == "worker" {
			model = member.Configuration.Model
		}
	}
	member := developmentMember{ID: uuid.NewString()}
	member.Configuration = teamMemberAgentConfiguration{
		DisplayName: name, Role: "worker", Engine: "loom", Model: model, SystemPrompt: duty, MemoryScope: "tenant",
		SkillNames: []string{}, Skills: []teamMemberInlineSkill{}, MCPServerIDs: []string{}, BusinessCapabilityIDs: []string{},
		BusinessCapabilityBindings: []frozen.BusinessCapabilityBinding{},
		PermissionAllow:            []string{}, PermissionAsk: []string{}, PermissionDeny: []string{},
	}
	member.Relationship = teamMemberRelationshipDraft{
		Duty: duty, WhenToUse: "流程执行到本成员负责的步骤时", ContextInstruction: "保留本次任务的原话和材料来源。",
		AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "consult", Enabled: true,
	}
	e.doc.Members = append(e.doc.Members, member)
	e.added[ref] = member.ID
	e.note("新增成员：%s", name)
	return nil
}

func (e *editSession) changeMember(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	fields := []struct {
		key, label string
		max        int
		set        func(string)
	}{
		{"name", "成员名称", 80, func(value string) { target.Configuration.DisplayName = value }},
		{"duty", "成员职责", 2000, func(value string) { target.Relationship.Duty = value }},
		{"instruction", "工作方法", 16000, func(value string) { target.Configuration.SystemPrompt = value }},
		{"resultRequirement", "结果要求", 2000, func(value string) { target.Relationship.ResultRequirement = value }},
		{"whenToUse", "参与条件", 2000, func(value string) { target.Relationship.WhenToUse = value }},
		{"contextInstruction", "协作上下文", 2000, func(value string) { target.Relationship.ContextInstruction = value }},
		{"outputSchema", "交付格式", 16000, func(value string) { target.Configuration.OutputSchema = json.RawMessage(value) }},
	}
	changed := false
	for _, field := range fields {
		value, err := editOptionalText(operation, field.key, field.label, field.max)
		if err != nil {
			return err
		}
		if value == "" {
			continue
		}
		if field.key == "outputSchema" && (!json.Valid([]byte(value)) || !strings.HasPrefix(value, "{")) {
			return editFail("交付格式须是 JSON Schema 对象")
		}
		if field.key == "name" {
			for i := range e.doc.Members {
				if e.doc.Members[i].ID != target.ID && e.doc.Members[i].Configuration.DisplayName == value {
					return editFail("成员名称“%s”已被占用", value)
				}
			}
		}
		field.set(value)
		changed = true
	}
	if raw, given := operation["enabled"]; given {
		enabled, isBool := raw.(bool)
		if !isBool || target.Configuration.Role == "avatar" && !enabled {
			return editFail("负责人不能停用；enabled 须为 true 或 false")
		}
		target.Relationship.Enabled = enabled
		changed = true
	}
	if !changed {
		return editFail("成员修改缺少目标字段")
	}
	e.note("修改成员：%s", target.Configuration.DisplayName)
	return nil
}

func (e *editSession) changeSkill(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	name, err := editText(operation, "name", "技能名称", 80)
	if err != nil {
		return err
	}
	selected, isBool := operation["selected"].(bool)
	if !isBool {
		return editFail("技能须明确选择添加或移除")
	}
	skills := []teamMemberInlineSkill{}
	for _, skill := range target.Configuration.Skills {
		if skill.Name != name {
			skills = append(skills, skill)
		}
	}
	if selected {
		body, err := editText(operation, "body", "技能内容", 50000)
		if err != nil {
			return err
		}
		description, err := editOptionalText(operation, "description", "技能说明", 2000)
		if err != nil {
			return err
		}
		skills = append(skills, teamMemberInlineSkill{Name: name, Description: description, Body: body, AlwaysActive: operation["alwaysActive"] == true})
		e.note("配置技能：%s · %s", target.Configuration.DisplayName, name)
	} else {
		e.note("移除技能：%s · %s", target.Configuration.DisplayName, name)
	}
	target.Configuration.Skills = skills
	return nil
}

func (e *editSession) changeCapability(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	name, err := editText(operation, "capability", "业务动作", 256)
	if err != nil {
		return err
	}
	capability, err := e.capability(name)
	if err != nil {
		return err
	}
	selected, isBool := operation["selected"].(bool)
	if !isBool {
		return editFail("业务动作须明确选择添加或移除")
	}
	ids := []string{}
	for _, id := range target.Configuration.BusinessCapabilityIDs {
		if id != capability.ID {
			ids = append(ids, id)
		}
	}
	bindings := []frozen.BusinessCapabilityBinding{}
	for _, binding := range target.Configuration.BusinessCapabilityBindings {
		if binding.CapabilityID != capability.ID {
			bindings = append(bindings, binding)
		}
	}
	if selected {
		if capability.Status != "available" || capability.ExecutionMode == "employee_only" {
			return editFail("业务动作“%s”当前不能绑定给团队成员", name)
		}
		sources, err := editParameterSources(capability, operation["parameterSources"])
		if err != nil {
			return err
		}
		ids = append(ids, capability.ID)
		if len(sources) > 0 {
			bindings = append(bindings, frozen.BusinessCapabilityBinding{CapabilityID: capability.ID, Parameters: sources})
		}
		e.note("添加业务动作：%s · %s", target.Configuration.DisplayName, name)
	} else {
		e.note("移除业务动作：%s · %s", target.Configuration.DisplayName, name)
	}
	target.Configuration.BusinessCapabilityIDs, target.Configuration.BusinessCapabilityBindings = ids, bindings
	return nil
}

func (e *editSession) addStep(operation map[string]any) error {
	_, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	actor, err := e.member(operation)
	if err != nil {
		return err
	}
	placement := editString(operation["placement"])
	if _, given := operation["placement"]; !given {
		placement = "serial"
	}
	if !editOneOf(placement, "serial", "parallel") {
		return editFail("步骤排列方式须为 serial 或 parallel")
	}
	if !actor.Relationship.Enabled || placement == "parallel" && actor.Configuration.Role != "worker" {
		return editFail("并行分支须选择已启用的执行成员；串行步骤可由团队负责人或已启用的执行成员负责")
	}
	after, err := e.step(graph, operation, "after", "前置步骤")
	if err != nil {
		return err
	}
	name, err := editText(operation, "name", "步骤名称", 80)
	if err != nil {
		return err
	}
	requirement, err := editText(operation, "requirement", "步骤任务", 2000)
	if err != nil {
		return err
	}
	previousSource := editResultSource(editDeliveryNode(graph))
	var selected string
	if placement == "parallel" {
		selected, err = editAddParallelBranch(graph, editString(after["id"]), actor)
		e.note("新增并行步骤：%s", name)
	} else {
		selected, err = editInsertStep(graph, editString(after["id"]), actor)
		e.note("新增串行步骤：%s", name)
	}
	if err != nil {
		return err
	}
	node := editNode(graph, selected)
	node["label"] = name
	if editNodeType(node) == "lead" {
		editConfig(node)["instruction"] = requirement
	} else {
		editConfig(node)["result_requirement"] = requirement
	}
	return editConfigureResultProtocol(graph, editProtocolOn(graph), "", previousSource)
}

func (e *editSession) changeStep(operation map[string]any) error {
	_, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	node, err := e.step(graph, operation, "step", "步骤")
	if err != nil {
		return err
	}
	name, err := editOptionalText(operation, "name", "步骤名称", 80)
	if err != nil {
		return err
	}
	requirement, err := editOptionalText(operation, "requirement", "步骤任务", 2000)
	if err != nil {
		return err
	}
	var actor *developmentMember
	if _, given := operation["member"]; given {
		if actor, err = e.member(operation); err != nil {
			return err
		}
	}
	if name == "" && requirement == "" && actor == nil {
		return editFail("步骤修改需要 name、requirement 或 member；修改处理指令请填写 requirement")
	}
	branch := editParallelBranchWorker(graph, editString(node["id"]))
	if actor != nil && (!editOneOf(editNodeType(node), "lead", "worker") || !actor.Relationship.Enabled || branch && actor.Configuration.Role != "worker") {
		return editFail("该步骤不能分配给所选成员")
	}
	config := editObject(node["config"])
	current := editFirst(editString(config["instruction"]), editString(config["result_requirement"]))
	switch {
	case actor != nil && actor.Configuration.Role == "avatar":
		node["type"] = "lead"
		node["config"] = map[string]any{"instruction": editFirst(requirement, current, actor.Configuration.SystemPrompt)}
	case actor != nil:
		kind := "consult"
		if branch {
			kind = "dispatch"
		}
		node["type"] = "worker"
		node["config"] = map[string]any{"kind": kind, "agent_id": actor.ID, "agent_version": 1, "result_requirement": editFirst(requirement, editString(config["result_requirement"]), editString(config["instruction"]), actor.Relationship.ResultRequirement)}
	case requirement != "" && editNodeType(node) == "lead":
		editConfig(node)["instruction"] = requirement
	case requirement != "":
		editConfig(node)["result_requirement"] = requirement
	}
	if name != "" {
		node["label"] = name
	}
	e.note("修改步骤：%s", editFirst(editString(node["label"]), "执行步骤"))
	return nil
}

func (e *editSession) changeStepInput(operation map[string]any) error {
	_, graph, err := e.flow(operation)
	if err != nil {
		return err
	}
	node, err := e.step(graph, operation, "step", "步骤")
	if err != nil {
		return err
	}
	if !editOneOf(editNodeType(node), "lead", "worker") {
		return editFail("该步骤不支持输入选择")
	}
	selected, isBool := operation["selected"].(bool)
	if !isBool {
		return editFail("输入来源须明确选择添加或移除")
	}
	source := editString(operation["source"])
	if !editOneOf(source, "run_input", "node_output") {
		return editFail("输入来源须为 run_input 或 node_output")
	}
	var from map[string]any
	if source == "node_output" {
		if from, err = e.step(graph, operation, "from", "前序步骤"); err != nil {
			return err
		}
	}
	fromID := editString(from["id"])
	inputs := map[string]any{}
	for key, binding := range editObject(node["inputs"]) {
		value := editObject(editObject(binding)["value"])
		if source == "run_input" && editString(value["source"]) == "run_input" || source == "node_output" && editString(value["node_id"]) == fromID {
			continue
		}
		inputs[key] = binding
	}
	if selected && source == "run_input" {
		inputs["original"] = editOriginalBinding()
	} else if selected {
		expected := ""
		for _, prior := range editPredecessors(graph, editString(node["id"])) {
			if editString(prior["id"]) == fromID {
				expected = "text"
				if editNodeType(prior) == "join" {
					expected = "json"
				}
			}
		}
		if expected == "" {
			return editFail("输入来源必须是当前步骤的前序结果")
		}
		inputs["result_"+editBindingKey.ReplaceAllString(fromID, "_")] = map[string]any{"value": editNodeOutputRef(fromID), "expected_type": expected}
	}
	node["inputs"] = inputs
	e.note("调整步骤输入：%s", editFirst(editString(node["label"]), "执行步骤"))
	return nil
}

// finish writes the edited graphs back and refuses a draft whose flows no
// longer hold together.
func (e *editSession) finish() error {
	for i := range e.doc.Workflows {
		flow := &e.doc.Workflows[i]
		graph, err := e.graphOf(flow)
		if err != nil {
			return editFail("流程“%s”：%s", flow.Name, err.Error())
		}
		if issue := editResultProtocolIssue(graph); issue != "" {
			return editFail("流程“%s”：%s", flow.Name, issue)
		}
		completion, err := editCompletionRequirement(graph)
		if err == nil && completion != nil {
			if err = editRequireCompletionBindings(e.doc, graph, completion.Capabilities, e.catalog); err == nil {
				_, err = editRequireCompletionOutput(graph)
			}
		}
		if err != nil {
			return editFail("流程“%s”：%s", flow.Name, err.Error())
		}
		raw, err := json.Marshal(graph)
		if err != nil {
			return err
		}
		flow.Graph = raw
	}
	return nil
}

// applyDevelopmentOperations applies 1 to 24 controlled edits to doc in place
// and returns what changed, one line per edit. On error doc must be discarded.
func applyDevelopmentOperations(doc *developmentDocument, operations []map[string]any, catalog []catalogCapability) ([]string, error) {
	if len(operations) < 1 || len(operations) > 24 {
		return nil, editFail("一次须提交 1 至 24 项团队修改")
	}
	session := &editSession{doc: doc, catalog: catalog, graphs: map[string]editGraph{}, added: map[string]string{}}
	for index, operation := range operations {
		err := editFail("修改格式无效")
		if operation != nil {
			err = session.apply(operation)
		}
		if err != nil {
			var refusal *editError
			if errors.As(err, &refusal) {
				refusal.Index = index
			}
			return nil, err
		}
	}
	if err := session.finish(); err != nil {
		return nil, err
	}
	return session.changes, nil
}
