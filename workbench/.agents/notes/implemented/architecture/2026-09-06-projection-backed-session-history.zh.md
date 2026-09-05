# Agent Note：由投影承载的 Session 历史

Status: implemented

[English](2026-09-06-projection-backed-session-history.md) | 中文

## Problem

一个 Session 可能只有少量对话消息，后面却跟着数千条完整的后台快照。按消息数分页会把这些快照全部装入开场帧。实测一条 Workbench Session 的开场帧约为 1.99 亿字符，页面上的对话却很短。重复状态也放大了浏览器内存与传输采集开销。

减少新写入不能消除既有历史。截取事件尾部可能把对话藏在许多快照页之前，而任意删去事件会破坏日志的序号连续性。当前领域投影已经承载 Client 使用的完整状态，但载体仍需要显式规则，说明哪些历史事件可以由该状态表示。

## Decision

Session Controller 拥有可撤销的 `registerHistoryProjection` 贡献。领域声明自己的 Client 投影 key 与仅记入日志的快照事件类型，并保证该投影保留这些快照中的全部 Client 可见事实。对话、工具和操作回执事件保持原样。Controller 不包含 Workbench 专用事件清单；Workbench 仅在 `workTask` 下声明 `weave/work-task`。

精确的 follow 开场页携带权威投影 baseline，把每段连续声明快照替换为一条 `history/projection` 记录。记录保留首序号与首时间戳、投影 key，以及包含端点的末序号。Surface 记录和未声明事件会打断范围，并完整保留。实时 follow 事件保持原样。Persistence、重放与原始日志导出不变。

Gateway 接受连续页之前，Client 按开场投影 keys 与 watermark 校验每条范围。只有 Gateway 发布开场替换后，Client 才提交投影回执。旧页和缺口修复请求携带该回执；Host 仅折叠其游标所覆盖的声明快照。超过已接受 watermark 的事件保持原样。不带回执的分页返回原始事件，以及已有的无损 Assistant chunk 压缩记录。

本决策部分取代[Session 历史与事件传输](2026-08-18-session-history-and-event-transport.zh.md)中所有历史均按原始事件传输的表述，保留其激活规则、原始实时日志、游标连续性与 Gateway 职责。它落实了[Session 观察与投影拥有的 Client 状态](2026-08-25-session-observations-and-projection-owned-client-state.zh.md)中的权威投影规则。两项旧决策均保持有效。

Workbench 轮询器按字段值比较状态，不受对象属性顺序影响。执行尝试没有变化时保留上次变化时间，重复观察同一运行不会通过嵌套时间戳制造业务变化。业务变化立即追加；没有变化的观察保留现有的 30 秒新鲜度刷新。这减少后续快照增长，历史表示则处理已存在的日志。

## Alternatives considered

- 按字节限制原始尾页能够保留事件，但可能让初始对话为空，用户要经过许多只有快照的页面才能看到消息。
- 删除快照却不提供显式范围，会破坏日志连续性，让重连修复变得含混。
- 通用 JSON 差分能够保留每个历史值，但会为已经由投影拥有的事实增加编解码协议与 Client 重建工作。
- 重写持久化历史会改变审计和重放事实，不属于本次读取路径修复。

## Consequences

每段连续声明快照最多对应一条记录，所以单纯增加该段长度不会在开场负载中重复其状态。这不是通用字节上限；大投影、工具输出、消息或未声明事件仍可能产生大页面。原始大日志也仍然占用 Host 存储和观察成本。

领域注册承担语义义务。未来 Client 若需要投影中没有的历史事实，就必须保留相关事件或增加明确的历史读取方式，不能把它们声明为可省略快照。投影 key 不可用时禁用折叠；撤销声明或投影后恢复原始历史读取。

## Testing

Controller 测试覆盖 12,000 条大快照且完整 UTF-8 开场页小于 52 KB、用户与助手消息保留、原始工具元数据与操作记录、当前投影状态、原始实时续接、源事件不变、分页回执、范围边界、投影不可用及注册撤销。Client adapter 测试覆盖连续投影范围、实时追加、旧页回执，以及拒绝超出已接受 baseline 的范围。

Workbench 轮询回归覆盖连续 60 次观察不增加业务快照、精确的 30 秒新鲜度刷新，以及进度变化立即保存且保留执行尝试创建时间。
