---
description: "Host 与 Client 会话控制：创建、恢复、提示、跟随历史并投影实时会话状态。"
kind: "package-reference"
---
# Session Controller

[English](README.md) | 中文

## 概述

`@deepseek-ai/dsh-api-session-controller` 拥有 Host 的 `ctx.sessionController` 服务，以及生成的 Client `session`、`skills` 和 `fileReferences` Remote namespace。它提供 Session 生命周期与历史、Host generation 模型目录、工作区路径打开、用户可调用 skill 发现，以及面向 Agent 的文件引用 adapter。当 Client 需要按 Session 寻址的操作时，请通过 API Gateway 使用它。

## 目录

- [使用本包](#use-this-package)
- [配置](#configuration)
- [模型体验](#model-experience)
- [已知限制与延期工作](#known-limitations-and-deferred-work)
- [开发备注](#dev-note)

-----

<a id="use-this-package"></a>
## 使用本包

历史页与 follow opening snapshot 携带使用 `{ type, event }` 的判别联合 `SessionHistoryRecord`。`type: 'event'` 携带原始 `SessionWireEvent`；`type: 'chunks'` 携带连续且属于同一 block 的 `assistant/chunk` delta 组成的无损 `ChunkRowEvent`。每种内部值都公开 `type`、`seq`、`time` 与 `data`，因此 Client 无需逐条转换就能保留已接受记录。packed event 的 `seq` 与 `time` 表示首成员；`data` 保留 fragment 与 timestamp-gap 数组。`type: 'projection'` 携带 `history/projection` 范围，包括首序号、首时间戳、投影 `key` 和包含端点的 `throughSeq`。实时 follow frame 仍是单个事件。工具参数、结果、失败信息和 `tool/result.data.meta` 原样通过；controller 不解析 Tool definition，也不运行 presenter。

`registerHistoryProjection(definition: SessionHistoryProjection)` 让领域声明投影 `key` 及其快照 `eventTypes`。领域保证投影保留这些快照中的全部 Client 可见事实；对话、工具和操作回执事件不属于候选范围。注册由调用方 fiber 拥有并返回 disposer。Follow 只折叠可用开场投影所覆盖、已经声明且不在 surface 上的快照。同一 key 的每段连续快照使用一条范围记录，不重写 persistence。Page 请求用包含已接受 follow 游标与 keys 的 `projectionBaseline` 回执选择折叠；超过该游标的事件仍保持原样。不带回执的请求返回原始事件与无损 chunk run。Client 仅在接受开场页时提交回执，校验范围覆盖关系，并在加载旧页与修复时携带回执。参见[投影承载历史决策](../../../.agents/notes/implemented/architecture/2026-09-06-projection-backed-session-history.zh.md)。

每个 endpoint 都声明自己的激活策略。列表、搜索、附件、历史页、日志跟随、skill 发现和工作区路径打开可以在不激活 Agent 的情况下检查 persistence；`canOpenWorkspacePath()` 无需指定 Session 即可报告原生打开能力。queue 变更与取消要求 live 状态；模型、重命名、prompt 和文件引用操作可以解析或恢复普通 Session。只有 create 与 fork 会直接创建新 Agent。skill 目录则优先使用已有 live Agent，否则使用所记录 preset 的常驻 scope，因此列表查询绝不会启动 Agent。

冷列表在投影声明需要列表读取、缓存状态版本不可用且 Session 工件未超过 `coldBlankProbeMaxBytes` 时补读投影。观察只读取已保存的工作，不激活 Agent。未变化的源文件复用进程本地观察；文件标识或投影版本变化会使其失效。超限或无法读取的记录通过 `projectionUnavailableKeys` 标明未知；已观察到的 null 仍是正常值。打开 Session 会提供完整历史投影。

Client adapter 提供 `SessionEventStream`，即绑定到一个普通 Session 或 direct subagent address 的 Gateway `RemoteJournalStream`。它在读取首个 page 前打开 follow，只发布连续的 `replace`、`prepend` 和 `append` 变更，并通过 tail page 修复重连或 seq 缺口。普通 record 覆盖 `[event.seq, event.seq]`，packed row 覆盖 `[event.seq, event.seq + memberCount - 1]`，投影范围覆盖 `[event.seq, event.data.throughSeq]`。业务、persistence 或无法恢复的连续性错误会终止 stream，只有物理载体断开才触发自动恢复。`SessionControlStream` 是 Gateway `RemoteSnapshotStream`；每代都以完整的进程本地 baseline 开始，因此重连会替换 queue、jobs 和 projection 状态，而不会把瞬态值当作 durable event。

Session 对象还承载本地提交回显：`session.beginSubmission` 在调用方序列化与 prompt 之前，同步把一条回显写入 `SessionSnapshot.pendingSubmissions`，会话 UI 因此能在点击提交的当帧显示消息。prompt 的 `requestId` 就是关联标识。Host 在普通 `{ kind: 'user' }` 来源，以及由 `origin: 'ui-control'` 选定的 UI 自动控制来源 `{ kind: 'plugin', plugin: 'ui-control', form: 'relay' }` 上，将它回显为 `rpcId`；两种来源都保留可选的、经 Host 校验的 `clientTimeZone`。queue occurrence 把该标识投影为 `SessionQueuedItem.rpcId`。回显在观察到其 durable event 或 queue occurrence 后延迟一个动画帧退休（该延迟保证 transcript 节点可渲染之前回显仍在），带标识的 prompt 失败或被放弃时立即退休，销毁时按 failed 退休；每次退休恰好触发一次注册的 `onRetire` 回调。回显只存在于 Client 内存，刷新与重连只从 durable event 重建会话。

-----

<a id="configuration"></a>
## 配置

| 字段 | 默认值 | 含义 |
|---|---:|---|
| `coldBlankProbeMaxBytes` | `1,024` | 可进行空白状态验证或指定投影恢复的冷 Session 工件最大物理大小；`0` 禁用探测 |
| `nativeOpen` | 平台探测 | 是否能把 Session 工作区路径交给原生桌面打开器 |

生成的[配置目录](../../../docs/config-catalog.zh.md#deepseek-aidsh-api-session-controller)是所有受支持字段及其 JSDoc 的完整来源。

-----

<a id="model-experience"></a>
## 模型体验

无，因为被调用的 Agent 命令拥有任何模型可见效果。

#### KV Cache 影响

无直接影响；模型请求仍由 Agent 和 LLM 包拥有。

## 已知限制与延期工作

<a id="known-limitations-and-deferred-work"></a>

- 投影折叠把每段连续声明快照限制为一条记录；它没有对投影、消息、工具输出或未注册历史设置通用字节上限。
- Control baseline 表示进程本地状态，因此 Host 重启后无法重建 jobs。
- follow 恢复失败会对调用方可见，而不会无限重试。
- 文件引用补全使用共享 Agent lookup，因此可能恢复冷 Session；`skills/list` 目录是不激活 Agent 的 skill 元数据读取路径。


<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护者工作上下文——点击展开</summary>

无。

</details>
