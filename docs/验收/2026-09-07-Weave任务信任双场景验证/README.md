# Weave 任务信任双场景验证记录

2026-09-07。依据[任务信任契约与三阶段方案](../../计划/2026-09-07-Weave任务信任契约与双场景三阶段验证.md)执行。

## 当前状态

| 阶段 | 状态 | 当前证据 |
|---|---|---|
| 阶段一：契约与首轮真实性审计 | `IN_PROGRESS` | 路径修复后的冻结本地文本样本已形成真实 Workbench 执行、5/5 交付核验和精确版本“可以采用”评价；原电商历史复验与日冕业务闭环仍保留缺口 |
| 阶段二：重复使用与中断接续 | `IN_PROGRESS` | [成员级纠偏样本](held-out/correction-replay-02/RESULT.md)证明当前格式 checkpoint 仅重跑 calculator、完整保留 reviewer，并在固定预算内交付；尚未在日冕业务材料或外部副作用场景复验，也未补齐可比人工时间与 A/B/C 数据 |
| 阶段三：Loom 边界决策 | `COMPLETE` | [边界决策](loom-boundary-decision.md)已收束到 v0.4：共用信任机制进入主线，Loom 只准入已发布标准串行叶子；扩大范围仍受原阈值约束 |

2026-09-08 输入绑定修复已提交为 `ce5fe667`，[机制验证](input-binding/mechanism-verification.json)通过并完成[本地只读部署核对](input-binding/deployment-readonly-check.json)。首次派发与界面重跑由 Workbench 保存原始输入和固定服务端回执，按钮控制文字不混入材料；这批代码尚未完成活体复验。交付核验修复另已提交为 `1bad43f71692a89b1502dcc075305b32fe267dc2`，[机制检查](team-delivery/mechanism-verification.json)通过，尚未部署本批或执行业务复验。工具边界仍待实施；Loom 可选暂停在独立本地提交 `d704baf1`，尚未接入 Weave 的持久预算。完整出口逐项记录在 [completion-audit.json](completion-audit.json)，任何机制回归通过都不替代业务验收。

2026-09-10，成员级纠偏修复提交 `a197fa7952d44635e85c19bde544beb6fafe9aa1` 已部署并完成真实 Workbench 活体复验。运行在 `join` 安全点只重启数据计算员，独立复核员的物理任务、产物引用和文件 SHA-256 保持不变；计算文件只出现请求中的两处变化，最终 5/5 核验通过，并对同一交付版本记录“可以采用”。[机器结论](held-out/correction-replay-02/verification-summary.json)限定于 `external_effects=none` 的冻结本地文本工作流；负责人在协调节点越权生成角色式文件的质量问题仍保留，日冕外部材料恢复和原电商历史复验没有因此自动闭合。

## 证据规则

- 所有浏览器动作使用内置 Workbench 页面。
- 命令行只用于独立读取、归档和验证。
- 页面文案、模型自评、运行终态和成果核验分别记录。
- 找不到的历史事实标记为 `MISSING`；没有执行的检查标记为 `NOT_RUN`。
- 只有当前运行实际交付的成果可以登记为当前样本成果。

## 执行日志

### 2026-09-07 方案落档

- 已建立任务信任契约、证据等级、双场景职责、三阶段出口、Loom 对照规则和停止规则。
- 电商样本确认为 RealReplicaBench（现 CommerceAgentBench），本机 checkout `084489800bfd3f9f239503eda9e754bc267e98f5`，远端 `Accio-org/RealReplicaBench`。
- 阶段一首个任务选择 `api-gmail-vendor-brief-mcp`；原 verifier 对标签、已读、星标、未发送草稿、日历事件和禁止发送邮件进行确定性检查。
- 下一步从 Workbench 历史核对是否已有同任务运行；没有完整证据时按固定 checkout 新建独立运行。
- 日冕现有实施记录仍将阶段 D 标为未完成，不能作为完整业务闭环通过证据。
