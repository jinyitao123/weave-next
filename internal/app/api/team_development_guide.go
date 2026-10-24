package api

// What a controlled edit may change, told in layers: the list gives one line
// per topic, and a topic gives the exact shape of its operations. Add an
// operation here in the same change that teaches editSession.apply to accept it.

const developmentOperationsVersion = "1"

// developmentConsoleOnly names what a controlled edit does not cover and where it is done instead.
const developmentConsoleOnly = "归档团队、刷新业务动作目录，以及运行、执行节点、飞书和客户端会话的操作不属于团队草稿，用工作区操作办理（见工作区操作的类别清单）。成员的 MCP 服务、技能库技能和工具许可用“成员的外部工具”类别逐项添加或移除。"

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
	{Topic: "member_settings", Title: "成员的引擎与运行设置", Covers: "更换成员的引擎、执行节点和模型，设置执行限制、工具循环、工具权限和允许的交接方式", Operations: []developmentOperationDoc{
		{Kind: "member_engine", Summary: "engine 为 loom（内置引擎）、claude、codex 或 opencode；负责人不能更换引擎。更换引擎会清空原来的节点和模型。外部引擎须给 node（节点名称，见本说明 nodes 中提供该引擎的节点），model 可省略或给空字符串表示用节点上的默认模型；改用内置引擎须给 model（见本说明 models）。不换引擎时可只改 node 或 model", Example: `{"kind":"member_engine","member":"代码实现员","engine":"claude","node":"研发一号机"}`},
		{Kind: "member_limits", Summary: "只对内置引擎成员有效。maxTokens（累计 token 上限）、maxOutputTokens（单次回复 token 上限）、stepBudget（最多执行步数）、maxCostUsd（费用上限，美元）、maxToolRepeats（同一工具连续调用上限）至少给一项，0 表示不限制；toolLoop 给 {sliceRounds, totalRounds} 自定义工具循环轮次，给 null 恢复平台默认", Example: `{"kind":"member_limits","member":"材料协调员","maxCostUsd":2,"stepBudget":40,"toolLoop":{"sliceRounds":20,"totalRounds":60}}`},
		{Kind: "member_tools", Summary: "denyAll 为 true 时禁止该成员使用任何工具，为 false 时取消；clearExternal 为 true 时一次移除成员带有的全部 MCP 服务、技能库技能、允许和需确认的工具；逐项添加或移除用“成员的外部工具”类别。至少给一项", Example: `{"kind":"member_tools","member":"材料协调员","denyAll":true}`},
		{Kind: "member_handoff", Summary: "allowed 是 consult（咨询）、dispatch（派发）、handoff（移交）中的至少一项，default 是其中之一；负责人不设置。流程里已有步骤在用的方式不能取消", Example: `{"kind":"member_handoff","member":"材料协调员","allowed":["consult","dispatch"],"default":"consult"}`},
	}, Rules: []string{developmentNamingRule, "流程按步骤所在位置决定用哪种交接方式：依次执行的步骤是咨询，并行分支是派发；一般不需要改交接方式。", "同一个节点在一次发布里只能服务一种外部引擎；两位成员使用不同外部引擎时须分配到不同节点。", "外部引擎成员在节点上用节点自己的引擎登录运行，执行限制、记忆和技能正文对它们不生效。"}},
	{Topic: "member_external", Title: "成员的外部工具", Covers: "给成员添加或移除 MCP 服务、技能库技能，以及允许直接使用或使用前需确认的工具", Operations: []developmentOperationDoc{
		{Kind: "member_mcp", Summary: "server 是本说明 mcpServers 列出的服务名称；selected 为 true 添加、false 移除。只有 usable 为 true 的服务能添加", Example: `{"kind":"member_mcp","member":"材料协调员","server":"合同库","selected":true}`},
		{Kind: "member_library_skill", Summary: "只对内置引擎成员有效。skill 是本说明 skills 列出的技能库技能名称；selected 为 true 添加、false 移除。准备试跑时固定为技能库当时的最新版本", Example: `{"kind":"member_library_skill","member":"材料协调员","skill":"报价核对","selected":true}`},
		{Kind: "member_tool_rule", Summary: "只对内置引擎成员有效。list 为 allow（允许直接使用）或 ask（使用前需确认）；tool 是工具名称；selected 为 true 添加、false 移除", Example: `{"kind":"member_tool_rule","member":"材料协调员","list":"ask","tool":"send_mail","selected":true}`},
	}, Rules: []string{developmentNamingRule,
		"MCP 服务里声明为只读的工具在试跑和正式工作中都会真实调用；其他工具（包括没有声明的）会被拒绝执行，不会写入外部系统。",
		"使用外部引擎的成员带 MCP 服务时须先选定执行节点。"}},
	{Topic: "skill", Title: "成员技能", Covers: "给成员添加、更新或移除技能正文", Operations: []developmentOperationDoc{
		{Kind: "skill", Summary: "selected 为 true 时 body 必填，同名技能被替换；为 false 时移除", Example: `{"kind":"skill","member":"材料协调员","name":"报价核对","selected":true,"body":"技能正文","description":"何时使用","alwaysActive":false}`},
	}, Rules: []string{developmentNamingRule}},
	{Topic: "business_action", Title: "业务动作", Covers: "给成员绑定或移除 Forge 业务动作，配置动作参数的材料来源", Operations: []developmentOperationDoc{
		{Kind: "capability", Summary: "capability 是目录中可绑定动作的准确名称；名称在可绑定动作里重复时改用该动作的目录标识（id）。selected 为 false 时移除。parameterSources 按参数逐项说明材料来源", Example: `{"kind":"capability","member":"材料提交员","capability":"提交材料包","selected":true,"parameterSources":[{"name":"material_file_ids","source":"materials.ids"}]}`},
	}, Rules: []string{
		developmentNamingRule,
		"单值 file 参数可不做来源映射，由模型从本次冻结文件的具名枚举中选择；只有明确绑定唯一文件时才映射 materials.single.id；多值 file 参数必须映射 materials.ids，表示本次提交的全部文件。materials.single.name、materials.single.sha256、materials.manifest_json 只用于文本参数。不能根据参数名猜测材料来源。",
		"绑定只表示成员可以使用该动作，是否执行由员工每次交接时授权。成员带业务动作的流程发布前还要设置必须办成的业务动作，见 business_completion。",
	}},
	{Topic: "flow", Title: "流程", Covers: "新建流程，修改流程名称和说明", Operations: []developmentOperationDoc{
		{Kind: "flow_add", Summary: "只有团队还没有流程时可用；member 是已启用的执行成员", Example: `{"kind":"flow_add","name":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论","member":"线索整理员"}`},
		{Kind: "flow", Summary: "name 与 description 至少给一项", Example: `{"kind":"flow","flow":"线索跟进流程","description":"接销售线索材料，交付是否跟进的结论和依据"}`},
	}, Rules: []string{developmentNamingRule, "流程说明是桌面判断这个团队是否适合承接一件工作的依据：写清接什么、需要什么、交付什么。"}},
	{Topic: "step", Title: "流程步骤", Covers: "新增串行或并行步骤，修改步骤名称、执行者和工作要求，移除步骤", Operations: []developmentOperationDoc{
		{Kind: "step_add", Summary: "after 是定位用的已有步骤名称，member 可用本组新增成员的 ref；placement 省略时为 serial，新步骤接在 after 之后；parallel 表示并行分支，位置见本组规则", Example: `{"kind":"step_add","flow":"合同复核流程","after":"并行分工","member":"合同协调员","name":"汇总检查结果","requirement":"汇总分支结论和原文依据","placement":"serial"}`},
		{Kind: "step", Summary: "可改 name、member、requirement，至少给一项；requirement 是这一步的工作要求。更换执行者时连接和输入保持不变", Example: `{"kind":"step","flow":"合同复核流程","step":"理解任务","requirement":"读完任务后列出需要核对的条款"}`},
		{Kind: "step_remove", Summary: "只能移除负责人或成员步骤，且不是第一步；其他步骤仍读取它的结果时须先调整", Example: `{"kind":"step_remove","flow":"合同复核流程","step":"汇总检查结果"}`},
	}, Rules: []string{
		developmentNamingRule,
		"串行步骤可由负责人或已启用的执行成员负责；并行分支只能由执行成员负责。一个分支可以依次有多步（最多 16 步）：placement 为 serial、after 指向分支里的某一步，新步骤接在它之后、仍在同一分支内，分支在最后一步之后进入汇合。要在并行之后增加工作，把 after 指向“并行分工”，新步骤会放在汇合之后。",
		"placement 为 parallel 时：after 是负责人步骤或汇合步骤，新分支放进紧接其后的并行分工，没有就新建一个；after 是“并行分工”，在其中再加一个分支；after 是不在分支里的执行成员步骤，新分支与它同时执行，两者组成并行分工；after 是分支里的步骤，新分支加在该分支所在的并行分工里。新建的并行分工须在同一组修改里至少有两个分支，例如两项 parallel 的 after 都指向同一负责人步骤。紧接交付的步骤之后不能直接并行，先在它之后添加汇总步骤。",
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
	// Approval (approval, approval_remove) stays accepted but undocumented until
	// the approver is enforced by the server and a rejection ends the run
	// without success; documented, agents would rely on it as an audit control.
	{Topic: "guarantee", Title: "流程保证", Covers: "为步骤加检查回路、设置失败分支：检查不通过就退回重做，步骤失败就转给指定成员处理；这些环节由流程强制执行，成员无法跳过", Operations: []developmentOperationDoc{
		{Kind: "check_loop", Summary: "step 是要检查的负责人或成员步骤；checker 是负责检查的已启用执行成员（可用本组 member_add 新增成员的 ref）；criteria 是检查标准；maxRounds 是最多做几轮（1 至 5），用完仍未通过时交付最后一轮的结果", Example: `{"kind":"check_loop","flow":"合同复核流程","step":"核对条款","checker":"格式检查员","criteria":"每条结论都引用条款原文；金额大小写一致；没有遗漏付款与违约条款","maxRounds":3}`},
		{Kind: "guarantees", Summary: "require 整体替换这条流程的断言列表（至多 16 项，空列表表示清除）；每项是 {type, step}：passes_through 要求每条通往交付的路径都经过 step，checked 要求 step 有检查回路，failure_handled 要求 step 有失败分支。断言在每次读取草稿时证明，不成立时进入 issues 并阻止发布", Example: `{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"核对条款"},{"type":"checked","step":"核对条款"},{"type":"failure_handled","step":"核对条款"}]}`},
		{Kind: "check_loop_remove", Summary: "step 是加了检查回路的步骤；移除后它直接连到原来的下一步", Example: `{"kind":"check_loop_remove","flow":"合同复核流程","step":"核对条款"}`},
		{Kind: "on_failure", Summary: "step 是可能失败的步骤；它失败时改由 member 按 requirement 处理，name 是这一步的名称，处理结果作为本次任务的交付", Example: `{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"合同协调员","name":"核对失败说明","requirement":"说明核对没有完成的原因，列出已经核对的部分和需要人工补充的内容"}`},
		{Kind: "on_failure_remove", Summary: "step 是设置了失败分支的步骤；整条失败分支一起移除", Example: `{"kind":"on_failure_remove","flow":"合同复核流程","step":"核对条款"}`},
	}, Rules: []string{
		developmentNamingRule,
		"把使用者的硬性要求同时写成断言（guarantees）：断言由服务端在流程图上证明，草稿的 workflows[].guarantees 用白话列出已证明的保证和流程结构本身带来的保证；发布前把这份列表复述给使用者确认，不成立的断言要先修改流程。",
		"结果有明确的检查标准（金额要能验算、条款要有引用、格式要符合要求）时，用检查回路落实：检查由另一位成员按标准完成，不通过就带着意见退回重做。检查员可以用低成本模型，例如先 member_add 一位 model 为低成本模型的检查员，再用 check_loop 指定它。",
		"检查回路：只能加在不在并行分支、不在已有回路里、也没有失败分支的步骤上；被检查的步骤会收到上一轮的检查意见，后续步骤读取的是最后一轮的结果。加了检查回路的步骤要先移除回路才能移除步骤。",
		"使用者要求“出错时要怎样处理”时，用失败分支落实，不要只写进成员的工作要求：工作要求只能要求成员遵守，失败分支由流程强制执行。",
		"团队流程目前不提供人工审批保证。使用者要求“必须经某人同意”时，如实说明团队流程还不能强制审批；不要用成员的工作要求冒充审批环节。",
		"失败分支：只能设置在负责人或成员执行（不在并行分支里）的步骤上，每个步骤一条。处理步骤收到失败步骤原本收到的内容，不会收到失败步骤的结果；处理结果作为本次任务的交付，原流程在失败处之后的步骤不再执行。设置失败分支后，这一步遇到可重试的运行故障也会直接进入失败分支，而不是等待重试。",
		"设置了失败分支的流程有多个交付，不能同时启用“可要求补充材料”的结果分类或必须办成的业务动作。",
	}},
	{Topic: "business_completion", Title: "必须办成的业务动作", Covers: "声明流程交付前哪些业务动作必须有成功回执；成员带业务动作的流程发布前必须设置", Operations: []developmentOperationDoc{
		{Kind: "business_completion", Summary: "capabilities 是 1 至 16 个不重复的业务动作名称（名称重复时用目录标识），且已绑定到该流程的已启用成员；allowNeedsInput 说明是否允许没有业务效果的缺件结果交还员工", Example: `{"kind":"business_completion","flow":"线索跟进流程","capabilities":["线索转商机"],"allowNeedsInput":true}`},
	}, Rules: []string{
		developmentNamingRule,
		"流程须已启用“可要求补充材料”的结果分类（见 delivery）；本修改不会自动启用它。",
		"本修改不授予动作、不替员工授权，只要求声明的动作中员工本次授权的那些有成功回执。流程已有其他业务效果检查时拒绝，不替换它。",
	}},
}
