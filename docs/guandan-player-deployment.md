# 掼蛋 Loom 玩家部署与验证

## 执行边界

演示服务只向 Weave 提交冻结的本座手牌、公开事实和合法候选。专用服务密钥只具有 `game_decisions` scope，并绑定一个 Team / Workflow 的明确发布版本。

玩家必须为 `engine=loom`，模型为 `deepseek-v4-flash`。Loom graph 在 Weave 服务端进程执行，直接使用已冻结的 DeepSeek provider；不配置 `runtime_id`，不启动 Codex / Claude / OpenCode CLI runtime。玩家没有 MCP、技能、记忆、模型 fallback 或外部 delivery target。`tool_loop_control={slice_rounds:1,initial_total_rounds:1}` 选择现有 durable standard v2 member runner。

## 最小部署顺序

1. 从本分支的已验证 commit 构建镜像或 `cmd/weave`，不要传输 dirty checkout。沿用项目的 `docker-compose.platform.yml` 与 PostgreSQL 部署。启动会运行迁移，包括新增 `0135_game_decision_admissions.sql`、`0136_game_decision_cancellations.sql`；Loom 持久 member 表使用此前已有迁移。
2. 为正式服务保留稳定的 `WEAVE_SECRET_KEY` 与 `JWT_SECRET`，由部署机受保护配置注入。不要照搬本地开发服务随机内存密钥。已有 provider 密文依赖原 `WEAVE_SECRET_KEY`，不得更换后继续使用旧密文。
3. 管理员通过 `POST /v1/providers` 注册 workspace provider。字段为 `id=guandan-deepseek`、`base_url=https://api.deepseek.com`、`models=[deepseek-v4-flash]`、`json_object_mode=true`，API key 只从受保护配置读取到请求内存。凭据由 Weave 加密持久化，不能出现在代码、命令参数、日志或证据文件中。
4. 在已安装 Node.js 的操作端运行 `scripts/guandan-player-setup.mjs`。通过受保护环境提供 `GUANDAN_WEAVE_URL` 和管理员 `WEAVE_API_KEY`。该脚本只使用公开管理 API，创建 / 检查 Loom agent、team 和已发布决策 workflow，输出不含密钥的绑定 ID。只有本机开发模式允许额外设置 `GUANDAN_WEAVE_DEV_AUTH=1` 使用 loopback token；正式部署不能依赖它。
5. 管理员创建一个 `role=service`、`scopes=[game_decisions]` 的 API key，并用 `PUT /v1/game-decision-bindings/:key_id` 绑定脚本输出的 `team_id`、`workflow_id`、`workflow_version`。将明文服务密钥只交付到演示服务的受保护环境。
6. 演示服务设置 `GAME_PLAYER_PROVIDER=weave`、`WEAVE_DECISION_URL`、`WEAVE_DECISION_API_KEY`、`GAME_AI_TIMEOUT_SECONDS=90`、`GAME_AI_FALLBACK=false`。跨主机使用稳定 HTTPS 地址。浏览器不会收到这两个服务端密钥。

脚本对已有的不同模型 / 引擎 / 缺失 durable 控制会停止，不会悄悄覆盖已发布配置。升级时明确修改 agent，创建 workflow draft、引用新 agent version、发布，再重绑后续决策。已经接收的请求保留原发布版本与输入哈希。

## 本地验证快照（2026-09-10）

- Weave `127.0.0.1:18089`，独立数据库 `weave_guandan_ai_20260910`。
- Team `fe68837d-8dec-4aae-acc1-c5ed291205d6`。
- Workflow `6bafee7d-55aa-41ba-95c7-92bf2414a1ab` version 3。
- Agent `7923f032-8831-49b3-92f4-d3229102d213` version 3，`engine=loom`，`model=deepseek-v4-flash`，无 CLI runtime。
- 真实 probe `run-84470c53-22a6-5092-baa2-490179d095d6`：冻结输入 seq 0，模型返回候选中的四张 3 炸弹，演示权威状态提交为 seq 1，总耗时 7534 ms，未启用规则 fallback。
- Durable member `9af29b1c-ee58-505e-b1a4-f7775706b670`：`checkpoint_seq=6`，`checkpoint:default:guandan-loom-player` 有 6 条不可变历史，`member-operation:default` 有 1 条 chat 操作记录。
- `member_completed` 记录 `tool_calls=0`、`input_tokens=1567`、`output_tokens=978`。合法候选决策无需工具调用；模型操作、检查点与终态均由 Loom member runner 管理。

上面的 ID 仅是本地证据，不可作为另一数据库的固定部署 ID。早期 Codex 调试 run 和默认内存 checkpoint 的 Loom v1 run 均不替代这次 durable Loom 证据。完整真人同桌和四 AI 对局由演示主流程另行验收，单手 probe 不代表完整对局完成。

## 故障与隔离

同房间只允许一个未终止的 Weave 决策；跨实例使用数据库 advisory lock。同一请求 ID 的输入不可更换。暂停、重开或回合变化后，演示服务取消原请求；取消 tombstone 会拒绝迟到 admission。只有确认旧执行终止后才允许显式规则 fallback，默认关闭。即使结果迟到，权威提交仍检查 round / seq / policy / state / candidate hashes。

普通对手视角保留运行与模型追踪，但不会收到 AI 对未公开手牌的解释；当前出牌者或自然完成后的复盘可见解释。模型调用成功不代表游戏规则通过，最终仍由演示权威运行时验证并提交。
