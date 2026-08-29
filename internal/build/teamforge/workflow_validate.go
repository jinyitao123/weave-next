package teamforge

// Team-workflow validation bridge (plan §10.2.3). tf_wf_validate and
// tf_wf_commit run the platform's pure machine validator
// (internal/workflow/machine.Validate) over the in-memory draft. The proof
// assembly lives exactly once in internal/teameval/workflow_validate.go
// (校验器复用纪律): this file only binds the draft carrier, maps the machine
// report into the tool-facing {path, node_id, code, message, hint} contract,
// and derives the non-blocking L1 capability-point warnings through the
// shared teameval helper.

import (
	"context"

	"github.com/jinyitao123/weave/internal/build/teameval"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// WorkflowProblem is one structured validator finding. Path is a JSON
// Pointer, NodeID names the owning node when there is one, and Hint is the
// code→repair mapping (the machine validator only carries Message).
type WorkflowProblem struct {
	Path    string `json:"path"`
	NodeID  string `json:"node_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// workflowValidation is the full tf_wf_validate result: errors block a
// commit, warnings only enter the report.
type workflowValidation struct {
	Errors   []WorkflowProblem `json:"errors"`
	Warnings []WorkflowProblem `json:"warnings"`
	Valid    bool              `json:"valid"`
}

// defaultWorkflowHint is the fallback for codes without a dedicated entry.
const defaultWorkflowHint = "按该字段的 schema 修复后重新 tf_wf_validate；若为证明类错误，先确认引用的 Agent/Team 已存在且状态正确。"

// workflowHintByCode maps validator codes to model-facing repair hints. The
// F14 cluster (success path / cycle) points to the machine-native loop
// expression, which is the only legal way to encode rework.
var workflowHintByCode = map[string]string{
	// F14: rework must be a machine-native loop, never a condition back-edge.
	"workflow_success_path_unterminated": "F14：成功路径未终止。不要用 condition 直接回指上游表达返修（会被判前向环）；" +
		"改用机器原生 loop 节点：loop 的 body 边进入并行体，体末 latch 用 back 边回 loop，通过路径走 loop 的 exit 边到 deliver（body/exit/back + latch）。",
	"workflow_cycle_invalid": "F14：存在非 loop 前向环。返修必须用机器原生 loop（body/exit/back + latch）表达：" +
		"loop(latch=...，body→parallel，exit→deliver)，latch 的 back 边回 loop，绝不把 condition 直接指回上游。",
	"workflow_back_route_invalid":  "back 边只能从 loop 的 latch 节点指向该 loop 节点；请先配好 loop.latch_node_id 再连接 back 边。",
	"workflow_loop_back_missing":   "loop 的 latch 必须有一条 back 边回到 loop；用 connect(from=latch, to=loop, route=back)。",
	"workflow_loop_back_duplicate": "一个 loop 的 latch 只能有一条 back 边；删除多余的 back 边。",
	"workflow_loop_back_invalid":   "loop header 必须支配其 latch；把 latch 放进 loop body（body 边可达、back 边回 header）。",
	"workflow_loop_latch_invalid":  "loop latch 必须是独占的 lead/transform/consult worker 节点（validLoopLatch）；检查 latch_node_id 与 latch 占用。",
	"workflow_loop_body_invalid":   "loop body 边必须能沿前向边走到 latch；补全 body 内节点接线。",
	"workflow_loop_side_entry":     "loop body 只能从 header 的 body 边进入；删除从外部直插 body 的边。",
	"workflow_loop_side_exit":      "loop body 只能在 latch back 后经 header 的 exit 边离开；body 内节点不要直接连到 loop 外。",
	"workflow_loop_nested":         "v1 不支持嵌套 loop；把内层 loop 改写成外层 body 内的普通节点。",
	"workflow_loop_overlapping":    "两个 loop 的 body 不允许重叠；调整 latch/body 归属。",

	// Topology / routes.
	"workflow_graph_empty":                   "图不能为空：至少需要 entry 节点和 deliver 节点。",
	"workflow_node_id_duplicate":             "节点 id 必须唯一；为重复节点改名。",
	"workflow_edge_id_duplicate":             "边 id 必须唯一；为重复边改名或省略 edge_id 自动生成。",
	"workflow_entry_not_found":               "entry_node_id 必须指向一个已存在的节点；用 set_entry 修正。",
	"workflow_edge_source_not_found":         "边的 from_node_id 必须指向已存在节点；用 connect 时先 add_node。",
	"workflow_edge_target_not_found":         "边的 to_node_id 必须指向已存在节点；用 connect 时先 add_node。",
	"workflow_node_reference_not_found":      "config 引用的节点不存在（join_node_id / latch_node_id / node_output node_id）；先 add_node 再引用。",
	"workflow_node_unreachable":              "存在从 entry 不可达的节点；把它接进可达子图或 remove_node。",
	"workflow_success_terminal_missing":      "图缺少成功终点：需要一个 deliver 节点并让成功路径到达它。",
	"workflow_edge_cardinality_invalid":      "该节点的出边基数不合法（如 parallel≥2 branch、loop 恰 1 body+1 exit、deliver 0 出边）；按节点类型核对 route 组合。",
	"workflow_route_not_allowed":             "该节点类型不允许这条 route；按节点类型选择 success/failure/case/default/branch/join/body/exit/back 等。",
	"workflow_edge_priority_forbidden":       "priority 只允许出现在 condition 的 case 边上；删除其他边上的 priority。",
	"workflow_edge_predicate_forbidden":      "predicate 只允许出现在 condition 的 case 边上；删除其他边上的 predicate。",
	"workflow_timeout_route_without_timeout": "timeout 边要求节点 config 配置 timeout_seconds；补上后再连接。",

	// Parallel / join.
	"workflow_parallel_branch_count_invalid":       "parallel 至少需要 2 条 branch 边。",
	"workflow_parallel_branch_not_dispatch_worker": "parallel 的每条 branch 必须指向 dispatch worker。",
	"workflow_parallel_branch_not_exclusive":       "parallel 的 branch 目标不能被其他 branch 共享。",
	"workflow_parallel_nested":                     "v1 不支持嵌套 parallel；拆成顺序阶段。",
	"workflow_human_wait_in_fanout":                "human wait 只能位于 fanout join 之后的串行主图；请移动人工节点。",
	"workflow_parallel_join_invalid":               "parallel.config.join_node_id 必须指向其 join 节点。",
	"workflow_parallel_join_shared":                "join 节点不能同时属于多个 parallel。",
	"workflow_join_inputs_mismatch":                "join 的输入腿与 parallel 的 branch 不一致；检查每个 dispatch worker 的 join 边。",
	"workflow_join_policy_invalid":                 "join policy 必须是 all_success/quorum/deadline/fail_fast 之一。",
	"workflow_join_quorum_invalid":                 "quorum join 的 success_count 必须为正数。",
	"workflow_join_deadline_invalid":               "deadline join 的 deadline_seconds 必须为正数。",

	// Condition.
	"workflow_condition_case_missing":       "condition 至少需要一条 case 边；用 connect(route=case, priority, predicate)。",
	"workflow_condition_default_missing":    "condition 必须恰有一条 default 边。",
	"workflow_condition_default_duplicate":  "condition 只能有一条 default 边；删除多余 default。",
	"workflow_condition_priority_required":  "condition case 边必须带 priority。",
	"workflow_condition_priority_duplicate": "condition case 边的 priority 必须唯一。",
	"workflow_predicate_required":           "condition case 边必须带 predicate。",
	"workflow_predicate_right_required":     "该 predicate 运算符需要 right 操作数；补上 right ValueRef。",
	"workflow_predicate_right_forbidden":    "exists predicate 不允许 right；删除 right。",
	"workflow_predicate_operator_invalid":   "predicate operator 必须是 exists/eq/neq/gt/gte/lt/lte/contains/in 之一。",

	// Contracts and values.
	"workflow_output_required":                 "lead/worker/transform 节点必须声明 output 合同；用 set_node_output。",
	"workflow_output_forbidden":                "该节点类型禁止声明 output；删除 output 字段（loop/join 的输出由机器派生）。",
	"workflow_contract_invalid":                "input/output 合同 type 必须为 text/json/boolean/number 之一。",
	"workflow_output_contract_incompatible":    "deliver 结果与 workflow output_contract 类型不兼容；统一合同或调整 deliver 引用。",
	"workflow_value_source_unavailable":        "被引用的节点没有可读输出；引用 lead/worker/transform/loop/join 等有输出的节点。",
	"workflow_json_pointer_unprovable":         "ValueRef path 在对应输出 schema 中不可证明；核对 path 与输出合同字段。",
	"workflow_value_type_incompatible":         "ValueRef 与期望类型不兼容；调整 expected_type 或上游输出合同。",
	"workflow_value_loop_scope_invalid":        "loop body 的输出不能直接读给 loop 外，loop 外输入也不能引用 loop 内部节点；改用 loop 的派生输出（latch_result 等）。",
	"workflow_value_iteration_required":        "loop body 内引用 body 节点输出必须带 iteration=current/previous。",
	"workflow_value_iteration_forbidden":       "loop 不变式（loop 外→内）与普通引用禁止 iteration；删除 iteration。",
	"workflow_value_previous_default_required": "previous_iteration 引用必须有字面量 default（首轮兜底）。",
	"workflow_value_default_forbidden":         "default 只允许用于 node_output 的 previous_iteration；run_input/literal/current 禁止 default。",
	"workflow_value_not_dominating":            "被引用节点必须严格支配使用节点；调整接线使产生者先行。",

	// Trigger.
	"workflow_delivery_required":  "非会话触发（schedule/api/event）必须配置 delivery{kind,ref}。",
	"workflow_delivery_forbidden": "会话触发（conversation_explicit/auto）禁止 delivery。",
	"workflow_session_required":   "wait/handoff 节点需要会话触发（conversation_explicit/auto）；当前触发类型不支持。",

	// Authorization / team / roster.
	"workflow_team_inactive":                    "团队不在 active 状态；先经 tf_create_team/tf_set_roster 激活团队。",
	"workflow_node_unauthorized":                "节点引用的 worker 不在该团队 roster 或未启用/未授权该 kind；用 tf_set_roster 补齐 roster。",
	"workflow_authorization_proof_missing":      "授权快照缺少团队/worker 证明；先确认团队与 roster。",
	"workflow_authorization_workspace_mismatch": "授权快照与 workspace 不匹配；确认 workflow 属于当前 workspace。",
	"workflow_authorization_corrupt":            "授权快照损坏；重新 tf_wf_begin 重建。",
	"workflow_authorization_unprovable":         "授权无法证明；检查团队与 roster 是否可读。",
	"workflow_reference_proof_missing":          "触发引用的 catalog/schedule/delivery 证明缺失（L2 能力，需冻结管线）；先使用 conversation_explicit 触发。",
	"workflow_reference_not_found":              "触发引用的 catalog/schedule/delivery 不存在；核对引用 id。",
	"workflow_reference_unprovable":             "触发引用无法证明；先确认目标存在。",
	"workflow_reference_corrupt":                "触发引用损坏；重新配置 trigger。",

	// Agent versions / capabilities / dependencies / factories.
	"workflow_agent_version_proof_missing":          "缺少 AgentVersion 证明；确认图中引用了精确版本。",
	"workflow_agent_version_not_found":              "引用的 AgentVersion 不存在；用 tf_list_agents/tf_get_agent_version 核对 agent_id 与版本后钉到现有版本。",
	"workflow_agent_version_corrupt":                "AgentVersion 记录损坏；联系平台恢复或换版本。",
	"workflow_agent_version_unprovable":             "AgentVersion 无法证明（记录缺失或 graph_type 无对应 factory）；核对 agent 与版本。",
	"workflow_agent_sub_agents_forbidden":           "团队 worker/handoff 引用的 AgentVersion 不能定义 sub_agents；换无 sub_agents 的版本。",
	"workflow_agent_worker_step_forbidden":          "团队 worker/handoff 引用的 AgentVersion 不能包含内部 worker step；换普通员工图版本。",
	"workflow_agent_output_schema_invalid":          "AgentVersion 的 output_schema 不合法；用 tf_update_agent 修正。",
	"workflow_handoff_output_contract_incompatible": "handoff 输出与 workflow output_contract 不兼容；调整合同。",
	"workflow_capability_proof_missing":             "缺少能力证明；确认引用的 AgentVersion 存在。",
	"workflow_capability_unprovable":                "能力无法证明；核对 AgentVersion。",
	"workflow_cross_agent_capability_forbidden":     "团队 bundle 不允许再调用其他 agent；worker 不能配置 sub_agents。",
	"workflow_interactive_capability_forbidden":     "该执行上下文（无会话或并行腿）不允许交互能力；worker 图不要包含 yield/交互步骤。",
	"workflow_dependency_proof_missing":             "缺少依赖证明；确认引用的 AgentVersion 可读。",
	"workflow_dependency_unprovable":                "依赖无法证明；核对 AgentVersion。",
	"workflow_factory_proof_missing":                "缺少 factory 证明；确认 agent.graph_type 有已注册 factory。",
	"workflow_factory_unknown":                      "agent.graph_type 没有已注册 factory；用 tf_create_agent/tf_update_agent 修正。",
	"workflow_factory_unprovable":                   "factory 无法证明；核对 agent.graph_type。",

	// Strict encoding (DTO phase).
	"workflow_json_invalid":               "JSON 不合法；工具参数必须是合法 JSON。",
	"workflow_json_multiple_values":       "JSON 只能有一个值。",
	"workflow_field_duplicate":            "存在重复字段；删除重复 key。",
	"workflow_field_unknown":              "存在未知字段；工具不接受的字段请改用对应工具/op。",
	"workflow_field_required":             "缺少必填字段；按 schema 补全。",
	"workflow_type_invalid":               "字段类型不合法；按 schema 修正类型。",
	"workflow_schema_version_required":    "schema_version 必填且为 1。",
	"workflow_schema_version_unsupported": "schema_version 必须为 1。",
	"workflow_discriminator_required":     "缺少判别字段（type/kind/source/operator）；按 schema 补全。",
	"workflow_enum_invalid":               "枚举值不在允许集合内；按 schema 修正。",
	"workflow_integer_unsafe":             "整数超出安全 JSON 范围；使用 <= 9007199254740991 的值。",
	"workflow_unicode_invalid":            "包含非法 Unicode；检查字符串编码。",
	"workflow_json_nesting_limit":         "JSON 嵌套过深；简化结构。",
	"workflow_integer_invalid":            "期望整数；修正字段类型。",
	"workflow_json_pointer_invalid":       "ValueRef path 必须是合法 JSON Pointer（空串或 / 开头，如 /latch_result、/decision）。",
	"workflow_schema_keyword_unsupported": "JSON Schema 使用了固定子集之外的 keyword；只使用 type/properties/required/enum/const/items/additionalProperties 等受支持关键字。",
	"workflow_schema_keyword_invalid":     "JSON Schema keyword 取值不合法；按固定子集修正。",
	"workflow_schema_type_unsupported":    "JSON Schema type 不在 object/array/string/number/integer/boolean 之内；修正 type。",
	"workflow_schema_required_duplicate":  "JSON Schema required 数组存在重复项；去重。",
	"workflow_schema_enum_duplicate":      "JSON Schema enum 存在重复值；去重。",
}

// validateWorkflowDraft runs the complete machine validator over one draft
// through the shared teameval assembly. It never writes.
func validateWorkflowDraft(
	ctx context.Context,
	deps Deps,
	workspaceID string,
	draft *workflowDraft,
) (workflowValidation, error) {
	return validateWorkflowDraftForMode(ctx, deps, workspaceID, draft, false)
}

func validateTemplateWorkflowDraft(
	ctx context.Context,
	deps Deps,
	workspaceID string,
	draft *workflowDraft,
) (workflowValidation, error) {
	return validateWorkflowDraftForMode(ctx, deps, workspaceID, draft, true)
}

func validateWorkflowDraftForMode(
	ctx context.Context,
	deps Deps,
	workspaceID string,
	draft *workflowDraft,
	templateInstantiation bool,
) (workflowValidation, error) {
	var out workflowValidation
	validate := teameval.ValidateWorkflowDraft
	if templateInstantiation {
		validate = teameval.ValidateTemplateWorkflowDraft
	}
	report, err := validate(
		ctx,
		teameval.WorkflowValidateDeps{
			Teams:     deps.Teams,
			Roster:    deps.Roster,
			Agents:    deps.Agents,
			Workflows: deps.Workflows,
		},
		workspaceID,
		draft.WorkflowID,
		draft.Trigger,
		draft.Graph,
	)
	if err != nil {
		return out, err
	}
	out.Errors = mapWorkflowReport(report)
	out.Valid = len(out.Errors) == 0
	out.Warnings = workflowCapabilityWarnings(draft.Graph)
	return out, nil
}

// mapWorkflowReport converts every machine issue into the structured
// {path, node_id, code, message, hint} contract. The report is already
// sorted by the validator.
func mapWorkflowReport(report machine.Report) []WorkflowProblem {
	problems := make([]WorkflowProblem, 0, len(report.Issues))
	for _, issue := range report.Issues {
		hint := workflowHintByCode[issue.Code]
		if hint == "" {
			hint = defaultWorkflowHint
		}
		problems = append(problems, WorkflowProblem{
			Path:    issue.Path,
			NodeID:  issue.NodeID,
			Code:    issue.Code,
			Message: issue.Message,
			Hint:    hint,
		})
	}
	return problems
}

// workflowCapabilityWarnings projects the shared composition warnings:
// a fixed workflow needs real worker execution and deliver. Optional
// fanout/join and rework shapes are machine-validated when present.
func workflowCapabilityWarnings(graph machine.GraphDefinition) []WorkflowProblem {
	shared := teameval.WorkflowCapabilityWarnings(graph)
	warnings := make([]WorkflowProblem, 0, len(shared))
	for _, warning := range shared {
		warnings = append(warnings, WorkflowProblem{
			Path:    warning.Path,
			Code:    warning.Code,
			Message: warning.Message,
			Hint:    warning.Hint,
		})
	}
	return warnings
}

// workflowFactoryRegistry exposes the shared descriptor registry owned by
// teameval so the teamforge tool layer resolves factory keys exactly like
// the hard-gate evaluator (one registry, 校验器复用纪律).
func workflowFactoryRegistry() *compiler.DescriptorRegistry {
	return teameval.WorkflowFactoryRegistry()
}

// edgeRoutes is the stable route vocabulary used by the connect op and the
// route/node pairing pre-check.
var edgeRoutes = map[string]bool{
	"success": true, "failure": true, "case": true, "default": true,
	"branch": true, "join": true, "timeout": true, "body": true, "exit": true, "back": true,
}

// workflowNodeTypes is the stable node vocabulary of tf_wf_apply add_node.
var workflowNodeTypes = map[string]bool{
	"lead": true, "worker": true, "transform": true, "condition": true,
	"parallel": true, "join": true, "wait": true, "loop": true, "deliver": true, "handoff": true,
}

// workflowValueTypes is the contract/input expected_type vocabulary.
var workflowValueTypes = map[string]bool{
	"text": true, "json": true, "boolean": true, "number": true,
}

// hasWorkflowHint reports whether the hint table covers a code (used by the
// unit tests to pin the F14 cluster).
func hasWorkflowHint(code string) bool {
	_, ok := workflowHintByCode[code]
	return ok
}
