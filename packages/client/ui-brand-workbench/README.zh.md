---
description: "面向维护者说明如何基于 DSH 通用品牌槽位构建 Weave Workbench 浏览器身份。"
kind: "package-reference"
---

# `@deepseek-ai/dsh-client-ui-brand-workbench`

[English](README.md) | 中文

## 概述

本包使用 Weave Workbench 标记和构建时配置的产品名称填充浏览器现有品牌槽位。它仅在 `DSH_CLIENT_BUILD_PROFILE=workbench` 时启用，因此上游 Web 构建和官方构建仍保留各自身份。本包只负责呈现，不修改提示词、工具、会话或持久化状态。

## 目录

- [使用本包](#use-this-package)
- [开发备注](#dev-note)
- [模型体验](#model-experience)
- [已知限制与待办事项](#known-limitations-and-deferred-work)

<a id="use-this-package"></a>
## 使用本包

把本包挂载为浏览器配置项，并使用 `workbench` 客户端 profile 构建。它等待三个通用槽位全部声明后，把侧栏标记、侧栏名称和会话首屏标记作为一个可撤销 effect 安装。可见名称来自 `DSH_CLIENT_TITLE`；Workbench 构建 profile 将其设置为 `Weave Workbench`。

<a id="dev-note"></a>
## 开发备注

无。

<a id="model-experience"></a>
## 模型体验

无。本包属于浏览器呈现占位，不注册任何模型可见内容。

#### KV Cache 影响

无。本包只改变浏览器呈现。

## 已知限制与待办事项

<a id="known-limitations-and-deferred-work"></a>

- **名称是构建时值**：更改产品名称需要重新构建浏览器产物；运行时 profile 重载不会改变已嵌入浏览器的值。
