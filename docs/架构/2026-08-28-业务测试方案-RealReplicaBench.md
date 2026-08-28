# 业务测试方案：Weave × RealReplicaBench

> 日期：2026-08-28
> 状态：v1 方案（环境就绪后开工）
> 关联：[产品方案](./2026-08-25-Weave-产品方案-十分钟拉起一支业务团队.md)（终极验收指标：快速拉起团队、产出业务价值）

---

## 1. 为什么是它

RealReplicaBench（Accio/阿里国际，v1.3.1，107 任务）是"有状态的电商业务流基准"：任务在容器里跑，操作的是电商 SaaS 的本地复刻（商品发布、物流订舱、店铺运营、供应商分析……），**判定靠确定性状态校验或 LLM judge**，不是靠答案文本匹配。这与 weave 的哲学同构（机器校验优先），且是平台级考题：长程、多步骤、要真的改变世界状态。

107 任务 = 53 CLI + 28 browser + 16 file + 10 API/MCP；三个能力切片（65 纯文本 / 20 浏览器文本 / 22 视觉）。

## 2. 环境前提（硬阻断）

- RRB 需要 **Docker（linux/amd64 容器）**——本机没有 Docker CLI。需要一个有 Docker 的机器或 CI runner 才能开跑。
- 需要模型 API key（被测）+ LLM judge key。

## 3. 三级集成路线

### L0 对照组（先行，weave 不参与）

原版 OpenClaw harness + 模型直跑（OpenRouter/直连），先取 10 个纯文本 CLI 任务跑通流程。**目的不是成绩，是建立基线与排障**——证明 harness、镜像、judge 链路在本环境可用，并拿到"单 agent + 裸模型"的对照分数。

### L1 weave 团队作为被测智能体（核心实验）

RRB/OpenClaw 的模型接入点是 **OpenAI 兼容端点**（`base_url + model`）。由此得到一个干净的集成形态：

```
OpenClaw（容器内工具循环）──OpenAI chat/completions──▶ weave 适配端点 ──▶ weave 团队（team dispatch）
```

- weave 新增一个 **OpenAI 兼容适配端点**（app 带薄壳）：把 chat/completions 请求转为对指定团队的 dispatch（free-collab 或 published workflow），团队产出映射回 completion 响应；OpenClaw 不知道对面是一个团队；
- 被测团队按任务域用 **M1 模板快路径**拉起（例如"电商运营团队"：调研员+分析师+主笔），团队内部分工对 harness 透明；
- **这同时是对 M1/D5 的真实业务压力测试**。

### L2 RRB 作为 weave 评测的场景包（后期，最深）

把 RRB 任务定义（指令 + 初始状态 + verifier）转成 weave EvaluationContract 的场景，用 M2b 评测链路让团队在自己平台上接受评测、candidate 直接操作 mock 服务。这要求 candidate 执行环境能拉起 RRB mock 容器——工程量大，等 L1 有结论再做。

## 4. 度量

| 指标 | 来源 | 回答的问题 |
|---|---|---|
| Pass 率（对照官方 leaderboard） | RRB verifier | weave 团队做真实业务行不行 |
| **团队 vs 单 agent 差值** | L0 vs L1 同任务对比 | 平台的核心价值主张是否成立 |
| 每次通过的成本 | weave budget ledger（Phase 1/2 刚落地） | 团队模式的成本效率 |
| 完成时长 / 步数 | RRB trajectory | 效率 |
| 拉起团队耗时 | weave 侧计时 | M1 的产品承诺（<10 分钟） |

## 5. 执行步骤（环境就绪后）

1. Docker 环境准备（本机装 Docker Desktop 或找一台 Linux runner）；
2. L0：10 任务小切片跑通 + 基线分数；
3. weave 适配端点（一票小工程，app 带薄壳 + 映射测试）；
4. L1：同一切片跑 weave 团队，对比 L0；
5. 扩到 65 纯文本切片，出第一份正式报告；
6.（视结论）L2 场景包。

## 6. 风险

1. **对话形态约束**：RRB 是单会话工具循环，团队的内部协同价值被压缩到 lead 的决策里——如果 L1 对比 L0 无差，说明团队价值需要更长程的任务形态才能体现（这本身是有价值的结论）；
2. **延迟叠加**：团队 dispatch 的延迟叠进工具循环，可能撞 harness 超时——适配端点需要流式/快速响应策略；
3. **browser/vision 切片**：weave 团队的文本工具面不覆盖浏览器操作，首轮只做 text-only/CLI 切片，不硬上。
