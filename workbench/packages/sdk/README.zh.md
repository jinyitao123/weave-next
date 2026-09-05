---
description: "Workbench 运行底座保留的 JSON-RPC 协议与 stdio 服务端。"
kind: "package-group"
---

# sdk/：从另一进程驱动 Harness 运行时

[English](README.md) | 中文

## 概述

本组保留运行底座使用的 JSON-RPC 协议与 stdio 服务端；当前工作区不包含独立 TypeScript 或 Python 客户端。各包 README 负责各自的协议或服务端约定。

## 目录

- [包](#packages)
- [相关文档](#related-documentation)
- [开发备注](#dev-note)

-----

<a id="packages"></a>
## 包

每个包 README 描述你可用该包部分完成的事情。

| 包 | 职责 |
|---|---|
| [`protocol/`](protocol/README.zh.md) | 协议格式：按换行分帧的 JSON-RPC 传输，以及具名的请求、结果与通知类型 |
| [`server/`](server/README.zh.md) | `jsonrpc` 插件：通过 stdio 为进程外 SDK 客户端提供服务 |

-----

<a id="related-documentation"></a>
## 相关文档

先看协议与服务端包参考，再按需查阅历史决策依据。

- [SDK 应用组合包](../bundle/sdk-app/README.zh.md) — 启动 JSON-RPC 服务器的 `dsh --profile sdk` 应用。
- [Python profile 运行时决策](../../.agents/notes/implemented/architecture/2026-08-23-python-sdk-dsh-profile-runtime.zh.md) — 打包后的 Python 客户端为何启动相同的具名 profile。
- [TypeScript SDK 与 SDK subagent 后端决策](../../.agents/notes/implemented/feature/2026-07-27-typescript-sdk-and-sdk-subagent-backend.zh.md) — 客户端约定及其上的 subagent 后端。
- [SDK 项目工具链移除](../../.agents/notes/implemented/simplification/2026-08-11-remove-sdk-project-toolchain.zh.md) — 本组为何从不创建、配置或构建开发者项目。

<a id="dev-note"></a>
## 开发备注

无。
