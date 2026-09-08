# Workbench 桌面接入方案可落地性与简洁性评审

日期：2026-09-08  
审查对象：产品技术方案及实施计划 v0.1  
结论版本：v0.2  
方法：三个独立审查视角，主审逐项回查源码并裁决。未启动生产操作、未执行故障注入，不能据此报告运行 PASS。

## 1. 总体判断

v0.1 是一份范围较完整的云端多用户 Workbench 蓝图，但还不是可直接按模块开工的首期方案。它的主要问题不在于选错了一个桌面框架，而在于把未经确认的体验要求与一些未来规模化机制绑定起来，同时对真正困难的授权、持久接收和材料接缝估计不足。

从可落地性看，账号、Host、Session、MCP、远程执行都已有基础，不需要重写整个系统。不过现有基础之间并未构成方案承诺的完整合同。只有登录页和容器可启动，远不足以证明方案成立。

从简洁性看，v0.1 保留业务运行与客户端生命周期分离是正确的；重复对话元数据、动态 Host 控制面和多种授权对象则过早。v0.2 应当减少新的真相源和需要独立恢复的对象，保留那些不能用删除需求掩盖的正确性约束。

主审结论为：**有条件保留远程路线；首期改为静态专属 Host 的完整纵向试点。优先冻结授权、接收回执与材料契约，撤回未经小样验证的整体工期承诺。** 本地 Host 不是错误路线，但也尚无可安装证据证明更省事。

## 2. 判断“优雅”的具体标准

本次不打主观分数，使用以下可检查标准：

| 标准 | 好的状态 | v0.1 的主要问题 |
|---|---|---|
| 每项复杂度都有对应需求 | 机制直接服务已经选择的用户旅程 | 跨设备对话成为默认前提，继而带入云端控制面 |
| 同一事实只有一个权威 | 用户、权限、Session、运行各自归属明确 | 增加对话 revision/映射表但未定义与日志的一致性 |
| 进程和权限生命周期可解释 | 关窗、退出、命令接受、运行继续分别有边界 | accepted turn 与独立观察委托的关系模糊 |
| 增量可闭合 | 少量用户就能跑一个完整且真实的链路 | 横向先铺账号/Host 平台，材料接缝后置 |
| 故障规则落到实际写入处 | 能证明旧进程失去写入权、回执已持久 | 数据库 lease 被当成文件存储 fencing |
| 下一阶段由证据触发 | 达到容量/跨设备需求后才扩展 | 动态分配、双框架全面对照成为预置工作 |

简洁性不等于文件数少或删除验证。若把账号身份检查、材料冻结或持久接收去掉，方案只是少做了必要工作，并没有更优雅。

## 3. 源码核实与逐项裁决

### R01. 原始需求不足以证明云端多用户 Host 是必选

原需求是安装、登录、默认服务与显式改址。v0.1 第 1、4 节加入完整对话跨设备可见并作为首期标准，没有将其标为新增产品选择。

Workbench dispose 当前清理计时器和观察请求，没有在此路径发送取消业务运行。证据：[workbench-app/src/index.ts:1469](../../workbench/packages/bundle/workbench-app/src/index.ts#L1469)。这支持“关闭 Host 本身不等于取消远程 run”的判断，但实际运行续行仍需实测。

**裁决：采纳，收窄远程路线的论证。** 业务运行独立于客户端是两种 Host 部署路线共同需要的合同。真正决定 Host 地点的是完整对话的位置、前台监督可用性、本机材料与发行成本。当前保留远程静态试点建议，依据是既有部署接近它，而非假定用户已经要求完整跨设备体验。

### R02. 用户角色和成员角色并非统一授权基础

JWT 解析当前用户并以 user.Role 设置角色；用户更新只更新 weave_users；成员移除只删除 weave_members。API key 验证还会使用 key 的固定角色，未在该函数内读取当前 owner 状态和成员资格。

主审回查：[middleware.go:69](../../internal/app/api/middleware.go#L69)、[users/store.go:150](../../internal/app/users/store.go#L150)、[org/store.go:182](../../internal/kernel/org/store.go#L182)、[apikeys/store.go:83](../../internal/app/apikeys/store.go#L83)。部分人工任务有成员检查，不代表所有业务入口一致。

**裁决：采纳，列为首个开工前合同。** 统一账号状态、成员资格与资源动作政策，让新入口和旧 JWT/个人 API key 入口都遵循它。不通过新网关隐藏旧 API 的差异；不机械合并角色权限。

反例验收：同一用户从 UI、MCP 和直接 API 进行同一操作，移除成员或降权后得到一致结果。当前是代码层发现，未执行生产越权尝试。

### R03. CredentialProvider 不能替代具体操作的来源

当前 RPC handler 只接收 endpoint、payload、signal；MCP tools/call 只发送 name 与 arguments。增加一个动态 user token provider，仍不能确定同账号两个设备中哪一个授权了当前队列操作。

主审回查：[rpc-host.ts:240](../../workbench/packages/client/connection/src/rpc-host.ts#L240)、[mcp-client/tools.ts:80](../../workbench/packages/mcp/mcp-client/src/tools.ts#L80)。Go 配置还限制 wv_sk_ 前缀：[weaveclient/config.go:43](../../internal/app/weaveclient/config.go#L43)。因此不能只将 JWT 写入原环境变量。

**裁决：采纳核心，收窄建议机制。** 保留一个可信 OperationContext，绑定来源会话、用户、组织、会话/操作 ID。先不建设泛化 TurnGrant。每次新业务写入核验原会话；模型参数不能提供身份；设备 B 不能替设备 A 续权。

需要记录来源身份，不代表需要再发行一种长期凭据。内部传输和队列元数据的具体接法必须由小样证明。

### R04. 已收到输入和已持久接受不是同一事实

当前 prompt 将 requestId 写入消息来源，调用 followup/steer 后返回 accepted。类型注释将其定义为 inbox 回执。持久层后台批写存在单独生命周期，当前入口未等待该消息完成持久接收。

主审回查：[commands.ts:303](../../workbench/packages/api/session-controller/src/commands.ts#L303)、[types.ts:344](../../workbench/packages/api/session-controller/src/types.ts#L344)、[coordinator.ts:31](../../workbench/packages/session/session-persistence/src/coordinator.ts#L31)。requestId 被记录不等于该入口实现持久去重。

**裁决：采纳，提升至第一条链路。** 新输入接收事件应在进入执行前持久保存，并能按稳定 ID 返回同一回执。优先复用 Session 事件/持久层，避免新建一个双写任务库。模型计算可能重试，业务副作用则必须使用原操作身份对账。

反例验收：在接收响应前后杀 Host、丢失响应并重试，输入不会被悄悄遗失；同 ID 不同内容冲突。此处要求的是新增合同，不把现有 inbox ACK 报为 bug。

### R05. 现有“观察器”会写命令，而且观察对象不止 run

当前 poll 会提交 stop、rerun、retry、人审与纠偏，然后再读取状态。run 产生前还会读取 build_id 和 client_request_id。浏览器收到 204 时只是 Host 的操作日志已经保存。

主审回查：[workbench-app:1113](../../workbench/packages/bundle/workbench-app/src/index.ts#L1113)、[1143](../../workbench/packages/bundle/workbench-app/src/index.ts#L1143)、[1159](../../workbench/packages/bundle/workbench-app/src/index.ts#L1159)、[1242](../../workbench/packages/bundle/workbench-app/src/index.ts#L1242)。

**裁决：采纳职责拆分，暂缓长期观察授权。** 分成提交命令、请求对账、状态读取三个内部职责，保留同一个 WorkTask。首期没有有效个人观察授权就暂停读取，重新进入后对账；已接受的运行继续。只有明确要求无人登录持续监督时，再设计 build/request/run 范围的只读授权。

这比同时引入 AuthSession、TurnGrant、HostGrant、ObserverGrant 更容易解释与撤销。

### R06. 材料尚未进入唯一团队派发合同

附件 API 已保存摘要、大小及 created_by，但读取以 tenant 为主要范围。团队派发请求、Go client 和幂等指纹没有结构化材料集合。

主审回查：[attachments.go:67](../../internal/app/api/attachments.go#L67)、[80](../../internal/app/api/attachments.go#L80)、[team_dispatch.go:24](../../internal/app/api/team_dispatch.go#L24)、[94](../../internal/app/api/team_dispatch.go#L94)、[weaveclient/client.go:201](../../internal/app/weaveclient/client.go#L201)。

**裁决：采纳，将材料移到最早纵向样本。** 复用现有附件目录，明确个人草稿/组织共享政策，扩展 MCP → client → dispatch → fingerprint → snapshot → Runtime 的引用与摘要。不另建材料平台，不拿任务文本里的路径充当材料合同。

反例验收：两台机器读取到同一摘要；换材料后复用旧请求 ID 冲突。created_by 不是 ACL，现有组织共享政策也不能直接被判成个人数据泄漏。

### R07. 数据库租约不能防止 JSONL 双写

当前 JSONL 追加由文件系统执行，发生失败时按之前长度回滚；写入没有 Host epoch 校验。若仅因控制面租约超时启动另一写者，旧 Host 恢复后仍可能追加或截断文件。

源码依据：[JSONL appendLines:670](../../workbench/packages/session/session-persistence-jsonl/src/index.ts#L670)。此处发现的是 v0.1 拟议自动接管与现有写入机制不匹配，没有声称当前静态部署已经发生损坏。

**裁决：采纳，首期删除通用 Host 调度和自动 lease 接管。** 固定清单、独立卷、单写者、串行替换即可。只有存储独占或实际写入 fencing 通过暂停旧进程/恢复旧进程的故障测试，才进入自动接管。

### R08. 冷启动监督与重新打开后同步应分开

workbench-app 启动遍历 ctx.sessions.list()，该 list 只返回已加载的 live Session；持久历史列表不会自动激活全部任务轮询。

证据：[workbench-app:1310](../../workbench/packages/bundle/workbench-app/src/index.ts#L1310)、[SessionStore:1048](../../workbench/packages/core/session/src/index.ts#L1048)。

**裁决：采纳事实；首期收窄承诺。** 默认要求重新打开对话后对账。无人打开时恢复监督需要持久发现索引或日志扫描，并仅恢复必要 Session，不自动继续 LLM。它只有在产品选择该体验后成为硬门槛。

### R09. ConversationBinding 容易成为第二份对话真相源

SessionHeader 已有 ID、时间和父会话；目录归属由 WorkspaceRegistry 管理，创建流程也明确处理 Session 建立但目录关联失败的情况。v0.1 新表中的 revision、project_id、conversation_id 与这些对象没有单一权威关系。

证据：[SessionHeader](../../workbench/packages/core/session/src/types.ts#L61)、[会话创建](../../workbench/packages/api/session-controller/src/commands.ts#L72)、[历史目录归属](../../workbench/packages/workspace/workspace/src/index.ts#L421)。

**裁决：采纳，首期取消第二份权威对话表。** 固定 Host/卷证明用户归属，conversation_id = SessionId。项目仍使用明确的 Weave project_id；目录分组保持 Host 职责。未来统一发现索引只能由权威日志重建，不保存另一套标题/运行状态/revision。

### R10. Generation 不解决服务器副作用和实例切换竞态

丢弃旧页面响应不能证明请求未在旧服务器执行；先检查 instance 后载入页面也存在检查到实际写入之间的窗口。

**裁决：采纳，补全同一连接合同。** origin/expected_instance/generation 作为不可变快照；业务入口检查预期实例，旧写操作仍挂在原实例下对账。续期、上传与重连也要遵守 generation，不能只保护列表请求。

这项调整只延伸已有连接和操作身份，不创建新任务状态机。

## 4. 为什么不立刻改成本地 Host

原方案远程路线论证过度，并不能直接证明本地路线更好。本地 Host 的软件依赖、默认模型能力、凭据、休眠和目标平台更新都要随安装包解决；远端 Runtime 仍不能直接读取本机文件。当前 Dockerfile.workbench 还包含 Node、Weave 二进制及运行工具，说明桌面本地 Host 的发行工作不能忽略。

用户是否需要完整跨设备对话是决定性输入。需求未定时，本次不把“评审发现云端过重”变成“必须本地”的新结论。优先静态云端试点是小步验证现有部署的建议；本地小样在材料与使用习惯更适合它时进入比较。

## 5. 从 v0.1 到 v0.2 的实际变化

| 处理 | 内容 |
|---|---|
| 保留 | 默认服务、用户显式切换、个人身份、Web UI 与监督能力复用、远程执行、材料和成果闭环 |
| 前置 | 当前成员权限政策、操作来源、输入持久接收、材料进入派发快照 |
| 删除首期 | 动态 Host 管理器、自动租约接管、第二份对话权威表、泛化长期 Turn/Host/Observer 授权 |
| 改成条件项 | 跨设备完整对话、用户离线持续监督、Host 无人打开主动恢复、本机 Runtime |
| 收窄 | 每用户 Host 是固定部署隔离方式；设备管理保留机制但不先做完整产品页；桌面先验证一种框架 |
| 撤回承诺 | 未经小样的 35–60 人日作为首期基线，以及预先设定但未冻结环境的性能数字 |

“优雅程度提高”在本版具体体现为少了新的权威数据库、少了需要独立续期/撤销的授权对象、少了一个动态进程调度平台，但保留了正确性与真实交付出口。由于尚无小样，只能判断结构更简单、边界更清楚，不能宣称成本和性能已经实证优于 v0.1。

## 6. 建议下一步与可否开工

可以开始 S0 的合同冻结及 S1 的限定纵向小样准备。尚不建议直接建设全部多人云端平台。

小样必须回答四个问题：

1. 当前账号被移除或降权，三个调用入口是否同时生效。
2. 用户收到持久接收回执后杀 Host，输入是否仍能找回并避免重复业务副作用。
3. 相同材料在异机 Runtime 是否可读且摘要与派发快照一致。
4. 固定专属 Host 能否在目标设备和服务器预算下形成可安装、可更新、可恢复的产品链路。

通过后再冻结剩余范围与工期；失败则修实际接缝。无人登录持续监督、动态容器分配和集中索引不得成为掩盖这四个问题的外围工程。

## 7. 评审原文、版本与本次验证

- [产品与简洁性原始审查](Workbench桌面接入评审-2026-09-08/产品与简洁性原始审查.md)
- [工程可落地性原始审查](Workbench桌面接入评审-2026-09-08/工程可落地性原始审查.md)
- [契约与生命周期原始审查](Workbench桌面接入评审-2026-09-08/契约与生命周期原始审查.md)
- [产品技术方案 v0.1 原文](2026-09-08-Workbench桌面接入与账号体系产品技术方案.v0.1.md)
- [实施计划 v0.1 原文](../计划/2026-09-08-Workbench桌面接入与账号体系实施验收计划.v0.1.md)
- [产品技术方案 v0.2](2026-09-08-Workbench桌面接入与账号体系产品技术方案.v0.2.md)
- [实施计划 v0.2](../计划/2026-09-08-Workbench桌面接入与账号体系实施验收计划.v0.2.md)

原始审查保留各审查者的建议，本文逐项裁决与 v0.2 才是合并结论。v0.1 为字节一致归档，其内部链接保留当时写法；阅读历史配套计划请使用本节的 v0.1 链接。

本次源码基线为 da0e5c7eb5ba0f85c6381bd395418dba77f8aaf9。只修改方案、计划及审查文档，不修改运行代码、不改生产配置。文档检查不替代数据库集成、真实安装或故障验收。

v0.2 收口复核结果：契约审查确认来源会话、三种回执、同步职责、连接竞态与对话真相源已采纳或明确收窄；产品审查确认跨设备完整对话没有被当作用户批准，动态接管和长期观察机制没有重新进入首期。两项定向复核均未发现新的必须修正项。这是文档一致性结果，不是运行验收。

本次已检查 8 份当前/归档/原始审查文档的文件引用、源码行号范围与代码围栏，检查通过。没有重新运行与本次文档修改无关的业务测试。

## 8. 后续 v0.3 边界修订

在讨论是否应将 Weave 拆成更纯粹的框架后，[产品技术方案](2026-09-08-Workbench桌面接入与账号体系产品技术方案.v0.3.md)与[实施计划](../计划/2026-09-08-Workbench桌面接入与账号体系实施验收计划.v0.3.md)升为 v0.3。新增“单仓维护、职责收紧、按需独立部署”的约束，明确监督 Agent 与团队执行系统的分工，增加本次依赖审查和命令/投影边界验收。镜像分离、分仓、公开 SDK 和全量历史依赖清理均未设为桌面首期前置条件。

v0.2 原文已归档，本节以上独立审查结论仍对应 v0.2；v0.3 只做本轮作者修订与文档一致性检查，不将此前互审冒称为对新增内容的独立复核。
