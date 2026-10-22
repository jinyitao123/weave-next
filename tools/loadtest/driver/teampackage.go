package main

import (
	"encoding/json"
	"time"
)

// The load-test team has the same shape as the game's council
// (C:\Users\jinyi\Desktop\Game\weave\team-package.json): four seats in
// parallel, a join, a ruler, a deliver (workflow "decide"), plus a
// single-member workflow ("consult"). Prompts are short and carry a member
// marker the model stub records; the run input carries a probe token that
// ties every model call to its invocation.

var seatKeys = []string{"strategist", "general", "steward", "diplomat"}

const (
	flowDecide  = "decide"
	flowConsult = "consult"
)

var inputSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"request_id", "probe", "lane", "topic"},
	"properties": map[string]any{
		"request_id": map[string]any{"type": "string"},
		"probe":      map[string]any{"type": "string"},
		"lane":       map[string]any{"type": "string"},
		"topic":      map[string]any{"type": "string"},
	},
}

var seatOutputSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"text", "recommends"},
	"properties": map[string]any{
		"text":       map[string]any{"type": "string"},
		"recommends": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	},
}

var rulerOutputSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"answers", "decree", "directives"},
	"properties": map[string]any{
		"answers":    map[string]any{"type": "object", "additionalProperties": map[string]any{}},
		"decree":     map[string]any{"type": "string"},
		"directives": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	},
}

func mustJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func memberConfig(name, role, model, prompt string, schema any, maxOut int) map[string]any {
	cfg := map[string]any{
		"business_capability_bindings": []any{}, "business_capability_ids": []any{},
		"display_name": name, "engine": "loom", "max_cost_usd": 0, "max_output_tokens": maxOut,
		"max_tokens": 0, "max_tool_repeats": 0, "mcp_server_ids": []any{}, "memory_enabled": false,
		"memory_scope": "", "model": model, "permission_allow": []any{}, "permission_ask": []any{},
		"permission_deny": []any{"*"}, "role": role, "runtime_id": "", "skill_names": []any{},
		"skills": []any{}, "step_budget": 0, "system_prompt": prompt, "tool_loop_control": nil,
	}
	if schema != nil {
		cfg["output_schema"] = mustJSONString(schema)
	}
	return cfg
}

func relationship(duty, when, kind string) map[string]any {
	return map[string]any{
		"allowed_kinds": []string{"consult", "dispatch"}, "context_instruction": "保留议题原文，不改写。",
		"default_kind": kind, "duty": duty, "enabled": true, "result_requirement": "只输出约定 JSON。",
		"when_to_use": when,
	}
}

func packageMember(key string, cfg, rel map[string]any) map[string]any {
	return map[string]any{"key": key, "configuration": cfg, "relationship": rel, "library_skills": []any{}, "mcp_servers": []any{}}
}

func workerNode(id, agent, label, kind string, inputs map[string]any, schema any) map[string]any {
	return map[string]any{
		"id": id, "type": "worker", "label": label,
		"config": map[string]any{"agent_id": agent, "agent_version": 1, "kind": kind,
			"result_requirement": "Inputs.matter 是议题（JSON）。只输出符合输出合约的 JSON。"},
		"inputs": inputs,
		"output": map[string]any{"type": "json", "schema": schema},
	}
}

func runInput() map[string]any {
	return map[string]any{"matter": map[string]any{"expected_type": "json", "value": map[string]any{"path": "", "source": "run_input"}}}
}

func decideGraph() map[string]any {
	nodes := []any{map[string]any{"id": "council", "type": "parallel", "label": "四席并议", "config": map[string]any{"join_node_id": "council_join"}}}
	edges := []any{}
	for _, k := range seatKeys {
		nodes = append(nodes, workerNode(k, k, k+" 进言", "dispatch", runInput(), seatOutputSchema))
		edges = append(edges,
			map[string]any{"id": "branch-" + k, "from_node_id": "council", "to_node_id": k, "route": "branch"},
			map[string]any{"id": "join-" + k, "from_node_id": k, "to_node_id": "council_join", "route": "join"})
	}
	rulerInputs := runInput()
	rulerInputs["council"] = map[string]any{"expected_type": "json", "value": map[string]any{"node_id": "council_join", "path": "", "source": "node_output"}}
	nodes = append(nodes,
		map[string]any{"id": "council_join", "type": "join", "label": "汇合", "config": map[string]any{"policy": "all_success"}},
		workerNode("ruler", "ruler", "拍板", "consult", rulerInputs, rulerOutputSchema),
		map[string]any{"id": "deliver", "type": "deliver", "label": "交付", "config": map[string]any{"result": map[string]any{"node_id": "ruler", "path": "", "source": "node_output"}}})
	edges = append(edges,
		map[string]any{"id": "join-ruler", "from_node_id": "council_join", "to_node_id": "ruler", "route": "success"},
		map[string]any{"id": "ruler-deliver", "from_node_id": "ruler", "to_node_id": "deliver", "route": "success"})
	return map[string]any{
		"schema_version": 1, "entry_node_id": "council", "nodes": nodes, "edges": edges,
		"input_contract":  map[string]any{"type": "json", "schema": inputSchema},
		"output_contract": map[string]any{"type": "json", "schema": rulerOutputSchema},
	}
}

func consultGraph() map[string]any {
	return map[string]any{
		"schema_version": 1, "entry_node_id": "consultant",
		"nodes": []any{
			workerNode("consultant", "consultant", "召对", "consult", runInput(), seatOutputSchema),
			map[string]any{"id": "deliver", "type": "deliver", "label": "交付", "config": map[string]any{"result": map[string]any{"node_id": "consultant", "path": "", "source": "node_output"}}},
		},
		"edges":           []any{map[string]any{"id": "consultant-deliver", "from_node_id": "consultant", "to_node_id": "deliver", "route": "success"}},
		"input_contract":  map[string]any{"type": "json", "schema": inputSchema},
		"output_contract": map[string]any{"type": "json", "schema": seatOutputSchema},
	}
}

// buildTeamPackage returns the package JSON for the given seat/ruler model.
func buildTeamPackage(seatModel, rulerModel, consultModel string) ([]byte, error) {
	trigger := map[string]any{"schema_version": 1, "type": "conversation_explicit", "config": map[string]any{}}
	members := []any{
		packageMember("lead", memberConfig("压测主持", "avatar", seatModel, "你是压测团队的主持者，不在流程中执行。", nil, 256),
			map[string]any{"allowed_kinds": []any{}, "context_instruction": "", "default_kind": "", "duty": "主持压测团队。", "enabled": true, "result_requirement": "", "when_to_use": ""}),
	}
	for _, k := range seatKeys {
		members = append(members, packageMember(k,
			memberConfig("席位 "+k, "worker", seatModel, "@@member="+k+"@@ 你是压测军议的一席。只输出 JSON。", seatOutputSchema, 512),
			relationship("压测席位 "+k+" 进言。", "decide 流程四席并议时", "dispatch")))
	}
	members = append(members,
		packageMember("ruler", memberConfig("主断", "worker", rulerModel, "@@member=ruler@@ 你听四席进言后拍板。只输出 JSON。", rulerOutputSchema, 512),
			relationship("听四席进言后拍板。", "decide 流程汇合之后", "consult")),
		packageMember("consultant", memberConfig("召对", "worker", consultModel, "@@member=consultant@@ 你应召单独对答。只输出 JSON。", seatOutputSchema, 512),
			relationship("单独对答。", "consult 流程", "consult")),
	)
	pkg := map[string]any{
		"format": "weave-team-package", "version": 1, "exported_at": time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"team": map[string]any{"name": "执行面压测团队", "objective": "执行面压测：四席并行 + 主断拍板（decide），以及单成员召对（consult）。"},
		"members": members,
		"workflows": []any{
			map[string]any{"name": flowDecide, "description": "四席并行 → 汇合 → 主断", "trigger_config": trigger, "graph_definition": decideGraph()},
			map[string]any{"name": flowConsult, "description": "单成员召对", "trigger_config": trigger, "graph_definition": consultGraph()},
		},
	}
	return json.Marshal(pkg)
}
