# MCP 参数与 CLI 发布绑定机制验收

2026-09-08。代码提交 `acfa5233a4d3fdc427a035778424e8de1fcda1ce`。本批机制检查通过，尚未部署，也未重新派发电商或日冕业务样本。

## 已完成的边界

- 共同参数检查使用 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`。固定默认 Draft 2020-12 和内置 format 断言，禁止外部 HTTP/file schema 加载。重复键、非法 Unicode、尾随值、非对象参数、类型和必填项等错误在上游调用前拒绝。设置 schema、目录、参数大小和解析复杂度上限。
- 参数以 `json.Number` 校验，继续转发原始 JSON 参数，不补默认值或转换类型。实际 HTTP 调用验证了 `9007199254740993` 和 `1e3`。冻结 schema 仍遵守原有 JCS 格式；若规范化会改变数值约束，发布和解码直接拒绝，不静默舍入。
- Loom 与 CLI 复用 `mcphost.FrozenMCPDispatcher`，目录变化用于发现漂移，不能替换冻结 schema。Loom `DispatchWithHooks` 改写后的实际参数仍经过最终检查。受管 CLI 网关保留固定参数错误码供模型修正，屏蔽参数值及任意上游错误文本。
- 新的标准 v3 专用于携带 MCP 声明的 CLI 成员。标准 v1 的旧工件及 Loom 标准 v2 保留原有版本含义。没有把 CLI 标为具备 Loom journal 恢复能力。发布工件经 RuntimeLoader、可信执行上下文和现有 `engine_exec` 队列传递完整 binding。
- 复用 `/v1/runtime/tasks/{id}/mcp/{idx}`，新增的只是窄凭证与领取事实。`0134_task_claim_epoch.sql` 使每次领取原子递增 epoch，续租保持不变。网关在请求入口和最终上游调用前核对任务、runtime、workspace、server index、binding digest、epoch、状态、租约和当前访问许可。冻结连接不跟随 HTTP 重定向。
- daemon 只获得相对任务入口与本次领取凭证，上游连接和密钥保留在服务端。CLI 子进程过滤平台凭据及继承的其他任务令牌。Claude 使用严格 MCP 配置；Codex 通过同一目录与环境查询生效配置，仅对本次进程禁用已有连接并加入任务入口，保留本机登录。已验证 Codex 的 `-c` 会合并配置，不能靠替换表名宣称隔离。

## 已核验的执行事实

`TestFrozenCLIMCPClaimAuthorityAndEffectsRealPG` 使用真实 PostgreSQL、真实 HTTP MCP 上游及受保护的上游凭据。合法调用成功；参数错误无上游调用。续租后 A 仍有效；同一 runtime 在同一时间戳重新领取后 epoch 递增，A 被拒绝，B 可以调用。跨任务、workspace、runtime、server、摘要篡改、缺失 epoch、取消、终态、过期、目录漂移、数据库不可用与撤销访问均无新增上游调用。目录读取期间发生取消，也在最终调用前被拦住。

`TestPublishedCLIToTaskKeepsMCPContractAfterLiveEditsRealPG` 从真实发布事务加载 CLI 工件，先修改实时员工配置并放宽目录 schema，再经实际执行器入队。队列仍保留原始工具定义和员工版本；恢复同一逻辑调用只读取原任务，没有新增物理任务。

完整 `make test` 在隔离数据库 schema 中通过；相关包的定向 race 检查、`depguard`、`productguard` 和差异格式检查通过。原始成功日志以 gzip 无损保存，散列见 [mechanism-verification.json](mechanism-verification.json)。仅保存最终成功日志，不声称已归档此前所有调试输出。

## 尚未完成

这批结果证明受管网关及正常 CLI 配置传递的行为。它没有建立任意本机进程的网络或文件系统沙箱，也没有把存量稳定 agent 网关升级为发布绑定。先前业务样本中的 Bash/Python/curl 直连绕过、只读成员能力与真实写后核对，仍需在新隔离环境验证；不能由参数校验通过推定这些条件成立。

领取 epoch 本批用于 MCP 调用授权，不据此宣称所有旧 runtime 事件、续租及完成接口都已采用 epoch 协议。Workbench 自身的 harness 未改动。Loom 持久预算接入、真实中断续跑、两套业务验收和最终 Loom 边界判断继续推进。
