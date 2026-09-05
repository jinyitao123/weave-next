# Workbench 历史加载与文档检查修复

2026-09-06 的本地复验通过。重复快照写入已降至既有的 30 秒新鲜度刷新，真实大历史会话可正常打开、显示成员记录并刷新恢复。全量文档检查、构建、相关测试与 lint 均通过。验证基于 `8e201fc` 后的本次修复源码；最终源码摘要见 [validation-summary.json](validation-summary.json)。

## 实际变化

轮询器使用字段值比较业务状态，对象属性顺序不构成变化。执行尝试没有变化时保留其更新时间。业务变化立即追加；只有观察时间变化时，沿用每 30 秒一次的新鲜度刷新。[真实轮询样本](live-poll-results.json) 在 65 秒内记录两份快照，间隔 30,310 ms，执行尝试的更新时间保持一致。

Workbench 将 `weave/work-task` 声明为由当前任务投影承载的历史。Session Controller 在完整投影基线覆盖的范围内，以一条带起止序号的记录表示连续快照。浏览器验证序号连续性及投影覆盖范围，分页使用已经接受的基线。消息、工具结果、任务操作与实时追加仍保留。未声明事件、缺少投影的会话及不携带投影回执的原始历史请求保持原记录。

文档检查清理了迁移后失效的路径、注册表和目录，并同步中英文。已删除网站对应的构建与测试任务退出检查列表，其余文档规则继续执行；冻结归档仍验证提交中的封存记录。具体修复及负向回归见 [文档检查记录](../2026-09-05-Workbench视觉交互专项审视/入口与遮挡复验/doc-sync-baseline.md)。总 lint 发现的旧 UI 缩进、行宽和冗余条件也已清理，相关阅读恢复测试通过。

## 真实会话复验

样本为既有的“上线前本地知识库体验评审”，未发送消息或执行业务操作。使用当前正式构建和原数据目录，重启本地 Workbench 验证服务后测试；Weave API 服务保持运行。

| 指标 | 先前诊断样本 | 本次样本 |
| --- | ---: | ---: |
| 历史任务快照 | 11,318 | 21,090 |
| 开场帧字符数 | 199,228,612 | 143,111 |
| 开场帧 UTF-8 字节数 | 未测量 | 175,519 |
| 传输中的任务范围记录 | 0 | 8 |
| 用户及助手消息 | 9 | 9 |

字符数指 JavaScript UTF-16 code units，不等于字节数。两列采样时刻不同，期间旧轮询器仍累积了快照；它们不是相同日志长度的性能基准。先前样本见 [大历史诊断](../2026-09-05-Workbench视觉交互专项审视/现场排版复验/large-history-diagnostic.json)，本次完整测量见 [real-history-results.json](real-history-results.json)。

本次打开和刷新均显示对话、4 名成员及已有阶段成果，页面没有未捕获错误或横向溢出。该次浏览器 JavaScript heap 为 60,310,428 字节。[打开截图](01-large-history-opened.png) 与 [刷新截图](02-large-history-reloaded.png) 均经过查看；截图等待现场展开完成后采集。

[文件保留校验](persisted-history-retention.json) 对重启前已存在的 123,043,399 字节前缀重算 SHA-256，结果一致。后续允许正常追加，未删除、截断或重写原有日志。

## 验证

- `make workbench-install` 通过，锁文件仅增加 Workbench 对 Session Controller 的工作区依赖。
- `make workbench-check` 通过，包含真实 profile 检查和 265 项相关测试。
- Session Controller 全包 35 个文件、427 项测试通过。
- `pnpm run test:gui` 通过，301 个文件、4,005 项通过，1 项既有跳过。
- `pnpm exec vitest run --config vitest.web.config.ts apps/web/tests/workbench-history.e2e.ts` 通过，真实 Loader 组合加载 2,048 份大快照，复用已记录对话，并验证成果、刷新及日志保留。
- `pnpm exec tsx scripts/run-oxlint.ts .` 全量检查通过。
- `pnpm run doc-sync` 最终通过，30 项通过、0 失败、0 跳过。
- `make depguard productguard` 通过，`git diff --check` 通过。

测试中的 12,000 份大快照开场页低于 52 KB，覆盖序号范围、投影缺失、注册释放、分页、实时事件和对话事实保留。实际浏览器复验脚本为 [real-history-check.mjs](real-history-check.mjs)，只在本地读取当前验证服务的认证地址，不保存认证值。

## 范围

本次约束连续重复快照的传输量，不提供任意消息、工具输出或单份大投影的通用字节上限。原有大日志仍占宿主存储，首次观察仍需读取和计算投影。运行中断与等待原节点确认停止属于已有执行事实，本次加载修复不把它们改写为成功或已停止。
