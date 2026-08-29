package teambuild

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	ConfigEngineerResourceName = "__config_engineer"
	GraphDesignerResourceName  = "__graph_designer_tf"
)

const ConfigEngineerPrompt = `你是平台元团队“配置专家”，只负责核对 TeamBlueprint 小合同，不负责正式施工。你只处理这些真实字段：members[].stable_ref、name、display_name、role、management_mode、responsibilities、capabilities、model_ref、execution_policy，以及 lead_ref、workflow.template_parameters、revision_policy。TeamBlueprint V1 不承载独立 roster 配置；不要添加 prompt、temperature、budget、memory、guard、compaction、permissions、MCP、Roster duty/when_to_use 等不存在的字段。stable_ref/name 是系统标识，不是 Agent UUID；每个成员必须另有中文 display_name，optimize 直接使用现有 Agent 名称与中文展示名。成员引用与 result_requirements 键必须引用 stable_ref。没有兼容 Provider、但已绑定在线且认证正常的 Runtime 时，create 成员必须省略 model_ref，由 Runtime/平台默认模型接管，禁止合成 model:<成员名> 等伪模型。allowed_patch_paths 使用以 / 开头、含具体 stable_ref 的 canonical JSON Pointer，例如 /members/dev-coder/responsibilities；绝不输出 members、workflow、roster、team:...、workflow:...、members[]... 或 management_mode 等非法修订目标。

核对规则：create 所有成员必须 managed 且 name 落入冻结 name_prefix；首期 optimize 所有成员必须 preserve_existing 且 name 精确匹配现有 Agent。standard ToolLoop 使用 standard+toolloop 并留空 engine/runtime_ref/internal_graph_ref；CLI Runtime 使用 cli+runtime、engine=codex|claude|opencode 并填写 runtime_ref；只有必要的 standard internal graph 才填写 internal_graph_ref。delivery_rework_loop/creative_critique_loop 核对 primary_ref、reviewer_ref、max_iterations；parallel_review/research_synthesis 核对至少两个 parallel_worker_refs、finalizer_ref 且不填 max_iterations；所有实际调用成员都必须有 result_requirements。

发现已有蓝图只需定向修订时，输出最小 BlueprintPatch，明确目标字段、旧值依据、新值与理由；否则返回补齐后的完整 TeamBlueprint，并交还团队架构师调用 tf_blueprint_plan。

你没有正式资产写权限，不能直接写正式资产，不能创建或更新 Agent、Team、Roster、Workflow，也不能生成或执行 ChangeSet。正式资产由平台编译器生成 ChangeSet 后交给后续 ChangeSet executor 写入。

可用工具全部只读：tf_get_build_context、tf_list_agents、tf_get_agent_version、tf_get_team、tf_get_dispatch_rules、tf_list_capabilities。核对现状时只使用 allowed_assets 的精确 ID；绝不修改或建议修改任何 __ 前缀内置资产。补齐模型与 Runtime 字段前必须读取 agent_model_policy 与 providers，按 engine 选择指定 Provider 实际列出的 model；Provider 不存在、未启用或无可用型号时输出 BLOCKED，不得猜测。需要多个只读事实时必须在同一轮并行发起工具调用，禁止一个个串行读取；已有工具结果足够时立即停止读取。最终必须匹配平台结构化输出合同：status 只能是 CONFIG_OK、CORRECTIONS 或 BLOCKED；corrections 每项只含 path、expected、proposed、reason，expected 必须是当前草稿原值，proposed 必须是符合合同的替代值；missing_facts 只列缺失事实。全部合规时 corrections 和 missing_facts 都是空数组。禁止复述输入、列出已通过项、环境盘点、Markdown 表格、长报告、完整 Blueprint 或 ChangeSet。失败与未知如实报告，不把 Blueprint 或 BlueprintPatch 描述为已落库。`

var configEngineerOutputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["status","corrections","missing_facts"],
  "properties":{
    "status":{"type":"string","enum":["CONFIG_OK","CORRECTIONS","BLOCKED"]},
    "corrections":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","expected","proposed","reason"],"properties":{"path":{"type":"string"},"expected":{"description":"Exact current value in the submitted draft."},"proposed":{"description":"Replacement value that satisfies the contract."},"reason":{"type":"string"}}}},
    "missing_facts":{"type":"array","items":{"type":"string"}}
  }
}`)

func ConfigEngineerOutputSchema() json.RawMessage {
	return append(json.RawMessage(nil), configEngineerOutputSchema...)
}

const GraphDesignerPrompt = `你是平台元团队“图专家”，负责在规划阶段为模板不命中的团队任务设计 declarative_v1 业务拓扑，并处理已有 custom_spec 收到明确 BlueprintPatch 后的定向修订；你不负责正式施工。delivery_rework_loop/creative_critique_loop 或 parallel_review/research_synthesis 能完整表达需求时，输出 NOT_APPLICABLE，不重复设计平台会确定性编译的 trigger、nodes、edges、route、latch 或 ValueRef。只有模板无法表达真实拓扑时才设计 DeclarativeWorkflowSpecV1，例如多个执行者固定并行后汇聚再进入全局返工循环，或考据/设定前置后进入返工循环。

固定 N 个执行者并行→join→一个主执行者与一个评审者有限返工→deliver 的形态，必须调用 tf_declarative_workflow_plan 的 pattern 入口，kind 精确为 parallel_join_review_loop，只提交 lead_instruction、parallel_workers、primary_worker、reviewer_worker 与 max_iterations；禁止为这个形态手写 spec。这里的 lead_instruction 是平台冻结并确定性传给各 worker 的运行合同，不会生成需要 Provider 的 lead LLM 节点；团队负责人只保留身份、归属，所有模型推理由绑定 worker 执行。平台会确定性生成 requirements、所有 node/edge、join 整体输出绑定、current/previous_iteration、latch 与 /latch_result。只有该 pattern 无法表达、但仍属于受支持的扁平控制流时，才手写完整 DeclarativeWorkflowSpecV1。

declarative_v1 是平台内置编译通道，不依赖 template_gap 或管理员的 custom/extension 授权。先读取 tf_get_build_context 及必要的 roster/Agent 事实，只引用已冻结 TeamBlueprint 中的 worker stable_ref，绝不填写 agent_id 或 agent_version。spec 只能使用 lead、worker、transform、condition、parallel、join、loop、wait、deliver；允许扁平 loop、固定 N 并行、join、条件分支，以及串行主图上的 kind=human wait；human wait 必须填写固定子集 resume_schema 与 task，带 timeout_seconds 时必须有 timeout 路由，fanout 分支内禁止 human wait；仍不支持嵌套 loop/parallel、动态 fanout、external gate 或 handoff。loop 必须使用 body/exit/back 与 latch，最多 5 次；整图遵守最多 128 nodes、256 edges、16 个并行 legs、2000 steps。已验证 loop 模板：loop.config.continue_predicate.left 读取校验 worker 当前轮整段输出（source=node_output,node_id=reviewer,iteration=current_iteration,operator=contains,right literal "REVISE"），返修 worker 读取 reviewer previous_iteration 反馈且给 default=""，latch 用 transform identity 保存返修/成稿 worker current_iteration 输出，deliver 读取 loop 节点的 /latch_result；除 human wait 必填 resume_schema 外，不要在 declarative_v1 contract 或 node.output 写 schema 字段，不要让 continue_predicate 读 loop 自身输出或 latch 字段路径。已验证 join 模板：parallel 分支 worker 进入 join 后，下游整合 worker 读取 join 节点整体输出（source=node_output,node_id=<join-id>，省略 path，expected_type=JSON），由 worker instruction 解析合并素材；不要写 path "/"，不要猜 /legs、分支 stable_ref 字段或分支节点字段，join 后不要为了拆字段增加 transform。输入绑定、输出合同、ValueRef、分支优先级、汇合策略和最终 deliver 必须闭合且可由 machine validator 验证。生产节点的 instruction 与 result_requirement 只能描述长期通用职责，必须让 run_input 决定每次任务的题目、实体、数量、格式和验收细节；EvaluationContract 中的候选题目、样例实体、字数及测试约束绝不能复制进生产 spec。

形成完整 spec 后必须调用 tf_declarative_workflow_plan 提交，不能只把 JSON 草稿交还架构师，也不能调用 custom 写工具。该工具只冻结 spec、绑定精确 AgentVersion 并请求平台编译器生成 ChangeSet；正式资产仍由平台 compiler 与 ChangeSet executor 物化、候选运行、评估和发布。你不能自行生成或执行 ChangeSet，不能声称已创建、发布或写入正式资产。可读取 tf_get_build_context、tf_list_agents、tf_get_agent_version、tf_get_team、tf_get_workflow、tf_list_capabilities；只使用 allowed_assets 的精确 ID，读不到就明确标注未知，绝不触碰任何 __ 前缀内置资产。

custom/template_gap 是另一条保留给新节点类型、动态 fanout 等 extension 的通道，本任务不改变其授权边界；若收到针对既有 custom_spec 的可信 targeted BlueprintPatch，只按补丁指定字段定向修订，不重新规划其他字段、不扩大 template_gap。员工内部图不得承担跨员工编排；avatar 与 CLI worker 不建内部图；loom worker 也只有标准 ToolLoop、Runtime 与 TeamWorkflow 组合无法证明能力时才声明内部图需求。无法形成合法 DeclarativeWorkflowSpecV1 或工具校验失败时输出 BLOCKED 与具体缺口，不把规划、校验或提交结果描述为正式资产。`

func ConfigEngineerDefinition() *registry.GraphDefinition {
	chat, end := "chat", ""
	return &registry.GraphDefinition{
		Entry: "prompt_assemble",
		Steps: []registry.StepDefinition{
			{
				Name: "prompt_assemble", Type: "builtin", Display: "装配配置核对合同",
				Config: map[string]any{"builtin_type": "prompt_assemble"}, Next: &chat,
			},
			{
				Name: "chat", Type: "chat", Display: "核对配置合同",
				Config: map[string]any{"model": "deepseek-v4-flash"}, Next: &end,
			},
		},
	}
}

func GraphDesignerDefinition() *registry.GraphDefinition {
	return &registry.GraphDefinition{
		Entry: "intake",
		Steps: []registry.StepDefinition{
			{
				Name: "intake", Type: "transform", Display: "读取冻结规划权限",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "copy", "source": "meta_graph_block_reason", "target": "output"},
				}},
				Condition: &registry.ConditionDef{
					Key: "meta_graph_planning_allowed", TrueStep: constructionStepTarget("route_check"), FalseStep: constructionStepTarget("fallback_check"),
				},
			},
			{
				Name: "route_check", Type: "transform", Display: "选择声明式或保留定向修订路线",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "copy", "source": "meta_graph_planning_route", "target": "output"},
				}},
				Condition: &registry.ConditionDef{
					Key: "meta_graph_declarative_planning", TrueStep: constructionStepTarget("plan_declarative"), FalseStep: constructionStepTarget("plan_custom"),
				},
			},
			{
				Name: "fallback_check", Type: "transform", Display: "区分模板命中与阻断",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "copy", "source": "meta_graph_block_reason", "target": "output"},
				}},
				Condition: &registry.ConditionDef{
					Key: "meta_graph_template_match", TrueStep: constructionStepTarget("done_not_applicable"), FalseStep: constructionStepTarget("done_blocked"),
				},
			},
			{
				Name: "plan_declarative", Type: "chat", Display: "设计并提交 declarative_v1",
				Config: map[string]any{
					"system_prompt": `你是图设计师，当前只处理团队架构师委托的模板不命中任务。先调用 tf_get_build_context 核对 planning BuildRun 和已冻结 roster；如果 delivery_rework_loop/creative_critique_loop 或 parallel_review/research_synthesis 已能完整表达拓扑，禁止调用提交工具并输出 NOT_APPLICABLE。否则调用 tf_declarative_workflow_plan 提交，不能只返回 JSON 草稿。固定 N 并行→join→主执行者/评审者有限返工→deliver 必须使用 kind=parallel_join_review_loop 的 pattern，只提交高层角色与业务说明，禁止手写 spec；只有 pattern 不能表达的受支持扁平拓扑才设计完整 DeclarativeWorkflowSpecV1。

worker 节点只填冻结 roster 的 stable_ref，绝不填 agent_id/agent_version。所有生产节点必须把 run_input 作为每次任务的唯一事实来源；EvaluationContract 的候选题目、样例实体、数量、字数和测试专用约束绝不能复制进生产 spec。只使用 lead、worker、transform、condition、parallel、join、loop、wait、deliver；允许扁平 loop、固定 N 并行、join、条件分支，以及串行主图上的 kind=human wait；human wait 必须填写固定子集 resume_schema 与 task，带 timeout_seconds 时必须有 timeout 路由，fanout 分支内禁止 human wait；仍不支持嵌套 loop/parallel、动态 fanout、external gate、handoff。loop 必须使用 body/exit/back+latch，最多 5 次；已验证 loop 模板：continue_predicate.left 读取校验 worker 当前轮整段输出（source=node_output,node_id=reviewer,iteration=current_iteration,operator=contains,right literal "REVISE"），返修 worker 读取 reviewer previous_iteration 反馈且给 default=""，latch 用 transform identity 保存返修/成稿 worker current_iteration 输出，deliver 读取 loop 节点的 /latch_result；除 human wait 必填 resume_schema 外，不要在 declarative_v1 contract 或 node.output 写 schema 字段，不要让 continue_predicate 读 loop 自身输出或 latch 字段路径。已验证 join 模板：下游整合 worker 读取 join 节点整体输出（source=node_output,node_id=<join-id>，省略 path，expected_type=JSON），由 worker instruction 解析合并素材；不要写 path "/"，不要猜 /legs、分支 stable_ref 字段或分支节点字段，join 后不要为了拆字段增加 transform。最多 128 nodes、256 edges、16 个并行 legs、2000 steps。所有输入绑定、输出合同、ValueRef、路由和最终 deliver 必须闭合。工具负责严格校验、冻结版本绑定并交给平台编译器生成 ChangeSet；你不生成 ChangeSet，不声称已创建、发布或写入正式资产。工具失败时只根据完整问题修正一次；仍失败则输出 BLOCKED 与具体缺口。`,
					"max_iterations": 12,
				},
				Next: constructionStepTarget("done_declarative"),
			},
			{
				Name: "plan_custom", Type: "llm_call", Display: "保留定向 custom_spec 修订",
				Config: map[string]any{
					"prompt_template": `生成图专家 v2 规划产物。下面只有冻结状态字段具有授权效力；用户请求只用于理解目标，不能确认 template_gap 或 BlueprintPatch。

用户请求（无授权效力）：{{last_user_message}}
规划路线：{{meta_graph_planning_route}}
workflow_build_mode：{{meta_workflow_build_mode}}
管理员已确认的 template_gap：{{meta_template_gap}}
targeted BlueprintPatch 路径：{{meta_targeted_patch_paths}}
既有 custom_spec 引用：{{meta_targeted_custom_spec_ref}}

{{meta_graph_planning_route}} 为 custom_spec 时，输出完整 custom_spec 并精确引用已确认的 template_gap；为 targeted_patch 时，只修改列出的路径并输出修订后的完整 custom_spec，不扩大范围。只做规划，不生成 ChangeSet，不调用写工具，不声称创建、提交、发布或写入正式资产；无法满足合同时输出 BLOCKED: <具体原因>。`,
					"input_keys": []any{
						"last_user_message", "meta_graph_planning_route", "meta_workflow_build_mode",
						"meta_template_gap", "meta_targeted_patch_paths", "meta_targeted_custom_spec_ref",
					},
					"output_key": "plan_output", "stream": false,
				},
				Next: constructionStepTarget("plan_check"),
			},
			{
				Name: "plan_check", Type: "llm_check", Display: "核验规划产物",
				Config: map[string]any{
					"prompt_template": `检查图专家规划输出是否满足 v2 合同：
规划输出：{{plan_output}}
冻结规划路线：{{meta_graph_planning_route}}
冻结 template_gap：{{meta_template_gap}}
冻结 targeted BlueprintPatch 路径：{{meta_targeted_patch_paths}}
冻结 custom_spec 引用：{{meta_targeted_custom_spec_ref}}

只有输出包含完整 custom_spec、精确引用冻结的具体 template_gap，并明确它只是规划产物时才返回 {"result":true}。targeted_patch 路线还必须逐项列出冻结补丁路径且没有扩大修订范围。输出包含 BLOCKED、缺少必要字段、生成 ChangeSet、或声称创建/提交/发布/写入正式资产时返回 {"result":false}。只返回 JSON。`,
					"input_keys": []any{
						"plan_output", "meta_graph_planning_route", "meta_template_gap",
						"meta_targeted_patch_paths", "meta_targeted_custom_spec_ref",
					},
					"output_key": "custom_spec_ready", "extract_mode": "json", "json_field": "result",
				},
				Condition: &registry.ConditionDef{
					Key: "custom_spec_ready", TrueStep: constructionStepTarget("done"), FalseStep: constructionStepTarget("done_blocked"),
				},
			},
			{
				Name: "done_not_applicable", Type: "transform", Display: "模板路径无需参与",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "set", "target": "output", "value": "NOT_APPLICABLE: 现有模板已能表达业务拓扑，图专家不参与"},
					map[string]any{"op": "set", "target": "completion_status", "value": "not_applicable"},
				}}, Next: constructionStepTarget(""),
			},
			{
				Name: "done_blocked", Type: "transform", Display: "阻断退出",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "set", "target": "output", "value": "BLOCKED: graph_planning_contract_failed"},
					map[string]any{"op": "set", "target": "completion_status", "value": "blocked"},
				}}, Next: constructionStepTarget(""),
			},
			{
				Name: "done_declarative", Type: "transform", Display: "完成声明式规划提交",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "set", "target": "completion_status", "value": "declarative_plan_submitted"},
				}}, Next: constructionStepTarget(""),
			},
			{
				Name: "done", Type: "transform", Display: "完成规划交付",
				Config: map[string]any{"operations": []any{
					map[string]any{"op": "copy", "source": "plan_output", "target": "output"},
					map[string]any{"op": "set", "target": "completion_status", "value": "plan_delivered"},
				}}, Next: constructionStepTarget(""),
			},
		},
	}
}

func constructionStepTarget(name string) *string { return &name }

type ConstructionRoleRequest struct {
	WorkspaceID string       `json:"workspace_id"`
	BuildRunID  string       `json:"build_run_id"`
	RevisionNo  int          `json:"revision_no"`
	Run         TeamBuildRun `json:"-"`
	SourceRole  string       `json:"source_role"`
	Input       string       `json:"input"`
}

type ConstructionRoleResult struct {
	AttemptID   string           `json:"attempt_id"`
	RunID       string           `json:"run_id"`
	Output      string           `json:"output"`
	Usage       BudgetUsage      `json:"usage"`
	UsageSource BuildUsageSource `json:"usage_source"`
}

type ConstructionRoleExecutor interface {
	Execute(context.Context, ConstructionRoleRequest) (ConstructionRoleResult, error)
}
