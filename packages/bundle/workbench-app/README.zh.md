---
description: "面向用户与维护者说明如何通过 MCP 把 DSH 浏览器运行时连接到持久化 Weave 团队。"
kind: "package-bundle"
---

# `@deepseek-ai/dsh-workbench-app`

[English](README.md) | 中文

## 概述

本组合包在不修改 agent loop 的前提下，把通用 DSH Web 运行时变成 Weave Workbench。它加入 Workbench 浏览器身份，在业务 API key 存在时连接本地 `weave mcp serve` 进程，并给前台 agent 一条产品规则：派活前先匹配已有团队；如果没有合适团队，就如实说明并协助定义团队。宿主侧 WorkTask 投影会记录派发，并在前台回合结束后继续同步 Weave 状态。派发成功的 Weave 运行就是持久任务；前台 agent 不再创建影子 DSH 目标、轮询到结束或保存重复交付物。

## 目录

- [使用本包](#use-this-package)
- [界面边界](#surface-boundary)
- [凭据边界](#credential-boundary)
- [开发备注](#dev-note)
- [模型体验](#model-experience)
- [已知限制与待办事项](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## 使用本包

运行 `dsh --profile workbench`，或使用仓库快捷命令 `pnpm workbench`。快捷命令优先使用显式 `WEAVE_API_KEY`，随后检查 macOS 钥匙串服务 `weave-workbench-api-key`；其他平台使用显式环境值。随附 profile 依次叠加 `dsh-base`、`dsh-web-app` 和本组合包。存在 key 时，本组合包通过 stdio 启动 `weave mcp serve`，并把工具发布到 `mcp__weave__*` 命名空间。本地 Workbench 默认把 `WEAVE_API_URL` 设为 `http://127.0.0.1:18080`，`WEAVE_COMMAND` 可以选择另一个受信任的本地二进制。

缺少 `WEAVE_API_KEY` 时，MCP 配置项会被禁用，但浏览器仍能启动。这样前台运行时仍可使用，同时连接缺失不会被伪装；agent 会被要求在工具不可用时不得声称已经完成团队工作。

<a id="surface-boundary"></a>
## 界面边界

Workbench 客户端保留 DSH 的会话、工作区、模型、权限、人工确认、工具与交付物底座。它不展示 DSH 内测声明、DeepSeek 官方凭据首次引导、预览版标记、官方品牌占位、Subagent 呈现、消息反馈控件与轨迹检查界面。非 Workbench 构建中的 DSH 通用行为保持不变；Workbench 的模型设置页面仍然可用，但不会再以首次启动弹窗阻塞用户。

<a id="credential-boundary"></a>
## 凭据边界

`WEAVE_API_KEY` 只从受信任的宿主进程传给本地 MCP 子进程。它不会写入客户端产物，也不会加入模型上下文。`WEAVE_SECRET_KEY` 与 `WEAVE_SECRET_KEY_FILE` 只属于 Weave 服务端，用于加密存储凭据；本组合包永远不读取或转发这两个值。

<a id="dev-note"></a>
## 开发备注

无。

<a id="model-experience"></a>
## 模型体验

### Workbench 团队路由 persona

#### 模型看到什么

Profile 把这条规则注册为独立的 Workbench 提示词段，因此每个会话选择的 agent preset 都无法覆盖它。前台 agent 面对实质性业务工作时必须先使用 `mcp__weave__team_list` 等工具列出并匹配 Weave 团队，随后以 `wait=false` 通过合适团队的默认工作流派发。派发后最多调用一次状态接口确认交接，随即返回团队、运行 ID 和当前状态。它不得创建 DSH 目标、在前台轮询 Weave 运行，或保存重复交付物。Workbench 负责后台状态投影；运行终态或用户稍后索取时，模型再读取 Weave 最终交付物。没有匹配团队时，它必须如实说明并协助定义团队。只有用户明确要求时才可使用自由协作；Weave 连接不可用时也必须诚实报告。

#### Token 影响

一段稳定的产品 persona，加上连接成功后由 Weave MCP 服务发布的工具 schema。

#### KV Cache 影响

Workbench persona 与已连接的 Weave 工具集合不变时保持稳定。MCP 连接、断开或工具集合变化会改变请求前缀。

## 已知限制与待办事项

<a id="known-limitations-and-deferred-work"></a>

- **凭据发现因宿主而异**：仓库启动器支持 macOS 钥匙串；其他打包宿主目前通过进程环境提供 `WEAVE_API_KEY`。
- **暂时没有专用连接卡片**：启动日志会显示 MCP 进程不可用；Workbench 原生连接状态提示仍待补充。
