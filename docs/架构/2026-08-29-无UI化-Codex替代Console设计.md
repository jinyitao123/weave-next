# 设计：无 UI 化——Codex 桌面 App 替代 Console，删除元团队

> 日期：2026-08-29
> 状态：v2 定稿（已经 codex 只读审计，5 个 P0 全部处置，见 §7）
> 性质：客户端带终局形态——weave 成为无自有 UI 的纯基础设施（对齐 loom 的形态）
> 关联：[产品方案](./2026-08-25-Weave-产品方案-十分钟拉起一支业务团队.md) D5、[M2 设计](./2026-08-26-M2-元团队改户籍设计.md)

---

## 1. 目标与终态

weave 不再拥有自己的 UI 和内置对话产品。人的操作面完全由智能体终端（Codex 桌面 app / Claude Code / CLI）经 MCP/CLI 承担。验收标准：**用 Codex app 完成克苏鲁工作室全流程，全程不打开浏览器。**

## 2. 顺序（互审修正版：六步，先归建再拆家）

| 步骤 | 内容 | 门禁 |
|---|---|---|
| **N-0** | **建造角色归建**：`__config_engineer`、`__graph_designer_tf` 从 registry agent 迁为 build 带受控执行记录（M2c 裁判归建的同构操作）——不做这步，删 seed 会让全新部署的模板建队必坏 | 干净库模板建队通过 |
| **N-1** | MCP 民生面（§3 全部）+ bootstrap（§4） | 工具级测试 |
| **N-2** | Codex app 真实接入验证（§5 剧本逐项 checklist） | **此门不过，不删任何东西** |
| **N-3** | 删元团队（包/seed/gate/开关/测试；旧库资产保留只读，见 §6.4） | 全量绿 + 干净库回归 |
| **N-4a** | 运行时管理面板（独立小页，§6.2 例外规则） | 面板只读现状 + 发 token；零构建链 |
| **N-4** | 删 Console（§6 清单）+ README 重写 | 全量绿 + 干净库 bootstrap 全流程 |

## 3. MCP 民生面（互审修正版）

### 3.1 人工待办

- `human_task_list` → `GET /v1/human-tasks`（列表不带巨型 completed_outputs——分页 + 摘要）；
- **`human_task_get`（新端点）**：任务详情 + **关联的前序节点输出**（终审要读的三章稿子在这里——wait 发生在 assemble/deliver 之前，聚合交付物终审时还不存在，这是拓扑事实不是缺陷）；长内容用 path 参数分章拉取（JSON pointer，RFC 6901，合同细则：只支持 JSON 值、返回选中值本体、超尺寸 413 + 分页参数）；
- `human_task_complete`（payload + idempotency_key）→ 既有 complete 端点。

### 3.2 建队与管理

- `team_create` 工具与 CLI `team up` **增加 declarative_spec 透传**（P0-4：否则克苏鲁建队第一步就得退回 raw curl）；
- `provider_add/list`、`apikey_create`、`runtime_create`（响应含合成好的下一步命令文本——三种形态：直接 `weave runtime` / install.sh / Docker runtime）；
- `team_list` / `team_status` / `usage_summary`（映射 `/v1/usage` + build run usage——"预算花了多少"必须有工具）；
- （可选）`team_evaluate` → `POST /v1/teams/:id/evaluations`。

### 3.3 权限模型修正（P0-3 + P1-11）

- **API key 代表创建者**：key 记录 owner_user_id，middleware 在 membership 类端点以创建者身份解析（否则 admin key 也过不了 human task 的 membership 检查）；
- 每个 MCP 工具的 role/scope 冻结成表（现状实测：provider_add/admin、runtime_create/org、human-tasks/membership 等），bootstrap 生成的 key 显式携带最小必要 scopes（admin,org,chat,runs）；
- **env 变量名修正**：代码读 `WEAVE_API_URL`（v1 设计文档写错为 WEAVE_BASE_URL）。

## 4. bootstrap（首次启动闭环）

`weave bootstrap` 子命令（早分发，DB 初始化后执行、不启动 server）：幂等地确保 admin 用户/密码（首启生成或显式重置）、产出首个 API key（带最小 scopes）、打印 Codex/Claude MCP 注册片段。部分失败可重跑。README 重写为无 UI 快速开始。

## 5. N-2 验证剧本（逐项 checklist，含 P1 修正）

1. 干净库 bootstrap → Codex app 注册 MCP → 工具发现（实测工具数/长响应/重启生效——Codex 无官方上限保证，实测为准）；
2. 建队（含 declarative_spec 的克苏鲁团队）→ 派活（**必须 wait=false**——人工等待型工作流会在 parked 永久轮询，阻塞中的工具调用无法再发起 complete）；
3. 终审：human_task_get 分章读稿 → 驳回带批注 → **手动发起新一轮 dispatch（批注进 input）**——驳回不自动返修是现行语义，剧本如实包含这一步；
4. 放行 → complete → 交付物读取（path 分章）→ usage_summary 翻账；
5. 日常管理 checklist（provider/key/runtime/团队状态逐项）——**N-2 通过 ≠ N-4 安全**，管理面覆盖以逐项 checklist 为准，不以剧本通过为准。

## 6. 删除清单（互审核验版）

### 6.1 元团队（N-3）

`internal/app/metateam` 全包、seed、API gate、`WEAVE_METATEAM_ENABLED`、相关测试、features 端点中的向导标志。**数据策略**：旧库中的 `__` 平台资产保留只读（历史记录/外键完整），新库不再产生；删除代码不删数据。

### 6.2 Console（N-4）

weave-app/、`internal/app/webui`（含 gzip helpers）、Dockerfile ui-builder 阶段、Tauri 壳、ignore 规则、compose/install 文案、`POST /v1/mcp/tools`（仅 Console 用的 MCP 探测面）、teamforge 的 Console next_action 文案、realtime workspace hub（`/v1/events` 仓内唯一消费者是 Console；**请求级 SSE 必须保留**——chat 流式是它）。现状无前端 CI job，无需删。

**例外保留（2026-08-29 用户裁决）：运行时管理页独立存活。** Console 的原罪是"它是整个产品面"；运行时管理是纯运维监控（舰队健康/引擎发现/心跳扫视），对话式接口不适合它。规则：单用途（运行时状态 + 创建发 token，其余只读）、零构建链（单 HTML + vanilla JS，Go embed，不进 React/npm 体系）、app 带归属、禁止业务能力迁入。它不是 Console 的残躯，是一个新的、刻意很小的运维面板。

### 6.3 失去且不替代（如实记录）

团队拓扑可视化、运行轨迹图形化、设计 token 体系、项目/成员/schedule/delivery target/skills/MCP registry 的图形管理面（v1 不补；需求回流再逐项 CLI 化）。

## 7. 互审记录（codex 只读审计，2026-08-29）

| P0 论断 | 处置 |
|---|---|
| 删 seed 破坏新库模板建队（__config_engineer/__graph_designer_tf 仍是 registry agent） | 采纳，新增 N-0 建造角色归建（顺序调到最前） |
| MCP env 变量名错（代码读 WEAVE_API_URL） | 采纳，§3.3 修正 |
| API key 无法过 human task 的 membership 检查 | 采纳，§3.3 key 代表创建者模型 |
| team_create MCP 无 declarative_spec 字段 | 采纳，§3.2 透传 |
| 终审时聚合交付物不存在、无 task→稿寻址 | 采纳，§3.1 human_task_get 返回前序节点输出 + path 分章 |
| human_task_get 无端点；list 塞巨型 outputs 与减上下文目标冲突 | 采纳，§3.1 拆分 |
| 驳回→返修不是现行语义 | 采纳，剧本如实包含手动再 dispatch（§5.3） |
| 预算账本摘要无工具 | 采纳，§3.2 usage_summary |
| dispatch --wait 对人工等待型工作流永久阻塞 | 采纳，§5.2 强制 wait=false |
| 权限表与现状不符 | 采纳，§3.3 冻结 role/scope 表 |
| bootstrap 密码流不闭环 | 采纳，§4 幂等 bootstrap |
| 管理替代面远小于 Console | 采纳，§5.5 逐项 checklist + §6.3 如实记录 |
| deliverable path 合同不完整 | 采纳，§3.1 合同细则 |
| runtime_create 无下一步命令文本 | 采纳，§3.2 服务端合成 |
| 旧 metateam 数据无策略 | 采纳，§6.1 保留只读 |
| workspace realtime 消费者仅 Console；请求级 SSE 必须保留 | 采纳，§6.2 |
| 验证门需干净库场景 | 采纳，§5.1 |
