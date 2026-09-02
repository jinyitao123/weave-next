---
description: "面向 Workbench 团队发现和最终交付物的 Weave 原生浏览器呈现。"
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

[English](README.md) | 中文

## 概述

`dsh-client-ui-weave` 把持久化的 Weave MCP 调用结果转成可见的工作任务。Workbench 会展示已选团队、工作流进度、活跃运行时、人工决策、严格归属当前运行的交付物与明确异常，同时保留对话作为控制入口。

## 目录

- [使用此包](#use-this-package)
- [理解实现](#understand-the-implementation)
- [继续探索](#further-exploration)
- [模型体验](#model-experience)
- [已知限制与后续工作](#known-limitations-and-deferred-work)
- [开发说明](#dev-note)

-----

<a id="use-this-package"></a>
## 使用此包

在 `ui-chat` 和 `ui-tool` 之后挂载此包。会话记录第一次 Weave 调用后，右侧“工作现场”面板自动打开，Session 标题旁也会显示紧凑状态。面板从当前 Session 历史提取团队选择、阶段进度、运行时、人工任务、异常和交付物。只有记录中 `run_id` 与当前运行匹配的交付物才计入任务。

对话保留专用的 `team_list` 和 `deliverable_get` 卡片。工作现场把不透明编号收进折叠的诊断区，以实名运行时展示引擎、提供方和模型，只展示 Weave 实际记录的 CLI 工具输入输出，并把长时间排队或团队运行时缺失呈现为明确问题。

<a id="understand-the-implementation"></a>
## 理解实现

浏览器侧把准确的 MCP 工具名 `mcp__weave__team_list` 和 `mcp__weave__deliverable_get` 注册进 ui-tool 的键控 `tool.call.toolview` slot。它还占用 Session 标头操作列表与 Chat 详情摘要 slot。Workbench 宿主把 MCP 事实折叠进持久 `workTask` 投影，并在前台轮次之外同步对应的 Weave 请求。工作现场只有在用户展开某一份交付物时才把正文挂入页面，对话中的单文件卡片也会限制可见预览长度，同时保留完整的已记录内容用于下载。完整的受限文件存在时，浏览器会生成可下载副本。它不会伪装文件位于本地工作区，也不会解引用 agent 打印的路径。

<a id="further-exploration"></a>
## 继续探索

- [ui-tool](../ui-tool/README.zh.md) — 提供键控工具视图 slot 与通用回退呈现。
- [Workbench app](../../bundle/workbench-app/README.zh.md) — 挂载 Weave 连接与本呈现包。

<a id="model-experience"></a>
## 模型体验

无。此包只在浏览器中呈现已经记录的工具调用，不会增加工具、提示词、消息或模型请求内容。

#### KV Cache 影响

无。呈现发生在运行时记录调用结果之后。

## 已知限制与后续工作

<a id="known-limitations-and-deferred-work"></a>

- **每个 Session 只展示一个任务**——当前投影跟踪 Session 中最近一次 Weave 派发；多派发任务分组仍暂缓。
- **运行时详情是观察事实**——面板展示注册运行时名称与冻结的引擎、提供方、模型事实；缺失事实仍明确显示为未报告。
- **紧凑 JSON 是输入合同**——非数组结果，或缺少稳定 ID 与名称的团队条目，会进入格式异常状态。
- **交付物是远端不可变文件**——Workbench 可以预览和下载已记录正文，但不会把它物化到当前工作区。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护者工作上下文</summary>

Workbench profile 刻意把此包与通用 DSH 品牌包分开，确保上游 Harness 构建不受影响。

</details>
