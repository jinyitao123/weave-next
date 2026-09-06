# 日冕 20 项问题修复与验收

更新于 2026-09-06。本页是当前状态；此前失败、原始问题及截图完整保存在 [初轮记录](初轮失败与20项问题原始记录.md)。没有修改旧运行的失败结论。

## 当前结论

**执行主体说明：OpenSCAD 的下载、安装、系统签名检查，以及对前轮 SCAD 原文件的编译和渲染，均由当前 Codex 修复任务执行，并非 Weave 团队自主执行。此轮属于人工补齐环境后的验收，不能作为团队自主解决工具缺失并闭环的证据。后续独立审查只核对团队实际结果，不代做缺失的专业执行后再归功于团队。**

20 项涉及的代码、配置和默认执行指令修复已部署，并已完成本轮闭环证据。第 20 项在机器、交付、模拟 DOM 和真实 Chrome 交互层均已验证；内置浏览器仍按安全策略拦截 `localhost` 与 `file://` 本机页面，该记录仅说明工具边界，不再构成最终验收阻塞。

真实运行 `run-6090ab9d-300c-5fc3-a37c-28b7110ec0b5` 使用工作流第 5 版，于 18:53:12 成功完成 9/9 阶段。平台实际发布 89 个最终文件（459177 字节），下载后逐文件 SHA-256 一致；独立复跑退出 0，10 个本地 HTML 链接全部有目标。

[完整交付包](/Users/jinyitao/Documents/日冕/Weave交付-20260906-6090ab9d.zip) · [打开与复跑说明](/Users/jinyitao/Documents/日冕/Weave交付-20260906-6090ab9d/打开与复跑说明.md) · [团队原始验收报告](/Users/jinyitao/Documents/日冕/Weave交付-20260906-6090ab9d/outputs/FINAL_ACCEPTANCE.md)。

八名成员均使用 Claude CLI，本机配置指向 Kimi。页面分别显示配置服务地址、节点默认模型及 CLI 回执报告的模型。这里的 Claude CLI 是执行引擎名称，不能据此推断使用 Anthropic 模型。

## 逐项状态

| 编号 | 问题 | 当前修复与证据 | 状态 |
|---|---|---|---|
| 01 | 失败卡过重、占用空间过多 | 收紧卡片样式，保留失败详情及进展入口；真实页面前后复核。 | 已修复 |
| 02 | 输入路径错误 | 四个真实输入路径已更正，本轮负责人读取并核对。 | 已更正 |
| 03 | 会话派发没有推导项目归属 | 从所属会话补齐项目，拒绝不匹配；真实 PostgreSQL 派发验证通过。 | 已修复 |
| 04 | 重复派发被误判冲突 | 保留原始请求指纹，仅对显式项目补充比较；数据库验证不产生重复任务。 | 已修复 |
| 05 | 当前模型与工作流冻结配置不一致 | 成员执行配置与关联工作流原子发布；当前运行冻结版本与已保存配置一致。 | 已修复 |
| 06 | 凭据、网关配置错误被误判为工作失败 | API 模式增加凭据预检，明确配置错误优先归类且不盲目重试。 | 已修复 |
| 07 | 空模型无法清除、原生 CLI 被迫依赖平台绑定 | 支持显式清空；原生 CLI 模型与备用模型可冻结发布，不要求平台供应商绑定。 | 已修复 |
| 08 | 页面没有按成员配置执行引擎的入口 | 设置 → 运行节点新增成员配置，支持引擎、节点、模型与备用策略；同名成员显示独立标识。实际页面保存与数据库回滚验证通过。 | 已修复 |
| 09 | 账号授权标签遮蔽真实供应商来源 | 显示 CLI 本机配置、服务地址与默认模型；工作现场保留执行回执模型。当前明确显示 Kimi 配置。 | 已修复 |
| 10 | 只能在执行结束后看到活动 | Claude 公开文本及工具活动在执行中回传，按物理任务和序号去重；真实完成前截图已保存。遗漏或截断仍明确标注。 | 已修复并实测 |
| 11 | 中间文件过早标为最终成果、分类缓存未刷新 | 中间文件归为阶段产物，Host 同步更新已有标签；真实页面复核通过。 | 已修复 |
| 12 | 排队成员或历史成员状态不真实 | 根据当前物理任务区分排队/执行；活动窗口截断时从持久化回执恢复完成状态；纠偏旧执行不冒充当前执行，耗时使用真实 CLI 起止时间。 | 已修复并实测 |
| 13 | 没有真正走过单速度到三速度纠偏 | 单一 0.03c、1.0.0 启动；页面登记 CR-001，安全点确认后冻结三情景 1.1.0 并重新执行，最终成果已发布。另一次定向纠偏只重做总装，保留专业分支。 | 已完整实测 |
| 14 | 源码和页面依赖未进入成果 | 支持 CJS、ASCII DXF 等 UTF-8 文件，容量为 128 文件且原字节上限不变；最终送达 89 文件，实际渲染封装为 SVG 随包传递。下载集合原文一致，10 个页面本地链接齐全，本机复跑退出 0。 | 已完整实测 |
| 15 | 上游文件未传递，各成员重造基线 | 初轮五份基线逐字节一致；纠偏后当前基线为 8262 字节、SHA-256 为 592569b7…；总装实读平台传入 78 文件并核验哈希。最终保留唯一当前版本及独立命名的历史资料。 | 已完整实测 |
| 16 | OpenSCAD 只有结构检查 | 安装由 Codex 完成；Weave 数字产品和总装成员实际调用 OpenSCAD 渲染并读取图像，最终 SVG 内嵌真实 42305 字节 PNG。 | 已实测；安装为人工补齐 |
| 17 | 用量摘要截断导致完整结果遭拒 | 修复 UTF-8 截断，附属用量异常降级并保留正文和文件；真实 PostgreSQL 完成接口验证通过。 | 已修复 |
| 18 | 明确格式拒绝仍无限重传 | 400/413/422 停止同包重发并记录失败；临时错误重传结果而不重跑 CLI。HTTP 回归通过。 | 已修复 |
| 19 | 服务重启造成取消或重复执行 | 稳定逻辑调用身份、CLI 进度记录与结果保存生效。纠偏已应用后的恢复改为幂等，保留原应用时间及单次审计事件；真实重启后直接取回总装原回执并完成发布。全部 16 个物理执行与两轮专业工作/两次总装一一对应，无额外 CLI 重跑。 | 已修复并实测 |
| 20 | 智能体用自评 PASS 掩盖未执行硬门 | 统一默认指令要求职责/授权内诊断修复、复验与证据核对；文件交付边界明确。Weave 真实修复并重跑，最终报告明确 Node 模拟 DOM 非真实浏览器。Codex 下载后机器复验通过；真实 Chrome 打开交付应用并完成场景切换、参数重算、JSON 导出和 OpenSCAD 预览检查。 | 已完整实测 |

## 验证边界

最新 v15 的 Go 分层、产品边界及 `make test` 通过；纠偏首次恢复与提交后重启恢复另用真实 PostgreSQL 验证通过。Workbench 安装、构建、启动入口和 27 文件 269 项检查通过；配置页定向检查 3 文件 9 项通过。成员配置原子发布、草稿冲突回滚、过期版本冲突、CLI 首阶段恢复与写入者隔离均使用真实 PostgreSQL 验证。相关证据见 [详细进展](全面修复进展.md)。

自动模型回退已通过持久化回执及恢复复用测试。另一次明确注入主模型不可用回执的实验中，备用模型通过真实 Claude CLI 完成，执行器恢复后没有第三次调用。注入失败不是供应商自发失败；真实探针显示 Kimi 会接受人为构造的未知模型名，不能用请求别名冒充模型选择生效。

## 执行归属与剩余边界

Codex 修复平台、安装 OpenSCAD，并通过独立验收发现预览传递和当前情景导出的缺口，随后登记定向纠偏。Weave 成员完成专业成果，自行处理 PyYAML 缺失、限时工具缺失、只读副本、目录、图纸重建等问题，并修复反馈中的交付缺口、重新检查。这是有监督的真实闭环，不能称为整轮无人介入。

团队验收报告中的 ACCEPTED 是其机器验证判定；最终真实浏览器核验由 Codex 另行在 Google Chrome 完成。2026-09-06 解锁后，Chrome 打开 `http://127.0.0.1:18182/app/index.html`，验证默认 `S-0.03c`、切换 `S-0.05c`、修改质量为 `5000000000 kg`、切回 `S-0.03c`、导出 `selected_scenario=S-0.03c` JSON，并打开 `COR-DWG-004_preview.svg` 的 OpenSCAD 内嵌位图预览。复跑脚本还显式引用本机原始 acceptance.json；交付包附有输入快照，跨机时需配置该路径，导出没有静默修改团队源码。

## 关键证据

- [最终运行完成](evidence/final-run-completed.json)、[平台文件清单与哈希](evidence/final-platform-export-manifest.json)、[下载副本独立核验](evidence/final-downloaded-files-audit.json)。
- [真实 Chrome 浏览器验收](evidence/final-real-chrome-browser-smoke.json)、[内置浏览器策略边界](evidence/final-browser-policy-boundary.json)。
- [Weave 实际修复与复验](evidence/weave-finalizer-repair-and-recheck.json)、[最终物理执行记录](evidence/final-physical-task-ledger.json)、[纠偏恢复真实数据库验证](evidence/correction-recovery-real-pg.log)。
- [最终实际渲染图](evidence/23-weave-final-scad-render.png)、[最新 Go 检查](evidence/final-v15-go-checks.log)。

- [成员配置实际保存](evidence/15-member-execution-saved.png)、[同名成员与本机配置](evidence/18-member-disambiguation-and-cli-source.png)、[当前供应商来源](evidence/19-runtime-provider-current.png)。
- [Claude 完成前公开活动](evidence/17-claude-live-before-completion.png)。
- [平台基线文件逐字节核对](evidence/current-baseline-handoff.json)。
- [并行重启前](evidence/parallel-restart-before.json)、[重启后](evidence/parallel-restart-after.json)、[数据库物理任务身份](evidence/parallel-restart-task-identities.json)。
- [OpenSCAD 安装校验](evidence/openscad-installation.json)、[前轮原文件实际编译与渲染](evidence/openscad-original-artifact-check.json)、[真实渲染图](evidence/20-original-scad-actual-render.png)。
- [真实 CLI 备用模型与恢复实验](evidence/native-fallback-injected-live.log)。

当前任务不是施工、制造或飞行认证级设计。执行成功、文件存在和模型自评都不能替代专业成果验收。
