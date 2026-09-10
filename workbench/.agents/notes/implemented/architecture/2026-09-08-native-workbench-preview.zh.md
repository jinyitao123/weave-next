# Agent Note: 原生预览中的本地 Workbench 资源

Status: implemented

[English](2026-09-08-native-workbench-preview.md) | 中文

## Problem

远程加载页面使桌面呈现依赖 Web 托管，并增加导航权限边界。另写原生对话渲染又会重复既有 Session、输入框和工作现场行为。执行修复仍在进行，此时也不适合新增平行业务传输契约。

## Decision

[桌面预览](../../../../apps/desktop/README.zh.md)在本地打包既有 Workbench 浏览器模块。Electron 负责窗口外观、原生菜单、连接选择、按范围管理下载和独占应用数据。业务视图保留沙箱与上下文隔离，没有 preload 桥接，只加载本地应用资源。本决策扩展[选择策略归属](2026-09-08-desktop-connection-selection.zh.md)，不替代其纯策略或 [Host 传输架构](2026-07-19-gui-web-client-architecture.zh.md)。

原生资源装配器消费同一 Workbench profile 层中的已构建模块。夹具预览明确省略动态 Cordis runner 和插件管理面板，因为对应 Host 端点不存在。既有 schema 编译器需要 `unsafe-eval`，本地业务视图保留此例外，同时网络政策只允许打包资源。连接窗口使用独立的严格 CSP 和 IPC 校验。侧栏、输入框、欢迎页和工作现场样式仍由原包维护，因此桌面与 Web 共用实现。

## Alternatives considered

第二套对话渲染器会重复维护交互和状态。远程 HTML 会使前端版本依赖服务部署。本地打包共用资源保留单一实现，代价是资源构建步骤和较大的 Electron 运行时。[Codex 应用官方示例](https://learn.chatgpt.com/images/codex/app/codex-app-basic-light.webp)提供克制的侧栏与画布层级参考，不据此推断 Codex 内部实现，也不复制其品牌。

## Consequences

预览可以在没有开发服务器的情况下运行，但会话和服务认证仍是夹具，不能证明真实账号、模型、MCP、材料或业务完成。原生与 Web 冒烟使用私有数据操作实际控件，Web 冒烟还启动受支持的 Workbench profile。签名、更新、其他平台和真实 Host 接入仍需独立完成。生成的 Electron 清单不属于 npm 工作区，也不是新增业务 CLI 入口。
