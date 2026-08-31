# Agent Note: Weave Workbench 最小任务架构

Status: proposed

[English](2026-08-30-weave-workbench-durable-work-task-domain.md) | 中文

## 问题

Weave Workbench 必须让 FDE 看得懂并能控制持久团队执行。用户应当能够选择或确认团队，在 Weave 继续工作的同时离开对话，回来查看谁在做什么、运行在哪个运行时上，处理 HumanTask，检查交付物，停止方向错误的 run，修订简报并启动一次纠正后的尝试。

上一版方案满足了产品目标，但在第一条真实工作流证明需要之前引入了过多架构。它同时包含独立 WorkTask 数据库、WorkStream、attempt、命令记录、多套本地状态机、多 authority 恢复、可靠 Remote 流、安全点暂停、栅栏、因果影响计划和副作用账本。每项机制单独看都能解释，叠加起来却形成了围绕 Weave 的第二套编排平台。

## 提案

用最小的所有权模型完成完整用户闭环：

- Weave 始终是唯一执行权威。
- Workbench 从现有 DSH Session 投影出一个持久工作视图。
- Browser 永不拥有远程执行状态。
- 第一期只在网络调用前持久化 run 关联和一个尚未解决的本地动作。
- 面向用户的控制是停止整个 run，然后确认并新建 run。
- 多团队聚合和无损原地干预保留为证据驱动的扩展，不是预先承诺的基础设施。

产品概念仍叫 `WorkTask`，但第一期不创建独立 WorkTask 存储域。一个持久监督 Session 投影出一个工作项，并保留按顺序排列的 Weave run 尝试。关闭对话、销毁前台 Agent 或关闭应用都不会取消 Weave 执行。

## 最小化对抗互审

一名怀疑派架构审查者和主审查者共同攻击了重复状态、过早领域对象、过量接口、误导性干预范围与恢复行为。审查形成五条约束性结论：

1. Weave run 状态只持久化在 Weave。Workbench 只保存新鲜度和至多一个未解决的 `pendingAction`；`正在停止`、`等待修订`等文案都从事实推导。
2. 第一期保留当前由 Session 支撑的 WorkTask 投影。不增加 registry、独立命令数据库、单独 controller 包或多 authority 分区。
3. 增加一个精确 run 活动读取、一个幂等精确 run 停止，并为现有交付物列表增加 `run_id` 过滤。停止响应丢失时以相同请求和 key 重放。
4. 修订完整执行简报并创建一个新 run。不暗示可以在旧 run 继续执行时对单个成员进行手术式改向。
5. 第一期只面向没有不可逆外部写入的工作流。在真实工作流证明需要前，不建设通用重放策略或副作用账本。

本次互审取代本文早期草案中过重的第一期机制。它没有宣称高级干预不可能，只要求在这类架构进入实际开发前先有产品证据。

## 产品约定

### 完整核心旅程

当产品无需暴露内部评测或原始 MCP JSON 就能完成以下旅程时，它对第一位真实用户是完整的：

1. 用户选择团队，或描述结果并确认有事实支持的推荐。
2. Workbench 在 Weave 开始工作前记录简报和派发身份。
3. Weave 独立于对话和应用窗口持续执行。
4. 用户回到一个工作视图，看到团队、阶段、成员、运行时、HumanTask 和交付物。
5. 用户可以停止精确的当前 run，并在 Weave 报告 `cancelled` 或 `abandoned` 前看到真实进度。
6. Workbench 展示观察到的已完成、中断、未开始和未知事实。
7. 用户确认完整修订简报；Workbench 启动新 run 并保留旧尝试。
8. 用户读取并验收纠正后 run 自己的交付物。

### 诚实边界

第一期不承诺同 run 恢复、检查点重启、仅成员暂停、因果制品失效判断、回滚、补偿、跨设备本地状态恢复或多人审批。它不展示私有思维链。它会区分数据缺失与无活动，也会区分数据陈旧与工作已停止。

没有匹配团队时诚实讨论缺口。没有已发布默认工作流时明确报错。自由协作始终是显式模式，不会静默退化。

## 双所有者架构

```text
User / Codex / Workbench UI
            |
            v
DSH Session work projection
  brief, run history, freshness, pendingAction
            |
            v
Workbench Host poller and retry
            |
            v
Weave
  teams, workflows, TeamRuns, runtimes,
  HumanTasks, deliverables, cancellation
```

### Weave 所有权

Weave 拥有团队与工作流身份、精确 TeamRun 身份与状态、阶段、成员活动、运行时分配、HumanTask、交付物来源、取消转换、授权和审计。执行状态只能由 Weave 改变。

### Workbench 与 DSH 所有权

Workbench 拥有用户简报、所选团队快照、监督 Session、按顺序排列的尝试链接、本地新鲜度、一个未解决动作、修订草稿和展示偏好。DSH 提供对话持久化、Host 生命周期、MCP 访问、Browser 扩展槽、国际化和通用工具展示。

DSH `Agent.cancel()` 和本地 Job 取消可以停止前台对话或本地等待。它们永远不代表远程 TeamRun 已停止。

## 最小概念

### WorkTask 投影

第一期 WorkTask 是派生的 Session 投影，不是第二个业务数据库。它包含：

- 一个用户目标和预期输出；
- 一个已确认团队与工作流快照；
- 按顺序排列的尝试列表，每次尝试都由 `client_request_id` 和精确 run 身份关联；
- 最新权威活动快照及其新鲜度；
- 零个或一个未解决的 `pendingAction`；
- 精确 run 返回的 HumanTask 和交付物引用。

只有当一个真实结果必须跨多个团队，或必须在所有关联 Session 被删除后仍恢复时，才可以把该投影提升成独立领域。迁移必须复用精确 run 和 request 身份，不得按标题或团队名猜测。

### Attempt

Attempt 是指向一个不可变 Weave TeamRun 的本地链接。终态 attempt 永不重新变成运行中。一次修订会创建新的 `client_request_id` 和新 run。旧 attempt 及其交付物继续可见。

### Pending action

`pendingAction` 是唯一新增的本地恢复事实。它记录停止请求或纠正后派发、规范载荷摘要、幂等或 request key、精确目标和本地生命周期。它在网络发送前持久化，只有 Workbench 观察到权威结果后才清除。

它不是第二套 run 状态。UI 文案由 `pendingAction`、Weave 状态和新鲜度推导。

### WorkStream

`WorkStream` 延后。如果真实 FDE 使用证明一个结果需要多个专业团队和一个综合团队，独立 WorkTask 可以包含有界 WorkStream。Workbench 仍不调度它们的内部工作流，也不发明跨 run 依赖；每次派发都由 Weave 拥有。

## 所需 Weave 合同

第一期只需要三个公开变更：

1. **精确 run 活动：** 一个有界快照返回 TeamRun 状态、阶段、已知成员、运行时分配、HumanTask、精确 run 交付物引用、新鲜度 revision 和各部分完整度。
2. **精确 run 停止：** 一个经过认证的幂等命令按照 Weave 现有取消语义转换 queued、running 或 parked 执行。相同载荷和 key 的重复请求返回相同结果；载荷变化产生冲突。
3. **精确 run 交付物过滤：** 现有交付物列表接受 `run_id`；正文仍由现有交付物读取工具获取。

现有 `team_dispatch`、`dispatch_status`、HumanTask 工具和交付物读取工具保持不变。纠正后尝试使用普通 `team_dispatch` 和新的 `client_request_id`；第一期不增加 correction digest、成员目标、独立命令状态接口或独立停止摘要记录。

终态活动快照本身提供观察到的停止事实。Workbench 绝不从模型文本推导因果有效性。

## Workbench 融合

第一期保留现有包边界：

- `packages/bundle/workbench-app` 拥有 Host Session 投影、轮询和重试适配器。
- `packages/client/ui-weave` 拥有工作行、检查器、活动细节、停止确认、修订编辑器、run 历史、HumanTask 和交付物卡片。
- 现有 DSH Session、Remote、MCP、locale 和工具包保持不变，除非出现经过严格证明的窄扩展需求。

第一期不增加 `work-task-controller`、registry、独立命令存储、可靠事件流或通用同步框架。Browser 在挂载、重连和命令后从 Host 刷新基线。轮询必须有界，并在终态对账后停止。

Host 在调用 Weave 前把 `pendingAction` 写入持久 Session 投影。重启后扫描非终态工作投影和未解决动作。未知停止结果使用相同请求和幂等 key 重放，然后读取活动。未知纠正派发通过现有 `dispatch_status` 和相同 `client_request_id` 恢复。

## 产品界面

默认任务视图展示：

- 目标和预期输出；
- 所选团队和工作流；
- 当前阶段和简单进度；
- 成员、当前或最近业务活动及运行时分配；
- 需要人工处理的事项；
- 精确 run 交付物和 run 历史；
- 新鲜度和诚实的数据缺失标记。

诊断、原始 trace、checkpoint、传输细节、模型元数据和内部评测放在深入查看中或不展示。界面只有一个整 run 控制，即 `停止这个 run`。终态取消后提供 `修订并启动新 run`。

## 交付阶段

### Phase 1——单团队完整控制闭环

固化现有 TeamRun 取消竞态，开放三个 Weave 合同，用 run 历史和一个 `pendingAction` 扩展当前 Session 工作投影，完成 Workbench 流程，并用一条没有不可逆外部写入、持续时间足够长的真实日冕分析工作流验证。

### Phase 2——仅在有证据后支持多个专业团队

如果 FDE 反复需要多个团队服务一个结果，再把 WorkTask 提升成独立本地域，增加有界 WorkStream 和一个显式综合流。不增加通用依赖图或自动跨团队调度器。

### 证据驱动的高级控制

安全点暂停、检查点续跑、成员栅栏、因果影响计划和副作用账本不是排期中的阶段。只有当真实停止并重跑造成不可接受的工作损失，或外部写入工作流成为经验证的优先需求时，才为它们另做设计。在此之前 Workbench 不得暴露这些控制。

## 验证

最小确定性测试集证明：

1. queued、running、parked、已请求、终态、宽限超时、旧 generation 和迟到结果的取消行为；
2. 未知响应前后相同 key 的停止重放与载荷变化冲突；
3. 存在一个未解决停止或派发时的 Session 或应用重启；
4. 活动、HumanTask 和交付物的精确 run 隔离；
5. UI 文案仅由权威状态、新鲜度和 `pendingAction` 推导，不出现虚假的已停止或已暂停；
6. 一条真实日冕 run 被检查、停止、修订、重跑，并完成纠正后的交付物。

## 验收条件

- 关闭对话或应用不会停止 Weave 工作，重新打开后无需模型轮次即可恢复任务视图。
- 用户能识别所选团队、当前阶段、已知成员活动、运行时分配、人工待办和交付物。
- 停止请求指向一个精确 run，能从未知响应恢复，并在 Weave 报告终态取消前始终显示为进行中。
- 终态活动视图只报告已持久化的完成、中断、未开始、已交付和未知事实。
- 修订始终创建新 request 和 run；旧 run 及其交付物保持不可变且相互隔离。
- Workbench 除新鲜度和一个未解决本地动作外，不持久化执行状态机。
- Codex MCP 与 Workbench 使用同一个 Weave 权威并报告相同 run 事实。
- 没有通过对应证据门槛时，不实施高级干预或多团队架构。

## 考虑过的替代方案

**立即建设独立 WorkTask 领域。** 它为多团队工作预作准备，却在一个真实单团队任务通过前复制本地持久化、恢复、revision 和导航。第一期使用 Session 投影已经足够。

**等待安全暂停。** 它会让有用控制一直等待 Weave 变成更庞大的执行引擎。对于无副作用的分析工作，停止整个 run 并新建纠正 run 已经诚实且完整。

**使用本地 Agent 或 Job 取消。** 它无法停止远程 worker，会制造虚假产品状态。

**向单个成员发送实时纠偏。** 没有栅栏和因果执行身份时，Workbench 无法知道哪些工作使用了新指令。第一期为新 run 修订完整简报。

## 风险

由 Session 支撑的工作暂时不能聚合多个团队，也无法在所有关联 Session 数据被销毁后恢复。这是明确的第一期边界，不是隐藏的持久性承诺。

停止整个 run 可能丢弃未提交工作。产品保留已提交事实，并在投资无损干预前先验证更便宜的交互。

把第一期限制在没有不可逆写入的工作流会排除部分未来自动化。这个限制比过早建设通用重放治理系统更安全、更小。

当前取消实现可能暴露迟到结果竞态。这类失败会阻断 Weave 停止合同；Workbench 不得用本地状态掩盖它。
