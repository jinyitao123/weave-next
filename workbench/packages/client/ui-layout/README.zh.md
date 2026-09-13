---
description: "Web GUI 的外壳布局：三栏 AppFrame、拖动手柄与让步行为、面板几何服务与主题呈现；供窗口外观的用户与维护者阅读。"
kind: "package-reference"
---

# @deepseek-ai/dsh-client-ui-layout

[English](README.md) | 中文


Workbench 外壳标记自身的产品呈现，使详情和导航样式遵循该布局，不影响其他配置。

## 概述

`shell.access` 入口通过 `onAccessChange` 报告账号身份。Workbench 在入口确认身份前不挂载业务视图，身份失效时卸载整个业务树；其他构建配置保留直接组装方式。回调只传递身份值，列布局和子插槽仍由布局组件负责。

本包提供 Web GUI 的外壳布局：一个三栏 AppFrame，带可缩放的侧栏与详情面板；一条让步链，在空间不足时先收缩详情栏、随后自动关闭它；以及 `ctx.layout` 面板几何服务，供其他插件调用以打开或关闭详情栏。它还承载主题呈现器，把解析后的配色方案、别名 token、正文字号与 `theme-color` 元数据投影到 document。需要标准窗口外观时选择它；Workbench 保留现场宽度，重新加载会重置面板打开状态。

外壳还声明可选的 `useHostManagement` 投影与 `startPersonalSession` 命令，由拥有账号的产品通过 root standard sources 提供。通用界面只消费契约，不导入账号实现。Workbench 缺少权限投影时隐藏 Host 管理；其他构建配置保留原有导航。

## 目录

- [使用本包](#use-this-package)
- [理解实现](#understand-the-implementation)
- [进一步探索](#further-exploration)
- [模型体验](#model-experience)
- [已知限制与延期工作](#known-limitations-and-deferred-work)
- [开发备注](#dev-note)

-----

<a id="use-this-package"></a>
## 使用本包

在 root 槽位挂载本插件，渲染导航、对话与详情。Workbench 在内容区域至少有 900px 时，以初始 40/60 比例并排打开对话与现场，对话至少保留 420px、现场至少保留 480px，均未扣除内部留白。拖动分隔线或使用其左右方向键调整比例，现场宽度在关闭和重新加载后保留。空间不足时先把导航收为 56px 控制栏，再在对话与现场之间切换，不叠加浮层。主动开启全幅模式会占满内容区域，普通打开则恢复分栏。两侧内容树始终保持挂载；隐藏视图不参与键盘和辅助导航。其他 profile 沿用标准详情栏让步链。

### 主题呈现

呈现器消费解析后的主题快照，并投影到 document：`html { color-scheme }` 驱动原生 UA 控件，依据当前配色方案设置 `body[data-ds-dark-theme]`，把主题的别名 token 与 `--dsh-content-font-size` 设为 body 上的内联变量，并持有一个 `<meta name="theme-color">`，其内容随计算后的 body 背景色更新。释放呈现器时，它会连同其他全局写入一起移除自己的元数据节点。

-----

<a id="understand-the-implementation"></a>
## 理解实现

<details>
<summary>实现细节——点击展开</summary>

一次 `register()` 调用把 `AppFrame` 贡献进运行时的内建 `'root'` 槽位，并在同一刻声明四个子槽位（`shell.access`、`sidebar`、`conversation`、`details`、`shell.overlay`）、安放布局 store（面板几何）并接好 `ctx.layout` 面板动作服务。布局 store 以默认宽度启动侧栏、保持详情栏关闭。Workbench 只在 `weave.workbench.detailsWidth` 下保存用户拖动的现场宽度，存储失败不影响当前布局使用。AppFrame 始终挂载会话与详情两栏；已连接 Session 经 `SessionProvider` 渲染。它把所选 Session 标题投影到构建配置的产品标题或本地化 `common.brand.localBuild` 回退值之上，因此 locale revision 会随根 entry 一起更新文档元数据。主题呈现器是第二个 effect：从解析后的快照做纯 DOM 写入——初始状态经 getter 读取一次，此后仅事件驱动，不经过 React。它先应用调色板、字号与 token 变量，再把渲染出的背景测量为唯一的颜色依据。

</details>

-----

<a id="further-exploration"></a>
## 进一步探索

当布局面不够用时阅读以下页面。它们从框架进入它所渲染的栏与它所呈现的主题。

- [ui-sidebar](../ui-sidebar/README.zh.md)——占据 `sidebar` 栏及其座位。
- [ui-conversation](../ui-conversation/README.zh.md)——占据 `conversation` 与 `details` 栏。
- [ui-theme](../ui-theme/README.zh.md)——呈现器消费其解析快照的主题 seam。
- [Web 客户端架构](../../../.agents/notes/implemented/architecture/2026-07-19-gui-web-client-architecture.zh.md)——浏览器插件行如何加载并注册槽位。

-----

<a id="model-experience"></a>
## 模型体验

无。布局外壳管理浏览器查看状态；这里没有任何内容进入模型请求。

#### KV Cache 影响

无；该包既不组装也不发送提供方请求。

## 已知限制与延期工作

<a id="known-limitations-and-deferred-work"></a>


这些限制界定了当前布局行为。它们是当前包约束，不是通用窗口管理器对比或任务积压。

- **打开状态是瞬时的**——重新加载或切换到其他 Session 会关闭详情栏。Workbench 保留现场偏好宽度，其他 profile 重置宽度。未选中表面以零宽度渲染详情栏，不修改几何。
- **让步链自动关闭通过推导零宽度实现，不触碰偏好宽度**——窗口变宽时面板自行恢复；消费方不得把 store 中的详情宽度当作渲染真值。
- **挤压重排期间无滚动锚定**——布局变化可能移动读者的视口。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护者的工作上下文——点击展开</summary>

无。

</details>
