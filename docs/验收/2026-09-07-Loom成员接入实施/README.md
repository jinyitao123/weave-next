# Loom 成员接入实施记录

2026-09-07。依据[已授权的实施顺序](../../计划/2026-09-07-Loom既有路径补齐与验收顺序.md)，按 A、B、C、D 连续实施。A 已通过；B/C 的定向数据库及真实子进程契约已通过，D 正在实施。尚不能视为完整产品恢复闭环已经完成。

## A：工具发布与实际执行

新发布的、带 managed MCP 配置的 Loom standard 成员使用工厂 v2。CLI 成员与无 MCP 配置的 standard 成员仍按 v1 发布；历史产物始终按其完整工厂键加载。通用工厂选择器仍拒绝歧义，发布与角色证明改用同一项明确选择规则。

v2 工厂输入保存服务引用、工具过滤和写入策略。冻结包保存服务器功能版本、调用定义和凭据引用，定义纳入 MCP 依赖哈希。发布使用最近一次成功探针留下的工具目录；运行时重新核对实际目录，缺失或发生不兼容变化时停止调用。此项不承诺远端实现或输入数据永不变化。冻结包不包含凭据值。

旧 v1 的字节规范保持不变。新增金样本取自实际保存的旧发布包，基于本轮修改前 HEAD 的规范化结果。v2 新输入自身采用稳定的空列表表示；没有全局改写旧 DTO 的空列表规则。

### 已有证据

- `evidence/stage-a-publication-test-5-status.json`：真实 PostgreSQL 中完成候选构建、发布、读取冻结包、重建依赖、编译与实际 HTTP MCP 文件写入。模型为固定响应，只证明发布接线。
- `evidence/stage-a-packages-status.json`：当前 frozen、compiler、freezer、mcpregistry、workflow、teameval 定向包检查成功。设置了真实 `TEST_DATABASE_URL`，每项使用独立测试 schema。
- `evidence/stage-a-real-1-status.json`：真实推理首次样本失败。读取原基线成功，第二轮模型超过验收驱动器的三分钟单轮时限。保留失败，未计为通过。
- `evidence/stage-a-real-2-status.json`：第二轮真实模型样本成功，完成计算代码、数值文件、测试和报告。实际调用 6 次 MCP 工具；发生过代码语法错误和测试映射错误，模型据返回结果自行修复。全部推理回执均未记录原生工具尝试。
- `cases/stage-a-real-2/independent-direct-check.json`：另外直接执行模型生成的测试脚本，并使用宿主独立公式核对五项数值及基线哈希，全部成功。
- `evidence/stage-a-app-status.json`：真实 PostgreSQL 下应用 API 与命令装配层检查通过。

验收模型通过 `runtimellm` 与本机 CLI 推理适配器提供，测试发布模型绑定使用明确的 fixture，运行时替换 LLM 实例。该样本没有验收生产推理来源选择。CLI 回执报告的模型包括 k3/kimi 别名，不能据此宣称独立的底层模型证明。第一轮失败与第二轮修复过程均保留。第二轮驱动器单轮时限为八分钟，生产时限未改。

第二轮真实推理期间还修正了 v2 输入空列表规范化；最终代码另外通过当前包的发布重载检查。中间实验发布包是该次运行的证据，不作为正式发布或后续恢复入口。旧 v1 兼容样本保持原字节规则。

真实样本只验收发布后的成员工具功能，尚未运行完整 TeamRun、Workbench 或进程恢复。样本中的 CLI 只承接模型推理；发现原生工具尝试立即判失败。工具由实际 HTTP MCP 执行，模型调用与工具回执分别保存。完整交付与恢复属于 B 至 D。

## B/C：成员持久身份与通用循环恢复

新增成员登记表，将团队运行、快照、节点与已分配调用 ID 绑定到稳定的 Loom run ID。外层检查点显式保留当前未完成调用，继续时复用；团队代数只用于执行权判断。当前团队“继续”不一定推进 execution lease epoch，因此接管比较团队 generation，并在每次保存时验证团队 generation、execution lease epoch、resume generation 及成员 attempt。

成员复用已有 expected-run、attempt 租约、PostgreSQL 与正常终态事务。成员结果和成员终态原子提交，父流程幂等补收。父终态将成员用量列为子项，其自身用量排除这些子项，避免重复聚合。中断在图返回后释放 attempt；失去执行权的旧进程不能保存结果。

新 v2 图仍运行 Loom 原有的通用 ToolLoop。Weave 在模型与工具边界保存操作日志，记录完整模型请求、响应、调用队列、工具请求、回执和原始用量。恢复从同一个 Loom mid-step 检查点重建循环，只回放已确认响应，不再执行对应的模型或工具。工具批次第一版按顺序执行，循环计数由同一日志重建；模型响应未知的重试保留用量不完整标记。工具结果未知时停在待核对，拒绝普通继续操作。

每个操作边界将回执、Loom latest、同序号不可变 history 与成员进度索引在同一事务里保存。Loom 后续 history 写入只能确认相同字节。保存失败阻止后续派发。整个循环不通过内部 yield 伪造用户暂停或频繁更换 attempt。

已通过的证据包括

- `evidence/stage-bc-published-member-1-status.json`，真实发布包经正式冻结编译与实际 HTTP MCP 执行后保存成员结果，重复进入复用原结果。三轮工具预算在恢复后仍最多执行原三轮。
- `evidence/stage-bc-outer-member-4-status.json`，早期内部外层接线样本验证 park 与继续，后续已由正式 API 发布、派发和继续的 `stage-d-public-integration-7` 替代作为最终接线依据。早期源文件保留在 evidence 中。
- `evidence/stage-bc-real-process-1-status.json`，两个独立子进程争用同一成员，新所有者接管后旧进程写入被拒绝；第三个接收进程复用完成结果。另一用例在首项工具回执提交后强制终止进程，新进程接续后两项写入各发生一次。
- `evidence/stage-bc-member-3-status.json` 及后续运行，原子历史保存失败时模型和工具调用数均为零，工具效果未知时没有自动重放。

这些用例使用固定模型响应以控制故障边界，不代表完整业务质量或真实模型恢复验收。历次未通过日志保留，最终成功日志以以上索引为准。Loom 依赖仍为 v0.8.1，本轮没有修改其仓库。

## D：正式继续、文件交付与产品验收

队列重领未完成成员时保存原指针并等待明确继续。每次继续只授权一次新的执行入口，重复投递及继续后再次退出不会变成自动重试。Workbench 显示真实成员 ID 对应的保存时间，执行权尚未释放时不开放接管；未知工具效果仍停在待核对。成功工具回执通过版本化导出协议携带实际文件，成员终态和父流程通过既有交付事务接收，详细规则见[成员身份与恢复契约](成员身份与恢复契约.md)。

已通过的代码与契约检查

- `evidence/stage-d-public-integration-7-status.json`：真实 PostgreSQL、正式候选发布与派发 API、PGExecutor、冻结 RuntimeLoader、成员循环、StageRetry API、成员同身份继续、根/子用量恰好汇总一次、实际文件内容进入最终交付记录。该用例使用可控模型与工具响应，文件内容为固定 42，只验证系统契约。
- `evidence/stage-d-reclaim-2-status.json`：首次进程重领与继续后再次崩溃均等待用户；普通继续不能绕过未知工具效果。
- `evidence/stage-d-api-1-status.json`：API 进度查询的工作区/快照/节点隔离与原状态不泄露，租约释放前后分别拒绝/接受继续。
- `evidence/stage-d-artifact-1-status.json`：从已提交回执重建实际文件导出，重复进入不再产生工具效果；非法导出拒绝。
- `evidence/stage-d-final-backend-status.json`：设置真实测试数据库后的 `make test`、`make depguard`、`make productguard` 与 platform compose 检查全部成功。分层依赖允许表没有扩大。
- `evidence/stage-d-workbench-serialization-status.json`：Workbench 正式构建、浏览器检查和前端回归通过。实际浏览器发现缺失恢复字段被写成 undefined，导致会话严格 JSON 保存失败；现已改为省略缺失字段，并新增混合旧成员/新成员的持久化边界回归。

真实模型任务已从独立 Workbench 会话派发，使用生产模型绑定、发布加载、成员工具与交付路径。网关只把协议转给真实 CLI 推理并提供限定工作目录的 Python 工具，没有替换生产 HostFactory，也没有预制工程答案。网关身份及回执仅能证明实际执行过这些推理调用，不能独立证明供应方底层模型。

本轮验收配置曾误把队列租约设为 10 秒，短于执行器心跳，导致领队被多次回收；已按正式服务改回 60 秒。后续工程成员读取真实输入后，下一次生成触及驱动器 8 分钟时限，保存原成员并进入等待；通过 Workbench 继续，同一验收网关单轮时限改为 20 分钟。成员 ID 未变，attempt 从 1 变为 2，已确认输入读取没有重跑；截图见 `evidence/stage-d-after-timeout-continue.png`。生产超时配置未改。

继续后模型返回了代码，但外层 JSON 中再次编码的 `args_json` 存在转义错误，工具参数校验在执行前拒绝，首个 D 样本最终失败，没有生成工程文件。不能将这次超时继续替代预定的“计算测试完成、全交付未完成时强制结束进程”出口。后续仅把验收推理网关改为直接 JSON 参数对象，要求小批量调用；仍由真实模型决定业务步骤，原生工具尝试仍立即判失败，生产发布与 HostFactory 不变。这一修正尚待新真实样本验证。

2026-09-07 17:53 的状态为 D 未完成。Mac 锁定，浏览器工具明确要求用户手动解锁；新的 Workbench 样本尚未派发。首次失败的模型请求、响应、API 活动、工具回执及当前阻塞保存到 `cases/stage-d/`，机器状态见 `cases/stage-d/sample-status.json`。解锁后继续独立样本，完成预定进程中断、同一成员继续、最终文件独立验收，才可判 D 通过。

用户解锁并要求继续后，第二轮从 Workbench 独立派发，生成了计算源码；五项参考值独立检查通过。但下一项测试调用的 JSON 响应多出末尾括号，网关严格校验拒绝，第二轮失败，尚未执行该测试。已将其输出隔离保存为 `failed-second-outputs/`，没有人工修补代码或将它冒充新样本结果。

第三轮在清空的输出目录中重新从 Workbench 派发，使用同一冻结输入。验收网关增加至多两次格式修复，由真实模型读取原响应与校验错误自行重发，全部原始响应归档，用量累加；不向工具派发未通过格式检查的响应。该网关仍只作为本次推理传输，生产成员的发布、执行和恢复代码不变。

第三轮实际执行 13 项 unittest 成功后，在检查点 38、6 条工具回执已持久化、完整交付尚未生成时，强制终止本轮隔离 API 进程。新 API 进程重领后停在等待继续。Workbench 点击继续并确认后，同一个成员 `6dd8c3a9-d88d-5f64-89d3-a1fab68b46a7` 的 attempt 从 1 变为 2，检查点推进至 48。独立逐对象核对表明，中断前已确认的 17 次操作（6 次工具、11 次推理）完全不变；恢复后的第 7 次工具调用产生三张 SVG 与 OpenSCAD 源文件，每次工具实际执行恰好一次。证据见 `cases/stage-d/third-independent-recovery.json`、`third-attempt/` 及 `third-sample-final.json`。

随后模型尝试调用原生 Bash 的 `true`，推理网关检测事件后取消并拒绝响应，团队最终失败。尽管命令为无操作，本轮仍按工具隔离规则判失败，不能把第三轮视为完整 D 通过。已产生文件隔离为 `failed-third-outputs/`。独立质量检查另外发现：五项参考值通过，但先锋时间线未采用 ICD 要求的路线专属速度，转向计算缺少指定的速度增量角度接口，三张 SVG 均缺完整基线摘要，应用和最终导出尚未生成。记录见 `third-independent-quality.json`。

第四轮前修正验收传输的工具隔离：启动 CLI 时使用空原生工具集合、空且严格的 MCP 配置和只负责推理的系统提示。保留原生工具事件拒绝检查。独立小样本的 CLI 初始化回执明确报告 `tools=[]`、`mcp_servers=[]`，并返回合法推理 JSON，见 `inference-only-smoke-check.json`。该修改仅限验收适配器，不改变生产 CLI 成员，也不改底层模型选择。第四轮从清空的输出目录与独立 Workbench 会话重新发起，模型只执行一次目录盘点，随后返回下一步计划而结束。根状态为 `succeeded`，最终产物仅为该计划文本；工程文件及成员导出均为零，D 判失败。证据见 `fourth-sample-final.json`、`fourth-deliverables.json` 与 `evidence/stage-d-fourth-empty-final.png`。这也暴露出通用流程的执行完成和本任务要求的实际交付完成仍需分开验收。

第五轮前使用已有服务配置验证原生工具调用 API。[服务官方文档](https://www.kimi.com/code/docs/en/)列出兼容 API 及真实客户端标识要求。以 `weave-acceptance/1.0` 标识请求，没有伪装 CLI；服务将 CLI 专用别名 `k3[1M]` 拒绝并指示使用 `k3`，按该映射重试后，原生工具调用及随后工具结果接回均为 HTTP 200。本轮另存原始请求/响应与服务报告的模型、用量，不声称费用可由 token 推定。服务凭据仅从既有本机配置读取，不复制进冻结包或归档。

第五轮验收网关直接转换通用消息与服务原生工具协议，不再启动 CLI，也不要求模型生成外层 JSON 文本。工具执行仍由 Loom 的 managed MCP 派发。任务工作目录只放字节相同的 inputs/ 与空 outputs/，验收日志和旧样本放在其外；这属于工作目录隔离，不宣称 Python 进程具备文件系统沙箱。第五轮已从 Workbench 真实派发并完成输入读取、模型源码和测试源码生成；在中断后继续时，前 10 条工具回执被正确复用，但恢复消息缺少服务要求的 assistant tool-call 配对，服务返回 `tool_call_id  is not found`，运行失败。该失败记录保留，尚未计入 D 通过。


第五轮失败后，原生适配器增加了从持久化 `tool_call_id` 重建 assistant tool-call envelope 的逻辑，并通过真实服务回归：带有工具回执但缺少 assistant 包络的请求返回 HTTP 200。适配器仍使用服务原生 API、真实客户端标识和现有本机凭据，不归档凭据值；证据见 `native-recovery-reconstruction-smoke.json`。第六轮隔离目录和 API 服务已准备，尚未从 Workbench 派发，因为 Mac 再次锁定。
