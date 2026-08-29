# 业务测试总纲：Weave × RealReplicaBench

> 状态：**总纲（living document）——本仓文档惯例（日期快照制）的唯一例外**，方向变化就地更新，变更历史见 §7
> 创建：2026-08-28；最近更新：2026-08-28（L1 重定义）
> 关联：[产品方案](./2026-08-25-Weave-产品方案-十分钟拉起一支业务团队.md)（终极验收指标：快速拉起团队、产出业务价值）

---

## 1. 验证什么（本总纲的元问题）

weave 的产品主张是"快速拉起业务智能体团队、产出交付物"。因此业务测试的对象是**平台自己**：

- 团队执行基座（teamrun：dispatch/fanout/checkpoint/恢复）；
- 交付物闭环（产出 → 外部 verifier 判定状态改变）；
- 预算闸（Phase 1/2）在真实负载下的计量与切断；
- M1 模板快路径（按任务域快速组队）；
- D5 CLI/MCP 暴露面（第一个真实外部客户端）。

**反例（已否决，勿再提）**：把 weave 团队藏在 OpenAI 兼容端点后面、由 OpenClaw 在容器内执行工具循环。那个形态验证的只是"团队大脑 vs 单模型"的决策质量，平台的执行基座零上场——OpenClaw 拥有自己的执行循环，weave 沦为模型代理。（2026-08-28 用户质询"那我们验证什么呢"后否决。）

## 2. 基准资产：RealReplicaBench

Accio/阿里国际的有状态电商业务流基准（v1.3.1，107 任务：53 CLI + 28 browser + 16 file + 10 API/MCP；65 纯文本 / 20 浏览器文本 / 22 视觉切片）。任务在容器内操作电商 SaaS 的本地复刻，**判定靠确定性状态校验或 LLM judge**——与 weave 的机器校验哲学同构。

**关键资产**：RRB 仓内已有 `real_replica_bench/harnesses/weave/`（2026-08-17 建的 manual-UI harness）：RRB 管环境准备 + verifier，weave 管执行——分工正确，但中间段当时是"人在 Console 手工操作"。本总纲的核心就是把这个中间段自动化。

## 3. 三级路线

### L0 对照组（保留 OpenClaw 的唯一理由）

原版 OpenClaw harness + 裸模型直跑，10 个纯文本 CLI 任务小切片。目的：排障 + 建立"单 agent + 裸模型"基线分数 + 保持 leaderboard 可比性。**OpenClaw 的唯一角色是对照组执行体，永不做 weave 的执行体。**

### L1 weave harness 自动化（核心实验）

把既有 `harnesses/weave/` 从 manual-UI 升级为程序驱动：

```
RRB harness（容器环境 + verifier）
    │  prepare: 起 mock 服务、注入任务指令
    ▼
weave CLI/MCP（D5 交付物，第一个真实外部客户端）
    │  team up（M1 模板按任务域组队）→ dispatch → status → deliverable
    ▼
weave 团队（teamrun 全栈：自己的 runtime、自己的状态机）
    │  团队交付物落库
    ▼
RRB harness verify（确定性 verifier 判定状态改变，出分）
```

- harness 的 prepare/verify 原样复用（`runner.py` 已有骨架）；
- weave 侧零新业务逻辑——全部走 D5 公开面（CLI 优先，MCP 备选）；
- 被测团队按任务域用 M1 模板拉起，拉起耗时本身是被测指标。

### L2 RRB 场景包（后期）

RRB 任务定义（指令 + 初始状态 + verifier）转成 weave EvaluationContract 场景，进 M2b 评测链路。等 L1 有结论再启动。

## 4. 度量

| 指标 | 来源 | 回答的问题 |
|---|---|---|
| Pass 率 | RRB verifier | weave 团队做真实业务行不行 |
| 团队 vs 单 agent 差值 | L1 vs L0 同任务 | 平台核心价值主张是否成立 |
| 每次通过的成本 | weave budget ledger | 团队模式的成本效率 |
| 完成时长/步数 | RRB trajectory + weave run 记录 | 效率 |
| 拉起团队耗时 | weave 侧计时 | M1 的 <10 分钟承诺 |
| harness 驱动成功率 | D5 CLI 调用日志 | D5 暴露面的真实可用性 |

## 5. 执行步骤与环境

环境现状（2026-08-28）：Docker 29.7.2 ✅ 已装；RRB venv 已装（`real-replica-bench list` 可用）；基准镜像拉取中；**模型 API key 全部未配置**（judge key + 被测 key 需用户提供）。

1. 用户提供模型 key → L0 跑通 10 任务切片，拿基线；
2. L1 设计细化（harness 自动化的独立设计文档，**走完整互审再实施**——它将成为 codex 派工的事实来源）；
3. L1 实施：harness runner 自动化（prepare → D5 驱动 → verify）；
4. L1 同切片对比 L0，出第一份正式报告；
5. 扩到 65 纯文本切片；（视结论）L2。

## 6. 风险

1. **团队价值在单会话任务形态下可能不显现**——L1 对 L0 无差值本身就是有价值的结论，但要有心理预期；weave 原生执行（团队直接操作 mock 服务、不经 harness 的 agent loop）保留了团队协同的完整发挥空间，这是相对"OpenClaw 形态"的结构性优势；
2. **harness 自动化需要 weave 侧暴露的任务输入/交付物输出契约稳定**——D5 v1 的轮询形态延迟可能拉长任务时长；
3. **browser/vision 切片首轮不做**——weave 团队当前工具面不覆盖浏览器操作；
4. **模型 key 未到位前 L0 无法开跑**——这是当前唯一阻塞。

## 7. 变更日志

| 日期 | 变更 |
|---|---|
| 2026-08-28 | 初版（v1）：L1 为"OpenClaw + OpenAI 兼容端点"形态 |
| 2026-08-28 | **L1 重定义**：否决端点形态（验证对象错位——验证的是决策质量而非平台）；发现 `harnesses/weave/` 既有资产；L1 改为 harness 自动化（RRB 环境+verifier，D5 CLI/MCP 驱动 weave 团队全栈执行）；文档转为总纲制 |
