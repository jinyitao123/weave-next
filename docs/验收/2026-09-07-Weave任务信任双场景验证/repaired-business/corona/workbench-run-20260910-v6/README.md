# 日冕 v6 隔离恢复复验

本目录记录修复 `73dbafb96323f86fdd7e136a225730519d2e0e83` 和 `ec9ea5f8` 后的复验。v5 真实暴露了运行时关闭被误分类为业务失败的问题；v6 随后分别验证了中断恢复、完整成果收集、独立内容验收和精确交付版本的用户采用。结论必须按下面的运行边界理解，不能把不同运行合并成一次无故障端到端运行。

## 最终结论

- 当前可采用版本是第 5 次正式修订运行 `run-15fb877d-b973-5793-a585-de1c942b2009` 的交付版本 `delivery_7eee33883e215e29a10b5f1b3576a974bbed8842994553a97698a0575963d210`。
- 该运行 8/8 阶段完成，平台保存 75 份最终文件，共 545321 字节；`r5-final-manifest.json` 与保存候选逐件哈希一致，成果收集完整。
- 平台外独立复跑通过 G1–G11、真实浏览器三档速度切换、JSON 导出、两份追踪矩阵、本地链接和敏感信息扫描，见 `r5-independent-verification/summary.json`。
- Workbench 对同一保存版本重新核验后保持 16 项通过、1 项 unknown。unknown 对应合同中“旧成果仍通过只读主机路径和哈希清单提供，尚非 Weave 原生跨运行产物绑定”这一限制，当前没有登记的可信检查可把它自动判定为通过。
- 操作方在内置 Workbench 中对该精确交付版本记录了“可以采用”，原始可见结论和边界保存于 `r5-workbench-assessment.json`。用户采用与平台核验是两项独立事实。

## 运行边界

最初的 v6 运行 `run-b265eff8-af4a-507c-ae7d-d1229e14f7ea` 在负责人完成、fanout 检查点已持久化后中断了承载成员的进程。Workbench 接受了三个 infrastructure stage retry，请求保留已完成阶段，并把同一运行推进到新的任务领取代次；随后还记录了成员进程中止确认和同一运行时守护进程重启。相关原始证据为 `pre-interruption.json`、`post-retry-durable-state.json`、`post-retry-accepted-activity.json`、`b2-hung-member-interruption.json`、`b2-daemon-restart.json` 和 `propulsion-stop-ack.json`。

这个初始运行没有形成最终可采用交付，因此只证明持久检查点、精确 stage retry、领取代次和 Workbench 继续路径。最终内容来自后续正式修订运行。第 4 次运行的 75 件候选通过原 G1–G11 和浏览器检查，但额外本地链接扫描发现 `app/index.html` 中两处页脚链接指向未交付文件，所以没有采用。已完成运行不支持成员级纠偏，第 5 次不得声称只重跑了总装员；它是复用冻结候选后执行的一次完整团队修订，成本与范围已在 `r5-final-outputs/FINAL_ACCEPTANCE.md` 如实记录。

## 证据索引

- `r5-run-activity.json` 与 `r5-run-delivery.json`：第 5 次运行、成员、保存交付和平台核验。
- `r5-final-manifest.json` 与 `r5-final-outputs/`：最终 75 件候选及逐件身份。
- `r5-independent-verification/`：平台外独立复跑日志、重算后的验收结果、真实浏览器结果和汇总。
- `r5-workbench-assessment.json`：重新核验后的平台状态和同一交付版本的采用评价。
- `r4-independent-verification/app-local-link-check.json`：第 4 次候选的两处悬挂链接缺陷。

本轮没有外部副作用。旧成果仍依赖只读主机路径和哈希清单传递，尚未成为 Weave 原生跨运行产物绑定；这一点保留为明确限制。
