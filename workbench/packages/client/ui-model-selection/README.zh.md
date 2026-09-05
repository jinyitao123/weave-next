---
description: "Web GUI 的模型选择：/model 弹窗与 composer 模型位共用一份按提供方分组的会话级目录；供模型路由的用户与维护者阅读。"
kind: "package-reference"
---

# @deepseek-ai/dsh-client-ui-model-selection

[English](README.md) | 中文

## 概述

本包提供 Web GUI 的模型选择：`/model` 弹窗命令与 composer 模型位，两者共用一份按提供方分组的会话级目录。选择模型会提交完整选择——提供方、模型与推理强度——宿主在下一次提示词组装边界对其快照，因此后续请求采用该选择，而运行中的步骤保留已组装选择。composer 位打开紧凑的模型与强度弹层：当前模型可以进入按提供方分组的模型列表，所选具体模型则提供离散强度滑杆、适配器持有的强度名称与默认值。当宿主报告没有适配器服务该会话的路由时，composer 输入停用，直到路由恢复可用。

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

与 `ui-conversation` 及命令包一起挂载本插件；composer 随即在待处理指示器旁显示模型位，`/model` 则以弹窗打开同一份目录。当确切提供方／模型对仍在已公布分组中时，两个表面都显示宿主报告的当前选择；目录行缺席时，可路由的选择保持不变，触发器显示其提供方／模型标识。

### 模型与推理强度

模型按提供方分组。紧凑概览把当前模型、本地化后的标准强度名称与离散强度滑杆放在一起；选择模型摘要会进入按提供方分组的列表，并提供明确的返回操作。两个层级使用同一宽度，选择模型后会返回该模型的强度概览，不会关闭卡片。滑杆刻度与所选模型公布的强度逐一对应，原生 range 控件保留方向键与辅助技术操作。滑杆会先在本地跟随指针移动，并在松开时提交所选等级，避免短暂进入禁用态产生闪烁。`/model` 弹窗应用所选模型的默认推理强度；composer 随后可以在不关闭弹层的情况下选择任一已公布强度。适配器没有推理元数据时不显示滑杆；不存在任意推理强度输入。

### 不可路由的会话

当宿主报告没有适配器服务该会话的路由时，触发器显示“尚未选择可用模型”，点击直接进入模型列表。composer 阻止提交并保留草稿，模型选择入口仍可操作。空列表指引用户打开侧栏设置的“模型”页。保存设置、更新适配器或更改凭据引用后，共享目录刷新并重新核验当前路由。仍不可用的持久选择须由用户明确替换；恢复后无需刷新页面即清除输入阻断。首次加载之前或加载失败之后的 `null` 绝不阻断；只要宿主可以服务该路由，未出现在目录中也不会导致阻断。

-----

<a id="understand-the-implementation"></a>
## 理解实现

<details>
<summary>实现细节——点击展开</summary>

`/model` 弹窗与 composer 模型位共享各会话的 `ModelDirectory`，由它组合宿主代次目录与持久选择投影。模型选择通过 `session.selectModel` 提交；设置、适配器和凭据变化使共享目录失效，各常驻目录随之重新计算路由可用性。目录刷新通过代次隔离旧响应，选择投影在重连时保留会话选择。目录按会话惰性解析，随会话作用域一并释放；已寻址 subagent 会话不公开任一入口。

</details>

-----

<a id="further-exploration"></a>
## 进一步探索

当模型面不够用时阅读以下页面。它们从浏览器表面进入命令弹窗外壳与选择约定。

- [ui-commands](../ui-commands/README.zh.md)——`/model` 贡献项注册进的 popupSelect 外壳。
- [ui-conversation](../ui-conversation/README.zh.md)——声明 composer 的 `conversation.input.model` 位与 composer 阻塞块。
- [dsh-agent-default-model](../../core/agent-default-model/README.zh.md)——为从未选择的会话提供默认模型的默认模型服务。
- [客户端包映射](../README.zh.md)——相邻的浏览器 UI 包。

-----

<a id="model-experience"></a>
## 模型体验

间接影响。两个入口都提交 `session.selectModel` 选择；宿主在下一次提示词组装边界对完整 `ModelSelection` 快照并拥有模型可见效果，而运行中的步骤保留已组装选择。

#### KV Cache 影响

切换路由可能减少提供方侧后续请求的缓存复用，或使其失效；提示词前缀本身不受影响。

## 已知限制与延期工作

<a id="known-limitations-and-deferred-work"></a>


这些限制界定了当前模型表面。它们是当前包约束，不是通用模型路由器对比或任务积压。

- **无创建期或已寻址 subagent 选择**——两个入口都要求既有普通会话的 agent；没有可纳入会话创建的草稿阶段模型选择，subagent 继续执行也有意不公开独立的模型选择约定。
- **设置导航仍在侧栏** — 空模型列表提供配置路径，不直接打开设置弹层。
- **目录名仅供呈现**——选择与持久化使用提供方／模型／推理强度 id；目录查询或确切模型元数据查询失败的提供方以不可选失败行列出，重新加载前保持原样。
- **不能任意输入推理强度**——composer 仅提供确切模型由适配器公布的推理强度；适配器没有推理元数据时不显示 Effort 行。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护者的工作上下文——点击展开</summary>

无。

</details>
