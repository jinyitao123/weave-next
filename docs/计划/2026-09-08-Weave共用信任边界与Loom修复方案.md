# Weave 共用信任边界与 Loom 修复方案

2026-09-08。当前进展：输入绑定 `ce5fe66701083237398df038998899aa6d57fe7c`、交付核验 `1bad43f71692a89b1502dcc075305b32fe267dc2` 已提交；受管 MCP 参数及 CLI 发布绑定已提交为 `acfa5233a4d3fdc427a035778424e8de1fcda1ce` 并通过机制检查，尚未部署。Loom 持久预算接入与新业务复验仍未完成。初始审计基于 `da0e5c7eb5ba0f85c6381bd395418dba77f8aaf9` 和[双场景证据](../验收/2026-09-07-Weave任务信任双场景验证/)，历史失败与原始评分保持不变。

## 判断

问题跨越派发、工具使用、执行停止和业务验收。CLI 与 Loom 都受共用入口和验收缺口影响，切换引擎无法修复这些缺口。Loom 另有工具循环停止语义不完整的问题。优先修复现有路径的契约与状态转换，保留现有任务、运行、成员、attempt、检查点和回执结构。

| 已观察到的失败 | 已证实的原因或缺口 | 修改归属 |
|---|---|---|
| 第二次电商任务 INV-440 被两次派成旧 VBR-52 | 前台模型生成的派发正文已错误；后端未与当前已授权输入核对 | Workbench Host、派发 API；两种引擎共用 |
| 首轮电商原 verifier 7/7，但存在额外错误日历事件 | 模型误用参数并将 create 当 update；Mock 接受宽松参数；基准只检查必需事件存在 | 工具适配与权限、成员指令、独立业务核验 |
| 日冕生成部分模型文件后运行成功，图纸、应用和最终交付仍缺失 | 正常结束与任务要求满足没有形成独立判据 | Weave 工作流、交付记录及 Workbench 展示 |
| 日冕最后一轮到 20 轮后停止 | Weave 固定 ToolLoop 上限；Loom v0.8.1 耗尽后无工具总结并返回 nil error | Loom ToolLoop 及 Weave 适配 |

第二次电商最终被主动取消，不能由剩余业务项未完成推断执行引擎本来必然失败。三次取消后的环境有写入，但目前不能逐条归属到具体运行；不得编造归因。日冕历史恢复的回执检查成立，也不能抵消后续业务失败。

## 1. 先绑定用户实际交付的输入

在现有 WorkTask/监督会话中保存不可变的派发输入修订，绑定来源消息或已审阅正文、会话、团队、工作流和既有授权。派发时由 Host 读取该记录并提供正文、摘要和固定幂等键；模型不再重抄任务正文，也不能任意选择历史修订。

服务端在入队前核对修订归属和有效性。旧正文、旧修订、跨会话引用或改写正文，即使换新的 client_request_id 也应被拒绝。摘要由可信代码计算；让模型同时提交正文与摘要没有来源证明作用。原始正文与执行解释分开保留，合法性检查不能覆盖原文。沿用已有授权，不额外增加每次派发确认。

代码入口：`workbench/packages/mcp/mcp-client/src/tools.ts:315`、`internal/app/mcpstdio/tools.go:149`、`internal/app/api/team_dispatch.go:41`。当前指纹只约束同一个请求键的事实一致性，不能证明它属于当前用户任务。界面 `workbench/packages/bundle/workbench-app/src/index.ts:856` 也取模型工具参数作为摘要，不能用页面和后端文字一致证明输入正确。

验收必须故意提交旧正文、旧引用、新幂等键和响应丢失后的重试；错误请求入队前拒绝、业务写入为零，正确请求沿 Host→API→队列→成员保持同一输入。CLI 与 Loom 分别覆盖。

## 2. 将执行结果和交付核验分开

沿用现有 WorkTask 和成果记录，补齐执行、交付核验、用户接受三个维度。引擎只能报告正常结束、耗尽、中断、错误、可接续性与已观察证据。任务要求是否满足由 Weave 的契约检查决定，用户接受单独记录。

必需文件缺失、必需检查未执行、外部对象未核实或存在未处理副作用时，交付状态不能进入核验通过。执行状态可以忠实保留为正常结束。自然语言“PASS”、有一份总结、某个 deliver 节点返回，以及若干测试通过，都不能替代全部必需条件。

文件任务核对实际文件及归属；电商任务核对实际外部状态和全部新增写入。原始 benchmark 评分保持不变，另存业务信任检查；新增保护后必须标注实验条件发生了变化。

验收至少覆盖：模型正常结束但缺文件、复核文字称 PASS 但必需项未知、基准得满分但多写错误对象，以及真正满足全部条件四种情况。前后三种状态必须在 API 与 Workbench 一致。

## 3. 工具约束覆盖 CLI 和 Loom

复核员使用实际只读工具权限与凭据。写入成员按已发现的 schema 调用工具，创建与更新分开；写后回读核对目标对象及实际参数。已有写入结果不确定时先核对，不把重试自动解释为再创建一次。外部系统支持幂等键时使用稳定键；不支持时明确暴露不确定性，不承诺任意外部写入严格只发生一次。

优先保护当前受管 MCP 调用边界。CLI 内部若能通过其他工具绕过该边界，就不能仅靠外层回执或只读提示词宣称已建立权限隔离；该执行配置需要限制能力或标为不支持相应信任承诺。取消时持续留存已观察工具事实；当前取消样本缺失的归属不能事后靠推断补齐。

本次电商已观察到这种路径：Claude CLI 在 Bash 内用 Python/curl 直接访问 fixture 的 `/mcp`。因此只改 Weave ToolBroker 不覆盖这次错误写入。现有 `internal/kernel/mcphost/http_host.go:435` 已有工具名 allowlist，应复用并补参数 schema 校验；执行环境同时必须保证只读成员不能直连上游写入。新增代理或限制均另标实验版本，不修改原始基准评分。

成员指令同步补强创建/更新区别、异常聚合和未完成报告，但提示词不承担权限、输入来源和最终验收的强制保证。

### 3.1 MCP 补充审计与版本记录

2026-09-08，MCP 审计 v1。状态为已完成只读代码审计与依赖核对、待实施。本次只补充本节方案，没有修改 MCP 源码、安装依赖或执行外部写入；不属于当前交付核验批次已完成的内容。

| 版本 | 保留内容与本次变化 | 完成边界 |
|---|---|---|
| v0，2026-09-08 初版 | 本节原有四段原文保留，记录受管边界、CLI 直连绕过、只读能力和取消证据要求 | 原始业务样本及评分不改写 |
| v1，2026-09-08 MCP 只读审计 | 追加参数校验、CLI 发布定义绑定、任务专属入口、旧领取令牌失效判据与依赖选择 | 仅方案和待实施清单，不代表参数校验、发布身份或执行隔离已实现 |

### 3.2 当前路径与可复用部分

| 路径 | 已有保证 | 缺口与修改位置 |
|---|---|---|
| Loom 标准冻结成员 → `runtimeFrozenMCPDispatcher` → `HTTPHost` | `FrozenMCPBinding.Tools` 保存名称、说明、`InputSchema` 和只读提示；包装器检查工具身份和实时目录漂移 | `HTTPHost.Dispatch` 只检查名称过滤并转发参数；冻结包装器也没有按 schema 校验参数。修改 `internal/kernel/mcphost/http_host.go`、`internal/kernel/workflow/runtime_frozen_tools.go` 与构建参数即可补此缺口，无须先修改 Loom ToolLoop |
| CLI → `/v1/mcp-boundary/{workspace}/{agent}/{idx}` → ToolBroker → HTTPHost | 已经过现有解析、写入门和审计；不必新增一套通用工具框架 | `internal/app/api/mcp_boundary.go` 仍调用实时 `Registry.Get`；`internal/kernel/mcpregistry/resolver.go` 读取目录后只保留名称，丢弃 schema。先保留完整工具定义接入共同校验，再完成下面的发布绑定 |
| 远程 Loom → `/v1/runtime/tasks/{id}/mcp/{idx}` | `internal/app/api/runtime_mcp.go` 从服务端任务 payload 读 agent 记录，避免运行时重读 agent 记录改变服务器列表 | 目前仅允许 Loom；随后构建 dispatcher 仍会走当前目录解析。任务中的记录快照不等于完整 MCP 发布契约冻结，该入口扩展到 CLI 时必须同时使用冻结定义 |

`internal/kernel/compiler/standard_frozen_v2.go` 目前将包含完整 MCP 定义的标准 v2 输入限定为 Loom；CLI 所走的标准 v1 不携带这套 MCP 发布依赖。`runtime_loader.go` 生成 CLI 记录时仅保留服务器、URL、filter 和 write tools，`EngineExecRequest` 也没有完整冻结 MCP binding。因此仅在 HTTPHost 加校验，或者在派发时复制当前目录，都不能记为 CLI 已按发布版本契约执行。

### 3.3 最小改动分两步落地

第一步补共同参数校验。在 `mcphost` 增加一个使用成熟库的小型适配层，构建 dispatcher 时编译所绑定的工具定义，最终转发前校验调用参数。冻结路径直接传入 `FrozenToolDefinition.InputSchema`；后续实时 `tools/list` 只用于检测漂移，不能替换冻结校验依据。CLI 当前受管边界可先按已解析目录校验，但其验收名称只能是当前目录参数校验，不能写成发布身份闭环。

校验必须位于最终上游调用之前，覆盖 hook 改写后的实际参数。有效参数继续转发原始 `json.RawMessage`，不补默认值、不强制类型转换、不经 `float64` 或 JCS 重新序列化参数。无效 JSON、非对象参数、参数不匹配、工具不属于绑定或 schema 编译失败时停止转发；错误只带有限的代码和参数位置，保留原调用 `CallID`，不回传原始参数。工具缓存按 dispatcher 绑定隔离，或者使用含服务器、发布绑定摘要、工具名及校验策略的完整键，不能只按工具名全局缓存。

第二步补 CLI 发布定义与任务专属访问。复用现有 `FrozenMCPBinding`、发布依赖解析、任务 payload 和 ToolBroker，为 CLI 增加版本化的冻结工具输入，保留旧 v1 编码和既有历史记录。不要直接取消 v2 的 Loom 限制后向 CLI 宣称具备 Loom journal 或恢复能力。旧工件没有完整 MCP 冻结定义时，应明确保留为未冻结能力或拒绝需要发布冻结保证的执行，不能临时读取最新目录冒充原发布版本。

从发布工件将完整 binding 保留到服务端 `EngineExecRequest`，由任务实际 workspace、run snapshot、agent version 和服务器身份共同约束。复用现有任务 MCP handler 读取服务端 payload 并构建冻结 dispatcher，移除仅支持 Loom 的入口限制；`execenv` 生成 CLI 配置时改用本任务专属地址和窄令牌，不再根据可变 agent 名称及索引生成稳定入口。上游凭据保持在服务端，通过冻结访问引用解析；当前撤销政策可以拒绝调用，但不能用新的服务器或 schema 替换被冻结的契约。CLI 配置和进程环境不获得 `rtk_` 或上游访问凭据。

### 3.4 任务令牌与旧 claim 失效判据

现有 `runtimeAuthMiddleware` 只识别 `rtk_`，`claimedRuntimeTask` 只检查 runtime/worker 归属，没有检查任务运行状态、`LeaseExpiresAt` 或领取标识。因此不能把 runtime 令牌直接交给 CLI 来复用入口。应为任务 MCP 路由设置专属认证，复用 `internal/kernel/secret/secret.go` 的 HMAC 基础能力，使用独立域和无歧义的字段编码，将 workspace、task ID、runtime ID、服务器身份、冻结 binding 摘要和领取标识绑定到窄令牌；不沿用旧 agent/index 令牌作为任务授权。

每次调用在服务端回读任务，要求令牌字段与实际任务一致、任务为 `running`、worker 归属匹配且当前租约未过期。任务进入 `cancel_requested` 或终态、租约到期、worker/runtime 变化，或相同 task 被重新领取后，旧令牌必须在上游 `tools/call` 之前失效。服务端状态无法读取时停止调用，不能退回稳定 agent 边界。上述规则约束后续调用；已经在途的外部效果仍需取消事实与写后核对，不宣称令牌撤销能撤回已发生效果。

现有 `StartedAt` 是候选领取标识：`taskqueue.Store` 每次 claim 重写它，heartbeat 只更新租约与 `UpdatedAt`。它尚不是明确的 lease epoch 契约。若采用它，签名必须使用数据库回读后的同一精度表示，并证明每次重新领取都会改变该值、续租不会改变它；不能用会随 heartbeat 改变的 `UpdatedAt`，也不能仅用始终相同的 WorkerID。若时间精度或重新领取路径无法保证区别，必须补持久的 claim nonce/epoch，不能省略该事实后宣布旧令牌问题关闭。

最低失效验收必须完成这个序列：同一 runtime 领取任务得到令牌 A，A 可调用；正常续租后 A 仍可调用；旧租约到期、取消请求或完成后 A 被拒绝；任务重新进入可领取状态并被同一 runtime 领取，得到标识 B，A 仍被拒绝且 B 可调用。再覆盖跨 workspace/task/server、篡改 binding 摘要、不同 runtime、缺失领取标识和数据库不可用。所有拒绝分支上游调用与写入均为零。测试不得只更换 runtime，因为那无法证明同 runtime 再领取时旧令牌失效。

### 3.5 JSON Schema 依赖决定与明确限制

建议固定 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`，不用 `@latest`。官方实现覆盖 Draft 4、6、7、2019-09 和 2020-12，包括组合约束与引用。仓库现有 `machine.ValidateRuntimeInput` 是 parked workflow 专用固定子集，连 `description` 都会拒绝，不适合作为 MCP validator；本次不手写另一套 JSON Schema validator，也不拓宽 machine 子集。[官方版本发布](https://github.com/santhosh-tekuri/jsonschema/releases/tag/v6.0.3)、[支持范围](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/README.md)。

| 配置项 | 待实施约定与原因 | 官方依据 |
|---|---|---|
| Draft 与 format | 无 `$schema` 时显式设 `DefaultDraft(Draft2020)`；已有明确且受支持的 dialect 按其规范处理。固定内置 format 的断言策略，不能因宿主或库升级而变化；未知自定义 format 不计作已核验的业务约束 | [Compiler 配置](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/compiler.go)、[Draft 定义](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/draft.go) |
| 无外部资源加载 | 显式 `UseLoader(nil)`，仅以 `AddResource` 注册冻结 schema 及明确随工件冻结的引用资源。允许已内嵌规范元 schema 和内部引用；未冻结的 HTTP/file 引用与未知外部 `$schema` 编译失败后直接拒绝。默认包含 `FileLoader`，不能依赖默认设置 | [加载器](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/loader.go)、[默认加载配置](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/roots.go) |
| 数值精度与原始参数 | schema 与参数都使用 `json.Number` 路径；库解析器使用 `UseNumber`，数值实现使用 `big.Rat`。校验成功仍转发原始参数，避免重新序列化引入精度或词法变化 | [JSON 解析](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/loader.go)、[数值比较](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/util.go) |
| 失败与资源边界 | 使用返回错误的 `Compile`，不用面对外部 schema 的 `MustCompile`；保留实际编译错误的有限分类，不删掉失败约束后重试。沿用或明确设置调用输入、schema 大小和复杂度上限，验证不可用时不转发 | [Compiler 接口](https://raw.githubusercontent.com/santhosh-tekuri/jsonschema/v6.0.3/compiler.go) |

该库的 `UnmarshalJSON` 使用标准 JSON decoder，不能把 `UseNumber` 误记为重复键检查。重复键与尾随值的拒绝属于 JSON 解析边界；应复用或提取已有严格解析能力并保留数值，不通过拓宽 machine schema 子集解决。不要将 JCS 转换后的参数作为实际调用参数。

### 3.6 明确待实施清单

- [ ] 引入固定版本依赖与共同 MCP schema 适配层；明确 Draft、format、解析和资源限制。当前仅完成选型，没有安装或接入。
- [ ] HTTPHost 最终调用接入校验；冻结 Loom 使用原冻结定义，CLI 当前目录路径保留完整定义；目录漂移、未知工具和同名工具缓存隔离均停止错误转发。
- [ ] CLI 发布输入和运行 payload 保留完整 MCP 冻结 binding；旧工件兼容行为明确，不将派发时目录快照当发布契约。
- [ ] 复用任务专属 MCP handler 与现有 ToolBroker，配置窄令牌；补运行状态、租约和领取标识检查，完成同 runtime 再领取的 A/B 令牌失效序列。
- [ ] 确定性参数负例覆盖错误类型、必填缺失、多余字段、`enum`/`anyOf`、非法 JSON、重复键、非对象根值；错误均无上游 `tools/call` 或业务写入。合法参数覆盖 `9007199254740993`、`1e3`、内部引用及无自动填充/转换。
- [ ] 外部引用不触发网络或文件读取；冻结 S1 后目录变为更宽松 S2 仍不放行；不同服务器同名工具不串用 schema；CLI 入队后修改 agent、目录、服务器顺序不改变原任务 binding。
- [ ] Loom 与 CLI 都通过实际 API 转发路径验证，不只测 HTTPHost；核对 CLI 配置和环境无 runtime 总令牌及上游凭据。
- [ ] 独立处理已观察到的 Bash/Python/curl 直连绕过与只读执行能力；受管参数校验通过不能据此记为权限隔离完成。完成隔离条件后再运行全新业务样本，保留原 benchmark 和额外写入检查两套结果。

### 3.7 MCP 实施 v2 与验证边界

代码 `acfa5233a4d3fdc427a035778424e8de1fcda1ce` 已实现共同参数校验、CLI 标准 v3 发布输入、完整 binding 入队、任务专属凭证、持久 claim epoch 与最终调用前的动态检查。迁移新增 `claim_epoch`，原因是领取时间戳不能保证唯一；真实数据库验证了时间戳完全相同的两次领取仍使旧令牌失效。Loom 的冻结目录检查提取到 `mcphost` 共用，不另建队列或工具调度框架。

3.1 至 3.6 保留审计 v1 的原始判断和待实施基线。本轮具体检查、代码入口与剩余边界见 [MCP 机制验收](../验收/2026-09-07-Weave任务信任双场景验证/tool-contracts/README.md)。参数、发布、任务授权机制已通过测试；任意 CLI 直连隔离、业务写后核对、Loom 预算接续和完整业务复验仍不在已完成范围内。

## 4. Loom 的具体修改边界

Loom 已有图级 `StopBudget`、预算检查点和恢复机制；不重复建立另一套图调度或业务任务系统。需要补齐的是 ToolLoop 内部停止原因与上层之间的断层。

1. **结构化退出原因。** 工具轮次耗尽、重复调用被截断、模型输出长度截断等，要以可识别结果传出；正常无工具回答也只代表本轮结束。展示总结可以保留，不能抹掉原退出原因。禁止通过匹配模型总结文字判断预算耗尽。
2. **区分执行片段和总预算。** 将固定 20 轮改为发布版本中的明确配置；一个片段用完后保存状态。只有仍有总预算、存在可核验进展且无未核对副作用时，Weave 才继续调度。总预算耗尽或无进展应明确停下；不能每次恢复清零消耗。
3. **接续保存同一逻辑身份。** 复用现有成员 journal、operation receipt、checkpoint 与 attempt fencing。持久化片段计数、累计消耗和停止原因，恢复时不重做已确认效果。当前 v0.8.1 ToolLoop 内局部轮次及重放关系需要联合验证，不能直接改成从剩余轮次开始而跳过已有回执重建。
4. **由 Weave 映射产品状态。** Weave 区分可继续、待核对、预算已尽和执行失败，再调用独立交付核验。Loom 不判断 Gmail 业务正确性、图纸是否齐全或用户是否接受。

实现时还有两个容易误修的位置。v0.8.1 图级错误分支在 `graph.go:331` 未合并步骤返回的 `update`，所以不能简单将 ToolLoop 改成返回部分 state 加 error；受控停止必须先持久化有效现场。现有 `ErrBudgetExhausted` 虽已声明，图级预算出口没有包装该 sentinel，也需接通结构化识别。已被错误存成成功结果的历史成员不能靠加大轮数自动复活，应保留原记录，明确区分新修订/新运行与原成员恢复。

Weave 编译器入口是 `internal/kernel/compiler/compiler.go:264`；当前 frozen graph 消费结果的入口是 `internal/base/teamrun/workflow_interpreter.go:1328`。CLI 适配也应保留本机引擎提供的具体停止原因和 session 身份；无法观察到的内部工具检查点要标明能力边界，不能包装成与 Loom 相同的恢复保证。

### 4.1 团队成员预算接入

本轮实现采用发布记录中的可选 `tool_loop_control`，包含 `slice_rounds` 和 `initial_total_rounds`。缺省保持旧行为；启用后只能发布为标准 Loom 串行叶子成员。配置进入冻结工件和角色证明，预算变化要求重新发布。嵌套成员、权限交互、CLI、并行分支及无监督会话的触发方式在既有发布检查中拒绝这一配置，不靠运行到工具步骤后再判断整个拓扑是否可用。

Weave 固定依赖 Loom `d704baf1ae7fee089c4b086db9a271a7b004e92b`，模块版本为 `v0.8.2-0.20260908071214-d704baf1ae7f`。Loom 返回结构化片段耗尽、累计耗尽、重复调用和提供方截断状态；Weave 将有效且已保存的暂停映射为原团队任务等待，保留 active member，不提交成员成功缓存或节点完成输出。正常模型回答仍需经过共用交付核验。

接续沿用阶段重试事务与队列。请求的绝对累计上限、原暂停检查点、片段及 token 绑定为一次授权；授权记录、父任务接续和入队原子提交。成员启动新片段时再次比较检查点内容，并将授权消费回执与新 journal 入口在同一事务提交。已消费授权在进程重入时只恢复原片段，不再增加片段或额度；确认的模型/工具回执重放保留原费用身份。

自动接续采用保守条件。只有片段上限到达、累计额度仍剩余、本片段所有工具操作都有成功回执，且出现相对片段入口新增或改变的非空文件导出时，才在同一父任务事务中接续。普通工具文字和模型自报进展不满足这一条件。该条件只确认可观察的文件进展，不表示业务正确或交付验收通过。总额度耗尽、重复调用、提供方异常和不确定工具效果均不自动补额。

Workbench 沿用原阶段继续入口，显示已用轮次和累计上限；总额度耗尽后由用户填写新的绝对上限。网络响应丢失会重用已持久化的请求及额度。公开活动投影不包含私有暂停 token。此处只适配团队执行的权威状态，没有调整 Workbench 自身 Agent 循环。

机制验证覆盖发布→执行→预算暂停→原成员接续→文件交付，以及工具回执后的中断、已消费授权后的再次中断、保存失败的授权回滚、无授权和重复授权不增额、费用去重、请求同键变更额度拒绝。新业务复验与 Loom 增量价值判定仍未完成，历史业务失败和评分不改写。

## 实施顺序与停止条件

2026-09-08 用户确认先完善 weave-next 的团队能力。当前范围为团队派发、执行与恢复、受管工具和交付核验。Workbench 自身的 Agent 循环、预设、子 Agent 与独立工作流能力留待后续单独处理；本轮仅做团队权威结果所必需的展示适配。保留现有输入绑定修复，Loom 只修团队执行已经证明需要的缺口，不开展前台 harness 替换。用户同时要求优先利用 Weave 与 Loom 现有原语：沿用工作流、Snapshot、TeamRun、attempt、operation receipt、既有交付事务，以及 Loom 的 Step/delta、yield、mid_step 和 Store；新增内容须说明现有原语缺少的是哪项事实或状态，不另建通用任务队列、调度器或业务执行框架。

第一批修输入绑定和交付判定，建立两种引擎共用的防线。第二批修受管工具权限、写入核对和取消证据。第三批补 Loom ToolLoop 的退出语义与持久预算，并贯通 Weave 接续；同时规范 CLI 停止结果映射。每批先通过针对已发生故障的回归，再运行全新隔离业务样本。

这三批修复只能消除已经定位的机制缺口，不能预先保证专业成果质量或模型工具使用不再出错。最终仍需通过原电商 verifier、额外写入检查、日冕完整文件及专业核验，以及真实中断接续。未通过前，冻结扩大 Loom 使用范围。两种路径的模型、工具、输入和交付要求对齐之前，不做 A/B/C 排名，不把这些共用修复的收益归给 Loom。

日冕已经重新核实的历史恢复证据为同一成员 attempt 1→2、17 条确认操作保持一致、7 条工具回执各对应一次实际调用；该运行后来失败，总预算连续性与完整交付仍未证明。参见 `docs/验收/2026-09-07-Weave任务信任双场景验证/corona/recovery-verification.json`。

## 第一批确定性验收

- 相同监督会话先处理 VBR-52，再提交 INV-440；故意派旧正文、旧修订并换新幂等键，入队前拒绝且写入为零。
- 两条引擎路径都正常结束但缺少一项必需成果，保持执行结束、交付未完成，不丢已有成果。
- 固定响应需要三轮而局部预算为两轮时，准确记录耗尽与部分状态；中断恢复不重复工具效果、不重复累计费用、不凭重入获得新总额度。
- 记录明确追加额度后继续第三轮；只有全部交付检查通过才进入核验通过。
- 重复工具批次、provider 截断、错误参数、只读成员写入分别得到真实结果；额外事件存在或必需项不可核实时不总体 PASS。

上述是机制回归，不替代完整业务复验。审计分别覆盖了派发来源、Loom 预算恢复、CLI 与共用交付工具边界；实际实施范围与剩余工作见下节。

## 输入绑定实现进展与剩余边界

Workbench 的初次派发改由 Host 注册器提供原始消息快照，模型仅选已商定的团队、工作流和项目；明确重跑从已保存的用户修改正文取输入。服务端在同一事务锁下固定输入修订、已发布工作流和派发幂等键，并与入队原子关联。界面按钮生成的文字记为控制来源，不混入手输材料。针对网络不确定结果，注册可重复；新输入出现时原子核对旧请求，已消费则回原运行，未消费则关闭并阻止迟到入队。具体调用与限制见 [Workbench 包说明](../../workbench/packages/bundle/workbench-app/README.zh.md)。输入绑定已完成本地提交和机制验证，验收环境的 API、Workbench 页面、运行节点与产品插件已读取核对，未在这一核对中派发业务任务。

这部分不等于任务归属已完整解决。当前初次派发保留上次接受派发以来尚未消费的用户消息；旧请求关闭后也不能仅因新消息出现而丢弃旧材料，因为新消息可能是补充或纠正。独立新任务、当前任务追问与修改，仍需要产品明确的任务目标身份贯穿输入、授权与执行。不得靠中文关键词推断、自动清空原话或只取最后一句确认来伪装解决。这次输入绑定提交未增加交付验收、工具权限或 Loom 预算机制，三阶段业务目标保持未完成。会话分叉只继承已接受输入的消费水位，使用自己的修订头；继承的未决请求不会重放，需先在原会话核对，再从核对后的历史分叉。认证或传输错误也不会将结果未知的请求改成可重新派发的新身份。
