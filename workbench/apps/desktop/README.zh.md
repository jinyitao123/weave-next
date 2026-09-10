---
description: "在 macOS 原生预览中运行共用 Workbench 前端，并验证隔离的服务选择。"
kind: "package-reference"
---

# Workbench 桌面预览

[English](README.md) | 中文

## 概述

此 Electron 预览将现有 Workbench 前端随应用打包，提供原生窗口控件、菜单、连接设置和下载。当前目标为 macOS arm64。两个专属回环服务和既有浏览器夹具用于验证交互，不连接账号或发起业务工作。

## 目录

- [运行预览](#run-the-preview)
- [理解实现](#understand-the-implementation)
- [进一步阅读](#further-exploration)
- [模型体验](#model-experience)
- [已知限制与后续工作](#known-limitations-and-deferred-work)
- [开发备注](#dev-note)

-----

<a id="run-the-preview"></a>
## 运行预览

在 `workbench` 目录使用固定版本 pnpm。修改共用前端包后先重新构建，桌面资源装配读取这些产物。Electron、Packager 和 Playwright 在工作区根清单中固定为开发依赖。图标渲染和浏览器验证需要 Playwright 的 Chromium headless shell。

```sh
pnpm install --frozen-lockfile
pnpm exec playwright install chromium --only-shell
pnpm run build:workbench
pnpm run desktop:dev
pnpm run desktop:package
pnpm run desktop:test
node apps/desktop/scripts/web-smoke.mjs
```

打包将 `Weave Workbench Preview.app` 写入 `.dsh-build/desktop/Weave Workbench Preview-darwin-arm64/`。`.app` 自带运行时和本地前端，无需开发服务器。`Cmd+,` 打开连接设置，`Cmd+N` 发起前端会话意图，`Cmd+B` 切换既有侧栏。编辑使用原生菜单和右键操作。退出应用只停止其专属预览监听器。

默认数据目录为 `~/Library/Application Support/Weave Workbench Preview`。测试可以传入 `--desktop-data=<absolute-directory>`，独占偏好、端口和浏览器分区。连接设置只接受显示的两个夹具服务。连接数据损坏或保存端口被占用时明确报错，应用不会静默重置。工具栏明确标注预览环境。

-----

<a id="understand-the-implementation"></a>
## 理解实现

<details>
<summary>原生层与前端职责</summary>

[ConnectionSelection](src/connection-selection.ts)负责所选 origin、固定实例和响应范围，不持有业务状态或服务器取消权限。[FilePreferences](src/preferences.ts)写入独占临时文件、刷盘并原子重命名。重命名是提交点，不承诺目录同步或断电持久性。单个应用实例独占其数据目录。

[主进程](src/main.ts)通过 `weave-shell://app` 加载可信连接窗口，通过 `weave-app://workbench` 加载共用 Workbench。业务视图没有 preload、Node 集成、文件系统或 shell API，只有可信窗口获得经校验的窄 IPC 桥接。每个 origin 与固定实例拥有独立持久 UI 分区，候选认证使用新建的内存分区并在退役时清理。切换失败保留已选偏好，客户端保持断开。下载归属已接受的连接范围，范围退役时取消；传输中断明确显示失败。迟到的完成事件不能更新新服务。

[资源装配器](scripts/frontend.mjs)组合现有 Workbench profile 的浏览器模块。独立预览没有 Host 插件管理端点，因此省略动态 Cordis runner 及其管理面板，并使用既有夹具传输；Web profile 保留正常装配。共用对话、侧栏和工作现场组件继续由原包维护。既有 schema 编译器要求本地业务视图的 CSP 允许 `unsafe-eval`；该视图只接受打包资源，不能获取远程脚本或服务。可信连接窗口使用独立的严格 CSP。

</details>

-----

<a id="further-exploration"></a>
## 进一步阅读

[选择策略归属](../../.agents/notes/implemented/architecture/2026-09-08-desktop-connection-selection.zh.md)说明可复用策略。[原生资源归属](../../.agents/notes/implemented/architecture/2026-09-08-native-workbench-preview.zh.md)记录应用边界。[并行任务计划](../../../docs/计划/2026-09-08-Workbench桌面接入并行任务计划.md)维护后续接入顺序。

<a id="model-experience"></a>
## 模型体验

无。预览不新增模型工具、提示词或执行协议。前端展示合成夹具会话，不调用真实模型。

<a id="known-limitations-and-deferred-work"></a>
## 已知限制与后续工作

- 真实账号、Host 传输、MCP、材料派发和业务回执需要在接收执行修复提交后接入。本地夹具认证不能证明用户授权。
- 当前应用是未签名的 macOS arm64 预览。Windows、其他架构、公证、更新和干净设备安装尚未验证。
- 保存的 UI 分区按服务与实例隔离，尚无多用户退出政策或系统凭据存储实现。
- 此目录没有 npm 工作区包或业务 CLI 入口。构建脚本只在生成资源中写入 Electron 清单，受支持的 Node Host 仍通过 `dsh` profile 启动。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护上下文</summary>

[原生冒烟脚本](scripts/smoke.mjs)独占可清理数据和监听器，可通过 `WEAVE_DESKTOP_EXECUTABLE` 测试打包后的可执行文件。[Web 冒烟脚本](scripts/web-smoke.mjs)启动私有、无密钥的受支持 Workbench profile。两者均不提交业务工作，发行验收与这些预览检查分开。

</details>
