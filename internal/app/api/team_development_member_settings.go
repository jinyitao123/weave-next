package api

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

// How a member runs: its engine, node and model, its execution limits, its
// tool permissions and the handoff kinds it accepts. These are the
// settings the console keeps under a member's advanced panel.

// editRuntime is a node a member on an external engine may be placed on.
type editRuntime struct {
	ID      string   `json:"-"`
	Name    string   `json:"name"`
	Engines []string `json:"engines"`
	// Accepting says whether the node can take work right now.
	Accepting bool `json:"accepting"`
}

var (
	editEngines      = []string{"loom", "claude", "codex", "opencode"}
	editHandoffKinds = []string{"consult", "dispatch", "handoff"}
)

// developmentRuntimes lists the workspace's nodes for edits that place a
// member; an unreadable list offers none, which refuses a placement.
func (s *Server) developmentRuntimes(ctx context.Context, workspace string) []editRuntime {
	nodes := []editRuntime{}
	if s.Runtimes == nil {
		return nodes
	}
	stored, err := s.Runtimes.List(ctx, workspace)
	if err != nil {
		return nodes
	}
	for _, runtime := range stored {
		if runtime.DeletedAt != nil || runtime.RevokedAt != nil {
			continue
		}
		view := runtimeView(runtime)
		node := editRuntime{ID: view.ID, Name: view.Name, Engines: []string{}, Accepting: view.Accepting}
		for _, readiness := range view.EngineReadiness {
			node.Engines = append(node.Engines, readiness.Engine)
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func (e *editSession) settingsTarget(operation map[string]any, builtInOnly string) (*developmentMember, error) {
	target, err := e.member(operation)
	if err != nil {
		return nil, err
	}
	if builtInOnly != "" && engine.IsCLIEngine(target.Configuration.Engine) {
		return nil, editFail("成员“%s”使用外部引擎，%s由引擎自身决定，不在这里设置", target.Configuration.DisplayName, builtInOnly)
	}
	return target, nil
}

// node resolves a node by its exact name among those offering the engine; an
// empty name only reports which nodes there are to choose from.
func (e *editSession) node(name, wanted string) (*editRuntime, error) {
	var found *editRuntime
	offered := []string{}
	for i := range e.runtimes {
		if !editOneOf(wanted, e.runtimes[i].Engines...) {
			continue
		}
		offered = append(offered, e.runtimes[i].Name)
		if e.runtimes[i].Name == name {
			if found != nil {
				return nil, editFail("节点“%s”名称重复，请先给节点改名", name)
			}
			found = &e.runtimes[i]
		}
	}
	switch {
	case len(offered) == 0:
		return nil, editFail("还没有提供 %s 引擎的节点，请先接入节点", wanted)
	case name == "":
		return nil, editFail("外部引擎的成员须用 node 指定执行节点，可选：%s", strings.Join(offered, "、"))
	case found == nil:
		return nil, editFail("节点“%s”不提供 %s 引擎，可选：%s", name, wanted, strings.Join(offered, "、"))
	}
	return found, nil
}

func (e *editSession) changeMemberEngine(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	cfg := &target.Configuration
	chosen, err := editOptionalText(operation, "engine", "引擎", 32)
	if err != nil {
		return err
	}
	changed := false
	if chosen != "" && chosen != cfg.Engine {
		if cfg.Role == "avatar" {
			return editFail("负责人的引擎不能更换")
		}
		if !editOneOf(chosen, editEngines...) {
			return editFail("引擎须为 %s 之一", strings.Join(editEngines, "、"))
		}
		// A model and a node belong to the engine they were chosen for.
		cfg.Engine, cfg.RuntimeID, cfg.Model, changed = chosen, "", "", true
	}
	if !engine.IsCLIEngine(cfg.Engine) {
		if _, given := operation["node"]; given {
			return editFail("内置引擎的成员不选择节点")
		}
		modelChosen, err := e.chooseModel(target, operation)
		if err != nil {
			return err
		}
		if cfg.Model == "" && cfg.Role != "avatar" {
			return editFail("改用内置引擎时须同时用 model 指定模型，可选：%s", strings.Join(e.models, "、"))
		}
		if !changed && !modelChosen {
			return editFail("成员引擎修改缺少目标字段")
		}
		e.note("修改成员的引擎与模型：%s", cfg.DisplayName)
		return nil
	}
	if _, given := operation["node"]; given {
		name, err := editText(operation, "node", "节点", 128)
		if err != nil {
			return err
		}
		node, err := e.node(name, cfg.Engine)
		if err != nil {
			return err
		}
		cfg.RuntimeID, changed = node.ID, true
	}
	if cfg.RuntimeID == "" {
		_, err := e.node("", cfg.Engine)
		return err
	}
	// An empty model means the node's own default.
	if raw, given := operation["model"]; given {
		model, isText := raw.(string)
		if !isText || utf8.RuneCountInString(model) > 200 {
			return editFail("请为模型填写有效内容")
		}
		cfg.Model, changed = strings.TrimSpace(model), true
	}
	if !changed {
		return editFail("成员引擎修改缺少目标字段")
	}
	e.note("修改成员的引擎与模型：%s", cfg.DisplayName)
	return nil
}

// editLimit reads a limit that is absent, zero for "no limit", or positive.
func editLimit(operation map[string]any, key, label string, whole bool, max float64) (float64, bool, error) {
	raw, given := operation[key]
	if !given {
		return 0, false, nil
	}
	if _, isText := raw.(string); isText || raw == nil {
		return 0, false, editFail("%s须是数字，0 表示不限制", label)
	}
	value := editNumber(raw)
	if value < 0 || value > max || math.IsNaN(value) || whole && value != math.Trunc(value) {
		return 0, false, editFail("%s须是 0 至 %.0f 之间的%s，0 表示不限制", label, max, map[bool]string{true: "整数", false: "数字"}[whole])
	}
	return value, true, nil
}

func (e *editSession) changeMemberLimits(operation map[string]any) error {
	target, err := e.settingsTarget(operation, "执行限制")
	if err != nil {
		return err
	}
	cfg := &target.Configuration
	changed := false
	limits := []struct {
		key, label string
		whole      bool
		max        float64
		set        func(float64)
	}{
		{"maxTokens", "累计 token 上限", true, 1e12, func(value float64) { cfg.MaxTokens = int64(value) }},
		{"maxOutputTokens", "单次回复 token 上限", true, 1e9, func(value float64) { cfg.MaxOutputTokens = int(value) }},
		{"stepBudget", "最多执行步数", true, 1e9, func(value float64) { cfg.StepBudget = int64(value) }},
		{"maxCostUsd", "费用上限", false, 1e6, func(value float64) { cfg.MaxCostUSD = value }},
		{"maxToolRepeats", "同一工具连续调用上限", true, 1e6, func(value float64) { cfg.MaxToolRepeats = int(value) }},
	}
	for _, limit := range limits {
		value, given, err := editLimit(operation, limit.key, limit.label, limit.whole, limit.max)
		if err != nil {
			return err
		}
		if given {
			limit.set(value)
			changed = true
		}
	}
	if raw, given := operation["toolLoop"]; given {
		changed = true
		if raw == nil {
			cfg.ToolLoopControl = nil
		} else {
			loop := editObject(raw)
			slice, _, sliceErr := editLimit(loop, "sliceRounds", "每片最多轮次", true, 1000)
			total, _, totalErr := editLimit(loop, "totalRounds", "总轮次上限", true, 1<<53-1)
			control := &frozen.ToolLoopControl{SliceRounds: uint64(slice), InitialTotalRounds: uint64(total)}
			if sliceErr != nil || totalErr != nil || frozen.ValidateToolLoopControl(control) != nil {
				return editFail("toolLoop 须给出 sliceRounds（1 至 1000）和 totalRounds（至少 1），或给 null 恢复平台默认")
			}
			cfg.ToolLoopControl = control
		}
	}
	if !changed {
		return editFail("执行限制修改缺少目标字段")
	}
	e.note("修改成员的执行限制：%s", cfg.DisplayName)
	return nil
}

func (e *editSession) changeMemberTools(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	cfg := &target.Configuration
	changed := false
	if raw, given := operation["denyAll"]; given {
		deny, isBool := raw.(bool)
		if !isBool {
			return editFail("denyAll 须为 true 或 false")
		}
		kept := []string{}
		for _, rule := range cfg.PermissionDeny {
			if rule != "*" {
				kept = append(kept, rule)
			}
		}
		if deny {
			kept = append(kept, "*")
		}
		cfg.PermissionDeny, changed = kept, true
	}
	if raw, given := operation["clearExternal"]; given {
		if clear, isBool := raw.(bool); !isBool || !clear {
			return editFail("clearExternal 只能为 true：移除成员带有的 MCP 服务、技能库技能、允许和需确认的工具")
		}
		cfg.MCPServerIDs, cfg.SkillNames, cfg.PermissionAllow, cfg.PermissionAsk, changed = []string{}, []string{}, []string{}, []string{}, true
	}
	if !changed {
		return editFail("工具权限修改缺少目标字段")
	}
	e.note("修改成员的工具权限：%s", cfg.DisplayName)
	return nil
}

// handoffKindsInUse are the kinds the flows already ask of a member: a step's
// position decides its kind, so a kind in use cannot be taken away.
func (e *editSession) handoffKindsInUse(memberID string) (map[string]bool, error) {
	used := map[string]bool{}
	for i := range e.doc.Workflows {
		graph, err := e.graphOf(&e.doc.Workflows[i])
		if err != nil {
			return nil, err
		}
		for _, node := range editItems(graph, "nodes") {
			config := editObject(node["config"])
			if editNodeType(node) == "worker" && editString(config["agent_id"]) == memberID {
				used[editString(config["kind"])] = true
			}
		}
	}
	return used, nil
}

func (e *editSession) changeMemberHandoff(operation map[string]any) error {
	target, err := e.member(operation)
	if err != nil {
		return err
	}
	if target.Configuration.Role == "avatar" {
		return editFail("负责人不设置交接方式")
	}
	relation := &target.Relationship
	allowed := relation.AllowedKinds
	if raw, given := operation["allowed"]; given {
		kinds, valid := editStringList(raw)
		seen := map[string]bool{}
		for _, kind := range kinds {
			valid = valid && editOneOf(kind, editHandoffKinds...) && !seen[kind]
			seen[kind] = true
		}
		if !valid || len(kinds) == 0 {
			return editFail("allowed 须是 consult（咨询）、dispatch（派发）、handoff（移交）中不重复的至少一项")
		}
		used, err := e.handoffKindsInUse(target.ID)
		if err != nil {
			return err
		}
		for kind := range used {
			if kind != "" && !seen[kind] {
				return editFail("流程里已有步骤在用 %s，不能取消；请先调整流程", kind)
			}
		}
		allowed = kinds
	}
	if len(allowed) == 0 {
		allowed = []string{"consult", "dispatch"}
	}
	chosen, err := editOptionalText(operation, "default", "默认交接方式", 32)
	if err != nil {
		return err
	}
	if chosen == "" {
		chosen = relation.DefaultKind
		if !editOneOf(chosen, allowed...) {
			chosen = allowed[0]
		}
	}
	if !editOneOf(chosen, allowed...) {
		return editFail("默认交接方式须是允许的方式之一")
	}
	if _, given := operation["allowed"]; !given && chosen == relation.DefaultKind {
		return editFail("交接方式修改缺少目标字段")
	}
	relation.AllowedKinds, relation.DefaultKind = allowed, chosen
	e.note("修改成员的交接方式：%s", target.Configuration.DisplayName)
	return nil
}
