# base 词表与参与方合同

本文只描述 `internal/base` 已有代码合同。九个词是架构词表，不要求九个同名 Go 类型；其中 Participant 是对现有 Worker、Actor、Executor 身份的归并名称。

## 九个概念

| 概念 | 代码事实 | 边界 |
| --- | --- | --- |
| TeamRun | `internal/base/teamrun/types.go` 的 `TeamRun` 是一次团队工作流运行的持久状态，身份由 `WorkspaceID`、`RunID`、`TeamID`、`WorkflowID`、`WorkflowVersion`、`RunSnapshotID` 组成。`Status` 覆盖 queued、running、parked、cancel_requested 及四种终态；`Transition` 保存每次状态迁移的 generation、lease epoch、actor、source 和 idempotency key。 | TeamRun 不是 workflow 定义，也不是队列任务。它拥有运行状态机；定义内容由 Snapshot 冻结，调度入口由 Task 承载。 |
| Task | `internal/base/taskqueue/types.go` 的 `Task` 是可领取的持久工作单元。`IdentityKind` 区分 agent、team_workflow、audit，`ClaimFilter` 限定 worker 可领取的分区；`WorkerID` 与 `LeaseExpiresAt` 记录当前领取事实。 | Task 表示“要做的一份工作”，不等于 TeamRun。一个 TeamRun 可由 root、fanout leg、resume 等多个 Task 推进；Task 的 terminal 也不能替代 TeamRun terminal。 |
| Event | base 有三种明确事件形态。`internal/base/teamrun/types.go` 的 `Transition` 是 TeamRun 持久状态事件；`internal/base/fanout/workflow_types.go` 的 `AuditEvent` 是 fanout 持久审计事件；`internal/base/realtime/hub.go` 的 `Event` 是面向在线连接的进程内通知。 | 只有已持久化的 Transition/AuditEvent 能作为恢复事实。`realtime.Hub.Publish` 明确允许慢订阅者丢事件，因此 realtime Event 不能作为终态或恢复的唯一证据。 |
| Deliverable | `internal/base/deliverable/store.go` 的 `WorkflowOutput` 是工作流节点输出的写入请求，`FinalDeliverable` 是不可变、用户可见的最终交付记录，带 run、snapshot、conversation、event 身份。 | 普通中间输出不自动等于最终交付物；`WorkflowOutput.Final` 与持久化投影决定其是否进入用户可见 ledger。Deliverable 不承担运行状态迁移。 |
| Participant | 代码中没有新的 `Participant` struct。它归并三种已有执行身份：`taskqueue.Task.WorkerID` 的 Worker、`teamrun.Transition.Actor` 及各迁移请求的 Actor、`teamrun.Executor` 的 Executor。Agent 的冻结执行身份另由 `internal/base/execution/scope.go` 的 `AgentExecutionStamp` 表达。 | Worker、Actor、Executor 不再作为三个架构概念扩张。它们是 Participant 在领取、记账、执行三个位置上的角色名；具体字段和 fencing 规则保持原样。 |
| Lease | `taskqueue.Task.WorkerID/LeaseExpiresAt` 是队列领取租约；`teamrun.TeamRun.ExecutionLeaseEpoch` 与 `CurrentExecutorID` 是运行执行租约；`fanout.CreatorLeaseIdentity`、`CreatorLeaseState` 是 fanout 创建者租约视图。 | CreatorLease 归并为 Lease，不另立一级概念。Lease 必须带可比较身份或 epoch，用来拒绝旧 worker、旧 attempt 和重复 resume；它不是 Participant 的永久所有权。 |
| Checkpoint | `internal/base/teamrun/checkpoint.go` 的 `WorkflowCheckpointV1` 保存 stamp、run generation、execution lease epoch、当前 node、已完成输出和 usage；`fanout.YieldedCheckpoint` 把 fanout wait payload 与 checkpoint sequence 绑定。 | Checkpoint 是可恢复进度，不是配置快照，也不是终态。其 generation/epoch 不能领先当前 TeamRun，恢复时还必须与 Snapshot 身份一致。 |
| Snapshot | `internal/base/snapshot/store.go` 的 `TeamRunSnapshot` 以 run ID 冻结 team、workflow、worker versions、artifact、dependencies、runtime assignment 和 trigger source；`fanout.Snapshot`/`LegSnapshot` 是 fanout group 的读取投影。 | TeamRunSnapshot 是 create-only 的执行输入事实，不能被后续 live registry 读取替代。fanout Snapshot 是组状态投影，二者共享“某时点事实”语义但不互换。 |
| Fanout | `internal/base/fanout/workflow_types.go` 的 `WorkflowGroup`、`WorkflowLegPlan`、`GroupCompletion` 描述并行组、分支和聚合完成；`FanoutCoordinator` 及 teamrun 的 `ExecutorFanout` 是执行接缝。 | `JoinPolicy`、`ParkIntent`、`ResumeClaim` 全部是 Fanout 子概念。JoinPolicy 决定聚合，ParkIntent 固化 park/activation 身份，ResumeClaim 对 completion 后的恢复做 fencing；三者不得升格为独立顶层概念。 |

## 归并规则

1. CreatorLease 统一写作 Lease。代码名 `CreatorLeaseIdentity`、`CreatorLeaseState` 暂不改名，它们是 fanout 场景下的 Lease 载体。
2. JoinPolicy、ParkIntent、ResumeClaim 统一放在 Fanout 下。它们分别对应 `JoinPolicy`、`ParkIntent`、`ResumeClaim`，不能绕过 `WorkflowGroup.Generation`、`GroupCompletionID` 和 claim identity 单独推进状态。
3. Worker、Actor、Executor 统一写作 Participant。代码字段继续使用既有名字，避免迁移阶段改 ABI；文档和新增设计不得把三者包装成新的平行概念。

## 参与方合同

任何承担执行职责的 Participant 都必须同时满足下面三项。

### 接任务

- 任务入口是 `taskqueue.Store.Claim(ctx, workerID, ClaimFilter)`，合同面是 `teamrun.ExecutorTaskStore.Claim`。Participant 必须用稳定、非空的 worker identity 领取匹配分区的 Task。
- 领取后只有同一 `WorkerID` 能调用 `Heartbeat`、`CompleteClaimed`、`FailClaimed`。`Task.LeaseExpiresAt` 到期或 heartbeat 丢失后，旧 Participant 不得再提交结果。
- TeamRun 的执行还受 `ExecutionLeaseEpoch` 和 `CurrentExecutorID` fencing。`ClaimRequest`、`ReclaimRequest`、`ResumeRequest` 都携带期望 generation/epoch；队列 lease 成功不代表自动拥有 TeamRun lease。

### 发事件

- 每次 TeamRun 状态变化必须形成 `teamrun.Transition`，至少带 `Actor`、`Source`、`IdempotencyKey`、`OccurredAt` 及三个 generation/epoch 字段。
- fanout 的 prepare、activation、leg terminal、join、resume 必须留下 `fanout.AuditEvent` 或等价的持久 group/claim 更新，不能只发布 realtime 消息。
- `realtime.Event` 只能在持久事实之后用于在线通知。因为 `Hub.Publish` 在订阅缓冲满时会丢弃事件，消费者必须能通过 TeamRun、Task、Checkpoint、Snapshot 或 fanout store 重建当前状态。

### 终局必达

- Participant 领取 Task 后，必须通过 `CompleteClaimed` 或 `FailClaimed` 收束，或由取消/超时/裁剪状态机推进到 `Task.IsTerminal()` 承认的 terminal；不能遗留无租约的 running Task。
- TeamRun 必须经 `SucceedTx`、`FailTx`、取消确认或不可恢复 abandon 进入 `Status.Terminal()` 承认的 succeeded、failed、cancelled、abandoned 之一。parked 只是可恢复中间态。
- fanout leg 必须进入 `LegTerminal` 的 succeeded、failed、timeout、cut、cancelled、abandoned 之一；group 必须产生稳定 `GroupCompletionID` 后才能 claim/admit/advance resume。
- realtime `done`、日志文本和内存中的函数返回都不是终局证据。终局是否到达，只以持久 Task、TeamRun、fanout completion/claim 与必要的 terminal marker 为准。

## 词表守门

base 新代码若无法归入上述九词，应先修改本文并做架构裁决。仅新增字段或场景子类型，不自动构成新概念；尤其不得重新引入 CreatorLease、JoinPolicy、ParkIntent、ResumeClaim、Worker、Actor、Executor 作为顶层词。
