# Loom ReAct 能力归拢

## 决策

单个 Agent 内部通用的模型、行动、观察、上下文、停止和恢复语义归 Loom。Weave 负责把这些机制装配到已发布成员，并保留团队运行、业务权限、任务身份、交付验证和用户采用。

本轮先迁移已经存在但散落在 Weave 的通用执行组件，再单独补齐 Loom 的完成验证与纠正循环。迁移本身不作为“完整 ReAct Agent”证据。

## 迁移清单

| 原位置 | 新位置 | Weave 保留部分 |
|---|---|---|
| `internal/kernel/compiler/loop_detector.go` | Loom `stdlib.NewToolRepeatGuard` | 发布物中的重复次数配置 |
| `compiler.buildLLMCompactor` | Loom `stdlib.NewSummaryCompactionPolicy` | 是否启用及 token 阈值 |
| `memberJournalLLM`、`memberJournalTools` | Loom journal 装饰器 | PostgreSQL 事务、父运行 fencing、用量、操作身份及产物识别 |

## 边界互审

产品视角：完成标准和用户采用继续属于 Weave，避免 Loom 把某个业务的“合格”固化为通用机制。验证失败如何返回 Agent 并触发下一轮纠正属于 Loom，因为这是所有业务共享的执行语义。

工程视角：Loom 内核五个原语不变，新能力进入 `stdlib`。Weave 通过 `ExecutionJournal` 适配现有事务和租约，不把 workspace、team、member 或 delivery 词汇下沉。默认未启用日志和未来完成验证时，原工具循环行为保持不变。

质量视角：迁移要求 Loom 全量测试、API surface 门禁和 Weave 的 compiler、loomruntime、workflow 回归通过。还需验证同一成员恢复时模型与工具回执复用、工具参数规范化以及产物回执合并保持原行为。

## 缺口与补齐

目前 Loom 的模型在不再请求工具时即可结束，结束表示“模型停止输出行动”，不表示“任务通过外部标准”。重复动作防护解决的是局部卡死；检查点和日志解决的是可恢复性；上下文压缩解决的是历史容量。三者都不能代替目标验收。

Loom `ToolLoop` 已增加可选的只读完成验证器。模型提出最终结果后，验证器返回通过或结构化拒绝原因；拒绝原因进入同一消息历史，模型继续行动；预算或安全边界仍可暂停。操作日志同时提供统一的 `ErrJournalOutcomeUnknown` 分类，日志控制的工具错误会让循环停止，不能伪装成可继续纠正的普通工具错误。

Loom 的独立公开接口样本已经覆盖模型与工具回执丢失、已确认操作复用、结果未知停止、验证拒绝、跨检查点恢复、纠正行动和验证通过。由此可以确认机制层的完整执行路径。Weave 尚未为业务发布物配置完成验证器，因此现有业务行为没有改变，业务级自主完成能力仍待按场景接入和验收。

## 库与 CLI 决策

Weave 继续直接引用 Loom Go 包作为正式主路径。父运行锁、成员 checkpoint、操作回执和用量累计需要紧密组合，改成 CLI 会额外引入进程协议和跨进程事务断点。

`loom run` 保留为 Loom 的跨语言入口、独立验收入口和调试工具。将来只有在明确需要进程隔离或非 Go 宿主时才提升为正式适配器，而且 CLI 必须复用同一套 Loom `ToolLoop`、完成验证和恢复组件，不能形成第二套 Agent 执行实现。
