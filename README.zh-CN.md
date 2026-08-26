# Weave

<p align="center">
  <a href="README.md">English</a> | <a href="README.zh-CN.md">中文</a>
</p>

**给一个业务团队多个可用运行时，和一条清晰边界，你才敢让它实际产出。**

Weave 是 [Loom](https://github.com/jinyitao123/loom) 的平台层。Loom 造一个能动手的心智；Weave 把它组织成元团队、业务团队和多运行时执行面。

## 一句话立意

所有把事交给别人的关系，都压在一条耦合上：**谁有权力去做，谁就担这件事的后果。** 你把活交给一个同事，他和你共担下行——他的名声、他的饭碗。正是这份共担，让托付是安全的。

AI agent 掐断了这条耦合。它会动手——发邮件、动库存、调工具——但它不担你的任何风险。它激励不动、惩罚不到、对结果毫无利害。所以对人管用的两把锁——**对齐它的利益**、**信任它的判断**——两把都失效了。你交出去的是**劳动**，**风险却百分之百留在你这**。

风险搬不动，就只剩最后一把锁，而且是结构性的：**在动作变得不可逆的那些点上，把 agent 的权力从结构上划死界，并让每一次跨越都看得见。** 这道边界——可逆的计算在哪里变成不可逆的后果——就是 Weave 存在的全部理由。

所以 Weave 优化的是一个大多数 agent 平台连名字都没给的东西:**可托付性**。不是一个 agent 能有多自主，而是在你不盯着的时候，你能多安心地把活交给它。

让一个 agent 可托付，要两样东西:

- **确定性**——它的动作可预测、可回放。你没法治理一个你既预见不到、也重建不出的东西。*这是 Loom 的 graph 给的:一个 agent 是一张 JSON 步骤图，不是一个松散的 prompt 循环。graph 定死了先干什么后干什么;skill 只描述「什么算好」——只管* what*，从不管* how。*
- **一条写边界**——业务团队默认只产出可检查的文件、代码、报告和素材；外部不可逆写动作没有明确产品路径就 fail-closed。

**确定性 + 闸 = 一个你真能托付的东西。** Loom 让手稳，Weave 决定这只稳手的哪些动作能够抵达世界。一只你治不住的稳手仍是隐患;一道架在乱抖的手上的闸，没有可攥住的实物。两个都要。

## Loom 与 Weave

|  | Loom | Weave |
|---|---|---|
| **是** | 内核——一个能动手的心智 | 平台——那个心智触碰世界的、被治理的表面 |
| **管的** | agent 的*内部*(它如何思考与执行) | agent 的*组织和运行*(团队、运行时、工具边界) |
| **给你** | 确定性执行:agent 即数据，一张你读得懂的图 | 可运行的元团队、业务团队和多运行时 |
| **意象** | 织布的织机 | 经纬——经线是担着险的人，纬线是不担险、来回穿的 agent，综片决定哪根线能穿过 |

一个 agent 自己只是计算。Weave 给它团队身份、运行时选择和工具边界，让它能在业务流程里持续产出。

## 具体说，Weave 是什么

整套设计要守住的三个属性:

- **常驻**——agent 作为长期运行时活着(服务器、用户自己的机器、或边缘一体机),不是一次性调用。活可以在你睡觉时进来。
- **可寻址**——你像找同事一样找它:一个聊天、一个信箱。它记得你。
- **可运行**——业务团队可以稳定接收输入、执行流程并生成可检查的业务产物。

```
┌─────────────────────────────────────────────┐
│  Weave Console (React 19 + Vite)            │  ← 浏览器 UI
├─────────────────────────────────────────────┤
│  Weave API (Echo)                           │  ← REST + SSE
│  ┌──────────────┬──────────┬─────────────┐  │
│  │ Write Gate   │ Registry │ Memory      │  │  ← fail-closed writes
│  │ Run Records  │ Compiler │ LLM Router  │  │
│  └──────┬───────┴────┬─────┴──────┬──────┘  │
├─────────┼────────────┼────────────┼──────────┤
│   Loom 内核        Store (PGStore)  MCP Host │  ← 确定性执行
└─────────────────────────────────────────────┘
   PostgreSQL 16+          MCP Servers (HTTP)
```

## 箱子里有什么

**内核——让一个 agent 可托付的那部分**

| 能力 | 实现方式 |
|---|---|
| **写动作边界** | 不可逆工具调用默认拒绝；需要落地时必须变成明确产品流程。 |
| **运行记录** | 保留运行历史、SSE done 事件里的 `runtime_info` 与可定位的产物。 |
| **确定性执行** | agent 编译成 Loom Graph。声明式 JSON graph 表达多步业务流(6 种步骤类型:chat、llm_call、llm_check、yield、transform、builtin)。 |
| **护栏 & 预算** | 按 agent 的屏蔽词检查;按 agent 的 USD / token / step / 输出上限，带兜底模型。 |
| **交接 vs 请教** | 两个协同动词，天生带人参与:*交接*把任务丢给你(以你的结果为准);*请教*问下属、把结果汇总回来，由主导者拍板。 |

**常驻 & 可寻址**

| 能力 | 实现方式 |
|---|---|
| **常驻 & 多运行时** | agent 跑在服务器、用户自己的机器、或边缘侧——各自通过 API 领活。 |
| **对话 & 恢复** | `/v1/chat` 运行一张 Loom Graph;`/v1/resume` 从 yield 断点继续。 |
| **SSE 流式** | 实时 token 推送，自动拦截结构化内容块(chart / mermaid / SVG)。 |

**平台服务**

| 能力 | 实现方式 |
|---|---|
| **LLM 路由** | 运行时可配置的多 Provider(OpenAI、DeepSeek、Gemini)——热切换无需重启。 |
| **向量记忆** | 基于 pgvector 的语义记忆:自动提取、去重、召回。 |
| **技能系统** | 可复用的 prompt 模块，渐进式披露(SemanticMatcher + KeywordMatcher)。 |
| **MCP 集成** | 组合式 HTTP MCP Host，支持按工具名过滤、按服务器自定义 headers。 |
| **多租户** | HS256 JWT token 租户隔离。 |
| **控制台** | 随 Weave 内嵌的 React 19 界面：工作台/对话、Agent 配置（含记忆条目与技能正文）、团队、运行/最新存档、运行时与设置。 |

## 快速开始

### Docker Compose（推荐）

```bash
cd weave

# 配置环境变量
cp deploy/.env.example deploy/.env
# 编辑 deploy/.env:
#   JWT_SECRET=$(openssl rand -hex 32)

# 构建并启动
docker compose up -d --build

# 检查
docker compose ps
docker compose logs -f weave
```

启动后：

| 服务 | 地址 | 说明 |
|---|---|---|
| **Weave + 控制台** | http://localhost:8080 | 同源管理界面、REST 与 SSE |
| **PostgreSQL** | localhost:5432 | `weave/weave` |

默认登录：`admin` / `admin123`

`make smoke` 用真实 Weave 进程 + PostgreSQL + 本地假 LLM 端到端自检一遍。

### 配置 LLM Provider

```bash
# 取认证 token
TOKEN=$(curl -s http://localhost:8080/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}' | jq -r .token)

# 加一个 OpenAI 兼容 provider
curl -X POST http://localhost:8080/v1/providers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "deepseek",
    "name": "DeepSeek",
    "base_url": "https://api.deepseek.com",
    "api_key": "sk-...",
    "models": ["deepseek-chat"]
  }'
```

### 创建 Agent

```bash
curl -X POST http://localhost:8080/v1/agents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "assistant",
    "model": "deepseek-chat",
    "spec": { "identity": { "core": "You are a helpful assistant." } },
    "guard": { "enabled": true, "blocked_terms": ["password", "secret"] },
    "max_tokens": 4000
  }'
```

### 对话

```bash
curl -X POST http://localhost:8080/v1/chat \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "agent": "assistant", "message": "你好！", "stream": true }'
```

### CLI 与 MCP 客户端

`weave` 二进制可直接调用已经运行的 Weave 服务，不会加载服务端配置，也不会连接
PostgreSQL：

```bash
export WEAVE_API_URL=http://127.0.0.1:8080 # 默认值
export WEAVE_API_KEY=wv_sk_xxx

weave team samples
weave team up -f templates/code-review.yaml \
  --idempotency-key 2bab1728-9dc0-4b61-a1f5-115fcaaf8e08
weave team dispatch --team <team-id> --task "审查这次变更" \
  --client-request-id 676506ec-1cf5-4c78-b324-29057591d3b5 --wait
weave status build <build-id>
weave status dispatch <client-request-id>
weave status team-run <run-snapshot-id>
weave deliverable list
weave deliverable get <deliverable-id>
```

建队所用 API key 的角色必须是 `admin`。模板、团队和 build 状态需要 `org`
scope；派活、resume 和交付物需要 `chat` scope；team-run 状态需要 `runs`
scope。`team_create` 不会代替调用方生成幂等键。同一次 dispatch 必须始终使用
同一把 API key，因为请求归属与 key 绑定。CLI runtime 承载的 Lead 不接受异步
dispatch。只有 Agent 显式调用 `save_deliverable` 才会产生交付物；v1 的交付物
列表不支持按 team 或 run 过滤。

Codex 从 `~/.codex/config.toml` 或受信任项目的 `.codex/config.toml` 读取本地
stdio MCP server：

```toml
[mcp_servers.weave]
command = "/absolute/path/to/weave"
args = ["mcp", "serve"]
env = { WEAVE_API_URL = "http://127.0.0.1:8080" }
env_vars = ["WEAVE_API_KEY"]
```

启动 Codex 前先 export `WEAVE_API_KEY`。Claude Code 可用同一个进程注册：

```bash
claude mcp add --transport stdio \
  --env WEAVE_API_URL=http://127.0.0.1:8080 \
  --env WEAVE_API_KEY="$WEAVE_API_KEY" \
  weave -- /absolute/path/to/weave mcp serve
```

## API 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/v1/auth/login` | 用户名 / 密码登录 |
| `GET` | `/v1/health` | 健康检查 |
| `GET/POST/PUT/DELETE` | `/v1/agents` | Agent 增删改查 |
| `GET` | `/v1/agents/:name/topology` | 获取编译后的图拓扑 |
| `POST` | `/v1/chat` | 对话（支持 `stream: true`、`profile`、`context`） |
| `POST` | `/v1/resume` | 恢复被暂停的 Graph |
| `GET/DELETE` | `/v1/sessions` | 会话管理 |
| `GET` | `/v1/runs` | 运行历史与追踪 |
| `GET` | `/v1/usage` | Token & 费用统计 |
| `GET/POST/PUT/DELETE` | `/v1/providers` | LLM Provider 配置 |
| `GET/PUT` | `/v1/settings/embedder` | Embedding Provider 配置 |
| `GET/POST/PUT/DELETE` | `/v1/skills` | 技能增删改查 |
| `GET/POST/DELETE` | `/v1/agents/:name/memories` | Agent 记忆增删查 |
| `POST` | `/v1/agents/:name/memories/search` | 语义记忆搜索 |

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `DATABASE_URL` | 是 | — | PostgreSQL 连接字符串 |
| `JWT_SECRET` | 是 | — | HS256 签名密钥 |
| `PORT` | 否 | `8080` | HTTP 监听端口 |
| `WEAVE_DEV_MODE` | 否 | `false` | 开发端点开关（/v1/auth/token） |

LLM Provider 和 Embedder 在运行时通过 API 配置，不走环境变量。

## 与 Loom 的关系

Weave 严格遵守 Loom 的分层规则：

- Weave 是 **Layer 3**——只 import Loom，绝不修改 Loom。
- 平台功能（记忆、团队、运行时、工具边界、路由）全在 `weave/internal/` 里，不在 `loom/` 里。
- 扩展通过 `PGStore.Pool()` 直接访问连接池——不给内核加方法。

一句话:内核让 agent 动手动得稳，平台负责把这些能力组织成可运行的团队和可控的工具边界。

## License

Apache-2.0
