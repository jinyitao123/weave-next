package api

// What a controlled edit may change, told in layers: the list gives one line
// per topic, and a topic gives the exact shape of its operations. Add an
// operation here in the same change that teaches editSession.apply to accept it.

const developmentOperationsVersion = "1"

// developmentConsoleOnly names what a controlled edit does not cover and where it is done instead.
const developmentConsoleOnly = "归档团队、刷新业务动作目录，以及运行、执行节点、代码仓库、飞书和客户端会话的操作不属于团队草稿，用工作区操作办理（见工作区操作的类别清单）。成员已带有的 MCP 服务、技能库技能、允许和需确认的工具只能整体移除，新增在 Weave 管理端之外暂不提供。"

type developmentOperationDoc struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Example string `json:"example"`
}

type developmentOperationTopic struct {
	Topic      string                    `json:"topic"`
	Title      string                    `json:"title"`
	Covers     string                    `json:"covers"`
	Operations []developmentOperationDoc `json:"operations"`
	Rules      []string                  `json:"rules"`
}

const developmentNamingRule = "成员、流程、步骤和业务动作都用草稿与目录中的准确名称引用；一次提交 1 至 24 项，任何一项不合法时整组不保存。同名对象无法区分时，先在 Weave 管理端改名。"

var developmentOperationTopics = []developmentOperationTopic{
	{Topic: "team", Title: "团队名称与目标", Covers: "修改团队名称或目标", Operations: []developmentOperationDoc{
		{Kind: "team", Summary: "name 与 objective 至少给一项", Example: `{"kind":"team","name":"线索跟进团队","objective":"整理销售线索材料，判断是否值得跟进"}`},
	}, Rules: []string{developmentNamingRule, "目标决定员工在桌面能否找到这个团队：写清处理什么事，并使用员工平时的说法。"}},
	{Topic: "member", Title: "成员", Covers: "新增、修改、停用或移出成员，设置成员的交付格式", Operations: []developmentOperationDoc{
		{Kind: "member_add", Summary: "ref、name、duty 均必填；ref 只供同一组后续修改引用。新成员是执行成员，使用内置引擎，业务动作和技能为空；模型沿用草稿中第一位执行成员的，也可用 model 指定", Example: `{"kind":"member_add","ref":"new_worker","name":"材料协调员","duty":"整理本次材料并保留原文依据"}`},
		{Kind: "member", Summary: "可改 name、duty、instruction（工作方法）、resultRequirement（结果要求）、whenToUse（何时参与）、contextInstruction（需要带上的上下文）、enabled、outputSchema（交付格式，JSON Schema 对象的字符串）、model（模型），至少给一项；负责人不能停用", Example: `{"kind":"member","member":"材料协调员","duty":"整理材料并标出缺项","outputSchema":"{\"type\":\"object\",\"properties\":{\"是否齐全\":{\"type\":\"boolean\"}},\"required\":[\"是否齐全\"],\"additionalProperties\":false}"}`},
		{Kind: "member_remove", Summary: "负责人不能移出，团队至少保留一位执行成员；仍被流程步骤使用的成员须先改步骤的执行者", Example: `{"kind":"member_remove","member":"材料协调员"}`},
	}, Rules: []string{developmentNamingRule, "model 只能取本说明 models 列出的名称，即当前组织已启用的模型，只对使用内置引擎的成员有效。models 为空表示组织还没有可用的模型，需要先为组织配置模型供应方，此时不要猜测模型名称。"}},
	{Topic: "member_settings", Title: "成员的引擎与运行设置", Covers: "更换成员的引擎、执行节点和模型，设置执行限制、工具循环、记忆、工具权限和允许的交接方式", Operations: []developmentOperationDoc{
		{Kind: "member_engine", Summary: "engine 为 loom（内置引擎）、claude、codex 或 opencode；负责人不能更换引擎。更换引擎会清空原来的节点和模型。外部引擎须给 node（节点名称，见本说明 nodes 中提供该引擎的节点），model 可省略或给空字符串表示用节点上的默认模型；改用内置引擎须给 model（见本说明 models）。不换引擎时可只改 node 或 model", Example: `{"kind":"member_engine","member":"代码实现员","engine":"claude","node":"研发一号机"}`},
		{Kind: "member_limits", Summary: "只对内置引擎成员有效。maxTokens（累计 token 上限）、maxOutputTokens（单次回复 token 上限）、stepBudget（最多执行步数）、maxCostUsd（费用上限，美元）、maxToolRepeats（同一工具连续调用上限）至少给一项，0 表示不限制；toolLoop 给 {sliceRounds, totalRounds} 自定义工具循环轮次，给 null 恢复平台默认", Example: `{"kind":"member_limits","member":"材料协调员","maxCostUsd":2,"stepBudget":40,"toolLoop":{"sliceRounds":20,"totalRounds":60}}`},
		{Kind: "member_memory", Summary: "只对内置引擎成员有效。enabled 必填；scope 可为 tenant（工作区共享）、user（按员工区分）、session（仅本次会话），省略时沿用原值", Example: `{"kind":"member_memory","member":"材料协调员","enabled":true,"scope":"user"}`},
		{Kind: "member_tools", Summary: "denyAll 为 true 时禁止该成员使用任何工具，为 false 时取消；clearExternal 为 true 时移除成员带有的 MCP 服务、技能库技能、允许和需确认的工具（带着这些配置的成员不能试跑）。至少给一项", Example: `{"kind":"member_tools","member":"材料协调员","denyAll":true}`},
		{Kind: "member_handoff", Summary: "allowed 是 consult（咨询）、dispatch（派发）、handoff（移交）中的至少一项，default 是其中之一；负责人不设置。流程里已有步骤在用的方式不能取消", Example: `{"kind":"member_handoff","member":"材料协调员","allowed":["consult","dispatch"],"default":"consult"}`},
	}, Rules: []string{developmentNamingRule, "流程按步骤所在位置决定用哪种交接方式：依次执行的步骤是咨询，并行分支是派发；一般不需要改交接方式。", "同一个节点在一次发布里只能服务一种外部引擎；两位成员使用不同外部引擎时须分配到不同节点。", "外部引擎成员在节点上用节点自己的引擎登录运行，执行限制、记忆和技能正文对它们不生效。"}},
	{Topic: "skill", Title: "成员技能", Covers: "给成员添加、更新或移除技能正文", Operations: []developmentOperationDoc{
		{Kind: "skill", Summary: "selected 为 true 时 body 必填，同名技能被替换；为 false 时移除", Example: `{"kind":"skill","member":"材料协调员","name":"报价核对","selected":true,"body":"技能正文","description":"何时使用","alwaysActive":false}`},
	}, Rules: []string{developmentNamingRule}},
	{Topic: "business_action", Title: "业务动作", Covers: "给成员绑定或移除 Forge 业务动作，配置动作参数的材料来源", Operations: []developmentOperationDoc{
		{Kind: "capability", Summary: "capability 是目录中可绑定动作的准确名称；selected 为 false 时移除。parameterSources 按参数逐项说明材料来源", Example: `{"kind":"capability","member":"材料提交员","capability":"提交材料包","selected":true,"parameterSources":[{"name":"material_file_ids","source":"materials.ids"}]}`},
	}, Rules: []string{
		developmentNamingRule,
		"单值 file 参数只能映射 materials.single.id，只在明确绑定唯一文件时才映射；多值 file 参数必须映射 materials.ids，表示本次提交的全部文件。materials.single.name、materials.single.sha256、materials.manifest_json 只用于文本参数。不能根据参数名猜测材料来源。",
		"绑定只表示成员可以使用该动作，是否执行由员工每次交接时授权。成员带业务动作的流程发布前还要设置必须办成的业务动作，见 business_completion。",
	}},
	{Topic: "flow", Title: "流程", Covers: "新建流程，修改流程名称和说明", Operations: []developmentOperationDoc{
		{Kind: "flow_add", Summary: "只有团队还没有流程时可用；member 是已启用的执行成员", Example: `{"kind":"flow_add","name":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论","member":"线索整理员"}`},
		{Kind: "flow", Summary: "name 与 description 至少给一项", Example: `{"kind":"flow","flow":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论和依据"}`},
	}, Rules: []string{developmentNamingRule, "流程说明是桌面判断这个团队是否适合承接一件工作的依据：写清接什么、需要什么、交付什么。"}},
	{Topic: "step", Title: "流程步骤", Covers: "新增串行或并行步骤，修改步骤名称、执行者和工作要求，移除步骤", Operations: []developmentOperationDoc{
		{Kind: "step_add", Summary: "after 是前一步的名称，member 可用本组新增成员的 ref；placement 省略时为 serial，parallel 表示并行分支", Example: `{"kind":"step_add","flow":"合同复核流程","after":"并行分工","member":"合同协调员","name":"汇总检查结果","requirement":"汇总分支结论和原文依据","placement":"serial"}`},
		{Kind: "step", Summary: "可改 name、member、requirement，至少给一项；requirement 是这一步的工作要求。更换执行者时连接和输入保持不变", Example: `{"kind":"step","flow":"合同复核流程","step":"理解任务","requirement":"读完任务后列出需要核对的条款"}`},
		{Kind: "step_remove", Summary: "只能移除负责人或成员步骤，且不是第一步；其他步骤仍读取它的结果时须先调整", Example: `{"kind":"step_remove","flow":"合同复核流程","step":"汇总检查结果"}`},
	}, Rules: []string{
		developmentNamingRule,
		"串行步骤可由负责人或已启用的执行成员负责；并行分支只能由执行成员负责。一个分支可以依次有多步（最多 16 步）：placement 为 serial、after 指向分支里的某一步，新步骤接在它之后、仍在同一分支内，分支在最后一步之后进入汇合。要在并行之后增加工作，把 after 指向“并行分工”，新步骤会放在汇合之后。",
	}},
	{Topic: "step_input", Title: "步骤能看到的内容", Covers: "控制每一步收到本次任务输入或哪些前序步骤的结果", Operations: []developmentOperationDoc{
		{Kind: "step_input", Summary: "source 为 run_input（本次任务输入）或 node_output（另给 from：前序步骤名称）；selected 表示添加或移除", Example: `{"kind":"step_input","flow":"合同复核流程","step":"汇总检查结果","source":"node_output","from":"核对条款","selected":true}`},
	}, Rules: []string{developmentNamingRule}},
	{Topic: "delivery", Title: "交付与结果分类", Covers: "设置交付来源、启用“可要求补充材料”的结果分类、调整并行汇合条件", Operations: []developmentOperationDoc{
		{Kind: "delivery", Summary: "from 是作为交付来源的步骤名称", Example: `{"kind":"delivery","flow":"合同复核流程","from":"汇总检查结果"}`},
		{Kind: "result_protocol", Summary: "enabled 为 true 时交付来源按“完成／需要补充”的固定结构交付，为 false 时恢复普通结果；可选 from 同时指定交付来源", Example: `{"kind":"result_protocol","flow":"合同复核流程","enabled":true}`},
		{Kind: "join", Summary: "policy 可为 all_success、fail_fast、quorum（另给 successCount）、deadline（另给 deadlineSeconds，1 至 86400）", Example: `{"kind":"join","flow":"合同复核流程","step":"汇总分支","policy":"quorum","successCount":2}`},
	}, Rules: []string{
		developmentNamingRule,
		"只有负责人或成员步骤能生成结果分类；交付来源是并行汇合时，先在汇合后添加汇总步骤并把它设为交付来源。不要改写成员已有的工作方法来塞入格式要求。",
	}},
	{Topic: "business_completion", Title: "必须办成的业务动作", Covers: "声明流程交付前哪些业务动作必须有成功回执；成员带业务动作的流程发布前必须设置", Operations: []developmentOperationDoc{
		{Kind: "business_completion", Summary: "capabilities 是 1 至 16 个不重复的业务动作名称，且已绑定到该流程的已启用成员；allowNeedsInput 说明是否允许没有业务效果的缺件结果交还员工", Example: `{"kind":"business_completion","flow":"线索跟进流程","capabilities":["线索转商机"],"allowNeedsInput":true}`},
	}, Rules: []string{
		developmentNamingRule,
		"流程须已启用“可要求补充材料”的结果分类（见 delivery）；本修改不会自动启用它。",
		"本修改不授予动作、不替员工授权，只要求声明的动作中员工本次授权的那些有成功回执。流程已有其他业务效果检查时拒绝，不替换它。",
	}},
}
