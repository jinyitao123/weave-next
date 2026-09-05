# Workbench 单一产品边界清理

Weave 只服务 Workbench。独立业务控制台、独立 Codex/Claude 接入和独立业务 CLI 退出；边界外源码、依赖、脚本和方案直接删除，不另建归档。

## 保留依据

| Workbench 使用场景 | 接口与实现 | 保留原因 |
| --- | --- | --- |
| 接单与团队准备 | `mcp serve`、`internal/app/mcpstdio`、`weaveclient`、团队模板/团队构建 | 正式 Workbench 配置通过 MCP 准备团队和派发 |
| 派发与后台执行 | `/teams/:id/dispatch`、`/chat-requests/:id`、工作流/自由协作执行、内部会话与项目归属 | Workbench 需要接单回执、后台运行、重访状态；内部持久化继续支撑执行 |
| 进度、调整与恢复 | `/runs/:id/activity`、human-tasks、corrections、stop、stage retry、resume | 主对话和工作现场的真实操作与回执 |
| 领取成果 | `/deliverables`、`/deliverables/:id`、`/deliverables/:id/content` | Workbench 列表、预览与完整文件获取 |
| 外部执行与增量过程 | `/runtime/*`、MCP/LLM 任务网关、daemon、Codex/Claude 等引擎 | 成员执行、心跳租约、公开输出与停止确认 |
| 私有部署与维护 | serve/bootstrap/runtime、鉴权/API key、provider、runtime CRUD、运行时维护页面 | Workbench 可部署、可连接、可维护所需基础能力 |
| 执行历史 | 数据库迁移、任务/成果/恢复存储与相应验证 | 保留真实历史与恢复依据，不删除业务数据 |

Workbench 的项目导航与主对话由 Workbench 自己维护；它们不调用旧控制台 `/projects` 或 `/conversations` 管理接口。`POST /chat` 公开入口可退出，但团队自由协作仍调用内部 `handleChatRequest`，不能整删执行实现。`POST /mcp/tools` 是旧浏览器任意 URL 工具发现代理，与正式 MCP stdio、执行期 MCP 注册表和任务网关不同。

## 本轮范围

三个智能体分别负责运行时维护前端、CLI/bootstrap/MCP、产品文档；主代理负责旧公共 API、引用清理、边界检查和整体验证。每个源码文件只有一个修改负责人。

执行前已按最初要求校验工作树快照。之后按用户追加要求不再创建额外归档。旧功能源码直接删除，Git 历史不改写。

## 验证记录

清理前的工程和页面验证保留原有时间及适用版本，不能作为清理后通过依据。

已删除旧业务页面及独占组件、接口封装、样式与资源；运行时维护前端只保留登录和运行节点管理。同步删除旧原生工作区/差异桥接和独占依赖，并重新生成后端嵌入的维护页面。旧对话、项目、收件箱、来源连接、单智能体调度、任意 URL MCP 工具代理等公共入口及其独占存储方法退出；团队执行使用的内部会话、项目归属、团队工作流调度、成果存储和迁移继续保留。

独立 `team`、`status`、`deliverable` 业务 CLI 与 Codex/Claude 客户端配置生成退出。保留 Workbench 使用的 21 项 MCP 工具，职责指引统一为主对话、确切任务、真实回执。旧 ontology/Neo4j 独立部署组合和六份与当前产品方向冲突的方案直接删除。中英文 README、当前产品合同、发布基线和架构索引已对齐；执行/迁移历史证据明确其原验证时间和适用范围。

`make productguard` 检查已退出路径、客户端注册代码和真实 HTTP 注册表；`make ui-embed` 与 CI 的嵌入资源差异检查防止已删除的页面继续从旧构建产物提供。

清理版本 `23c6b1d-dirty+20260905-workbench-only-e72e4124` 已通过带 race 和隔离 PostgreSQL 的 `make ci`，包括 `make test`、`make depguard`、四层预算和产品边界检查。维护前端干净离线安装、lint、设计令牌检查、类型与构建、实际 Rust 编译已通过。该版本已安装并通过真实服务健康检查。

真实 Workbench 新任务 `run-f1f40d1d-0365-5a89-8acd-950ca545b353` 暴露了另外两类问题：负责人在计划中提及最终路径被误判为已经交付但缺文件；任务同步未完整恢复团队名称，并由自身记录触发高频轮询。此任务未施加断线故障，仅负责人执行，保留为失败证据。相关修复和新一轮端到端验收仍在归拢，不能将上述工程通过写成恢复闭环通过。容器实际启动结果另行记录。
