# 电商首轮真实性审计

本轮业务信任结论为 **不通过**。原始 verifier 得到 7/7、`passed=true`，但独立复核确认仍有一条额外畸变日历事件。所需正确事件存在，不能抵消这条未处理写入。运行已结束；用户接受尚未请求。

## 样本与已取得证据

本轮为固定 benchmark 提交 `084489800bfd3f9f239503eda9e754bc267e98f5` 下 `api-gmail-vendor-brief-mcp` 的新隔离样本，fixture 为 `trust-stage1-vendor-brief-20260908-remote001`，Weave 运行是 `run-e659f172-6876-521c-a6d8-50642aca9c18`。它建立了当前可复验的基线；现有包没有恢复出早于本轮的原电商运行身份、完整输入与 verifier 对应链，不能称为旧运行的完整复验。

任务从 Workbench 派发。已归档团队、工作流版本、输入文件哈希、成员活动、终态和 verifier。两名成员实际走 Claude CLI，配置模型为 `k3[1M]`；因此这条失败样本不能归因于 Loom 工具循环。首次页面记录显示正在执行且没有完成声明，但终态页面是否准确区分执行、交付和接受，缺少完整复查证据。详见 [派发](dispatch.json)、[活动](activity.json)、[页面观察](browser-observations.json) 和 [核验](verification.json)。

## 结果为什么不可信

执行员经 Bash 内 Python/curl 调用 fixture MCP。第一次日历调用按另一套接口形状发送 `start/end` 对象，Mock 接受后把它们转成 `[object Object]`，宾客为空。第二次向 `createEvent` 传旧事件 ID，实际创建了另一个正确事件；原畸变事件保留。公开复核记录确认该状态，Mock 当时未提供 update/delete 工具。见 [发现记录](findings.json) 和 [复核公开记录](review-public-update.md)。

原始 verifier 检查了必要草稿和正确事件，但没有拒绝额外畸变事件；它的 7/7 保留为原始基准结果。独立复核员无法经现有工具回读草稿，明确标为 `UNVERIFIABLE`，却又给出“PASS（限定范围）”。原 verifier 对草稿的通过和复核员的回读缺口分别成立，不能替换彼此，也不能用自然语言 PASS 把副作用问题合并掉。

平台记录了一个 final 和两个 stage 文本产物。此任务的交付是 Gmail/Calendar 业务状态，无需独立文件；文本产物数量不能说明该状态可信。复核员仍能列出写入和发送工具，实际只读靠任务文字约束。本轮没有证据显示复核员写入，也没有证据证明强制只读隔离。

## 可比较指标与缺口

| 指标 | 本轮记录 | 使用边界 |
|---|---|---|
| 原始 verifier | 7/7，passed=true | 只覆盖原检查，不排除额外事件 |
| 业务信任 | FAIL_UNRESOLVED_EXTRA_WRITE | 至少一条已确认、未解决额外事件 |
| 原始运行时长 | 510125 ms | 后端运行窗口，不是人工操作时间 |
| 运行报告输入/输出 token | 877564 / 15960 | CLI 报告口径，见 run-terminal.json |
| 运行报告费用 | USD 1.133645 | 保留原始报告值，不由 token 推算 |
| 运行报告工具调用 | 31 | 与 fixture 的 42 次 workspace/MCP 事件属于不同口径，不能直接相减 |
| 用户接受 | NOT_REQUESTED | Codex 在授权范围内操作不等于用户接受结果 |
| 人工介入次数、人工耗时、结果修改量 | NOT_MEASURED | 不推算、不填写零 |
| 完整派发参数与源文本逐字相等 | NOT_VERIFIED | 源文件哈希和截断输入摘要不足以完成此检查 |

下一步应先封住错误参数与额外写入、把验收聚合规则落实到平台，并以实际工具白名单和可触达端点核验只读权限。修复后用独立 fixture 复验，同时保留原 verifier 分数与更严格的业务信任结论。这一轮不能用于 CLI/Loom 排名，也不能支持扩大执行层能力。
