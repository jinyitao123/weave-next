---
description: "面向 Workbench 工具调用的 Weave 原生浏览器呈现，首个视图覆盖团队发现。"
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-weave`

[English](README.md) | 中文

## 概述

`dsh-client-ui-weave` 把持久化的 Weave MCP 调用结果转成 Workbench 产品界面。首个视图把 `mcp__weave__team_list` 呈现为候选团队卡片，展示用途、场景、职责、完成标准和默认工作流可用性，不再直接显示原始 JSON。

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

在 `ui-tool` 之后挂载此包。Weave MCP 服务返回 `team_list` 时，对话里会显示“团队匹配”卡片，并直接列出候选事实。默认工作流不可用、团队需要修复、候选为空、传输失败、调用中止和返回格式异常都有明确状态。

卡片不会声称某个团队已经被选中，选中事实由后续派发调用确认。普通产品界面也不会展示团队 ID、工作流 ID 或内部健康度观察结果。

<a id="understand-the-implementation"></a>
## 理解实现

浏览器侧把准确的 MCP 工具名 `mcp__weave__team_list` 注册进 ui-tool 的键控 `tool.call.toolview` slot。呈现只读取冻结的调用与结果块，历史回放不会查询当前 Weave 状态。合法的紧凑团队文档会转成类型化候选卡片，失败时保留持久化诊断文本。

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

- **目前只专门呈现团队发现**——派发、人工任务和交付物调用仍使用通用工具卡片。
- **候选列表不等于选择结果**——卡片提供可比较事实，但不会推断或保存已选团队。
- **紧凑 JSON 是输入合同**——非数组结果，或缺少稳定 ID 与名称的团队条目，会进入格式异常状态。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护者工作上下文</summary>

Workbench profile 刻意把此包与通用 DSH 品牌包分开，确保上游 Harness 构建不受影响。

</details>
