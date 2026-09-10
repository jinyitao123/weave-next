# Loom 替换验证

2026-09-06，Asia/Shanghai。结论是 **Loom 的工具循环、安全点恢复和历史分支有可实测的价值；当前 Weave 工作流还不能仅靠切换成员引擎完整接入这些能力。**

本轮分为当前产品路径与 Loom 内核实验。产品路径使用当前运行中的本地验收服务；内核实验使用仓库锁定的 `loom v0.8.1`，沿用 Weave 的 `runtimellm` 和 Claude 引擎适配器。两部分结论分开记录。

## 实际结果

| 验证 | 结果 | 证据 |
|---|---|---|
| 现有工作流，成员使用 Claude CLI | 成功读取真实基线，复算三速度并返回报告。第一次手写断言常数错误，成员实际修正并重新执行。 | [运行记录](evidence/cli-activity.json)、[原报告](evidence/cli-final-report.md) |
| 同一验证工作流，物理成员切换为 Loom | 失败。成员配置有 MCP 工具引用，冻结产物中的 `mcp_bindings` 却为空；推理请求没有 `tools`。模型请求了不可用的 Bash，平台拒绝，未生成成功交付。 | [冻结产物](evidence/loom-frozen-payload.json)、[推理请求](evidence/loom-first-inference-request.json)、[失败运行](evidence/loom-activity.json) |
| Loom 内核，真实模型驱动工具循环 | 成功。模型依次请求 `load_baseline`、`calculate_scenarios`、`verify_results`，各一次；工具均真实执行。四次 CLI 推理回执没有原生工具执行。 | [原报告](evidence/toolloop-run-report.md)、[工具服务记录](evidence/tool-events.jsonl) |
| 内存检查点，计算步骤保存后进程退出 | 新进程恢复失败，检查点不存在。 | [故障注入](evidence/mem-crash.json)、[恢复结果](evidence/mem-resume-result.json) |
| PostgreSQL 普通检查点，相同退出位置 | 能恢复，但最后一个计算步骤再次执行。读取一次、计算两次、复核一次。 | [实际调用记录](evidence/pg-events.jsonl)、[恢复结果](evidence/pg-resume-result.json) |
| PostgreSQL，显式 `yield + after_step` 安全点 | 新进程沿用同一运行身份，仅继续复核与汇总。读取、计算、复核各一次。 | [故障注入](evidence/pgsafe-crash.json)、[实际调用记录](evidence/pgsafe-events.jsonl) |
| 从复核检查点分支，修改报告解释条件 | 新运行只执行一次汇总，复用全部三个工具回执；带有父运行与检查点序号。源检查点前后 SHA-256 相同。 | [分支结果](evidence/pgsafe-fork-result.json)、[源记录完整性](evidence/pgsafe-fork-source-integrity.json)、[分支报告](evidence/pgsafe-fork-report.md) |

[机器核对结果](evidence/verification-summary.json) 的 17 项断言均满足。断言包含“产品切换确实失败”和“内存恢复按预期失败”，因此不能把该文件的 `passed` 理解成产品接入通过。

## 产品路径的具体缺口

标准图发布器 `internal/kernel/compiler/standard_frozen.go` 的 `EncodeFactoryInput` 固定输出 `{}`，依赖枚举覆盖模型、运行时和技能，没有枚举成员配置中的 MCP 工具依赖。本轮通过产品 API 保存了一个工具服务引用，服务探针成功发现三个工具，但工作流第 2 版的冻结产物没有这些绑定。失败发生在工具循环的接线处，不能据此判定 Loom 内核无法执行工具。

当前工作流成员执行入口 `internal/base/teamrun/workflow_interpreter.go` 向 Loom 图传入 `loom.NewMemStore()`。团队层保存的阶段检查点，无法自动补足这份成员图内部状态的跨进程持久化。

Loom v0.8.1 的普通 `Resume` 在检查点的 `yield_phase` 为空时，会重入最后一个步骤。显式 `after_step` 才会沿该步骤的路由继续。本轮进程退出码为注入的 `86`，发生在 latest 检查点写入成功之后，恢复使用另一个真实进程。没有将内存复制回恢复进程，也没有手工补写恢复状态。

历史分支也需要选对位置。本轮先验证了终端检查点分支，它只产生新身份，不执行新步骤；最终用于验证报告变更的是复核完成、汇总开始前的第 3 个检查点。原始终端分支记录保留在 `terminal-fork-*` 文件中。

## 输入、模型与验证范围

输入为日冕现有 `baseline.yaml`，快照见 [基线文件](evidence/baseline.yaml)，SHA-256 为 `95ef8f8a35d79596ee25f01ecb391c5a5ca2ab16885609f84454a0921b0d13b1`。限定复算质量 `1e9 kg`、距离 `4.25 ly`、速度 `0.01c / 0.03c / 0.05c` 的经典动能和匀速航行时间。工具以 40 位 Decimal 独立复算，数值相对误差低于 `1e-12`。

模型沿用本机 Claude CLI 的 Kimi 配置。内核推理回执自报 `k3`、`k3[1m]`、`kimi-for-coding-highspeed`；这些是回执报告的名称，不作为服务内部模型身份的独立证明。Claude 在内核实验中只承接下一次模型响应，Loom 执行工具和推进图。

三个受控工具由本次验证探针提供，实际读取文件并计算。恢复实验的图明确拆成读取、计算、复核、汇总四步。这个实验并未为现有工作流补上工具冻结或持久化，也未证明 Loom 可以原样接手 Claude 的全部本机工具。CLI 对照轮自行编写 Python，Loom 轮使用预先定义的受控工具；两轮工具条件不同，不能据此比较整体质量、速度或成本优劣。

数值工具通过不代表最终文字全部正确。Loom 工具循环的原报告额外称 `0.05c` 时相对论动能修正约 `0.09%`，独立按 `(gamma - 1)/(beta²/2) - 1` 计算约为 `0.1879%`。原报告保留该错误；这一发现说明结构化执行与内容核验仍是两项工作。

本轮没有完成 Workbench 的 Loom 选择入口、成员内部恢复入口或历史分支交互验收。浏览器当前业务团队没有全量切换。

## 后续实现的最小边界

1. 为带工具的标准 Loom 成员完整冻结 MCP 依赖，并验证发布后的工具集合与配置一致；保留旧发布物的解释契约。
2. 将成员图的持久化身份关联到工作流运行、节点和执行尝试，明确正常恢复、失败重试与历史分支的不同语义。
3. 选择职责清晰的完成边界接入 `after_step`，为可能重入的工具保留幂等或结果回执约束。
4. 先通过同一个真实任务的中断、继续与限定修改，再接到 Workbench 中让用户操作。

这些是本次验证导出的实现范围，尚未实施。

## 运行身份与收尾

- 验证团队 `a4434d2c-f831-4651-bbbb-c674d0776377`，工作流 `3e510099-0e08-46c4-8ee2-bdecb9b22d9c`。第 1 版为 CLI 对照，第 2 版仅将物理成员换为 Loom。
- CLI 运行 `run-41e8a7b7-e2ca-5ac1-99c9-f20be9cb5df8`；产品 Loom 失败运行 `run-c712d4c0-53d3-5c6c-ae34-10ed620afb0b`。
- 安全点恢复运行 `7d4f807f-2192-495b-81da-ce612dde5134`；有效报告分支 `15eec19e-54b2-44d7-b8d1-570a1fa7aab1`。
- 产品服务为当时正在运行的 `weave-all-repairs-v15`；构建元数据标记基于 `3a9c0b0…` 且包含工作区修改。内核探针编译自当前 `5cbc3eb…` 工作区，锁定 Loom v0.8.1。这不是对两个产品二进制做严格性能比较。
- 验证团队已归档，测试 MCP 绑定已停用，历史运行与检查点保留供核对。见 [清理记录](evidence/cleanup.json)。
- 本轮仅新增验收探针和证据，没有修改生产执行逻辑。探针构建成功，实际执行、故障注入和证据核对已完成；没有将其表述为全仓测试通过。

[复核与复跑说明](probe/README.md) · [文件哈希清单](evidence/manifest.json)
