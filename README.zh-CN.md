# Weave

[English](README.md) | [中文](README.zh-CN.md)

**[Weave Workbench](https://github.com/jinyitao123/weave-workbench) 的执行后端。**

用户在 Workbench 选择团队、确认任务、看进度、处理问题、领取成果。Weave 保存输入和工作流版本，协调执行，记录等待与恢复，并保存产出的工作。

Workbench 是唯一受支持的业务入口。本仓库的内嵌界面只供维护者管理运行时。`weave` 命令负责服务部署、运行时维护和 Workbench 使用的 MCP 连接。独立业务 CLI 和 Codex/Claude 客户端接入教程退出现行支持；Codex、Claude 等执行引擎继续作为运行时的实现选择。

当前面向有维护者支持的私有设计伙伴试点。现行要求见[产品合同](docs/架构/2026-09-05-Weave-当前产品合同.md)、[工作对话方案](docs/架构/2026-09-05-Workbench-工作对话方案.md)和[统一验收清单](docs/验收/2026-09-05-Workbench统一验收清单.md)。历史版本和测试记录不能证明当前工作树已经通过验收。

## 职责

| 组成 | 负责什么 |
|---|---|
| Workbench | 工作对话、选团队、确认任务、进度、人工决策、恢复操作和成果阅读 |
| Weave API 与执行服务 | 持久任务、冻结工作流、排队、执行状态、权限、恢复、用量和成果保存 |
| Runtime | 在配置好的机器上执行当前任务，返回实际产出和可提供的活动记录 |
| 运行时管理界面 | 维护者登录、运行时注册、连接、容量和本机运行时控制 |

Workbench 宿主连接 Weave HTTP API，并启动 `weave mcp serve` 作为内部连接。浏览器使用 Workbench，服务凭据留在宿主。运行时管理页面只用于维护，不是第二套业务产品。

执行结束不等于成果可以采用。成果必须属于正确的运行且可以读取；文件缺失、活动不完整、用量未知和等待原因都要如实显示。恢复应保留已保存的工作，已请求停止和已确认停止必须区分。

## 维护者安装

使用 PostgreSQL 16 和 [go.mod](go.mod) 指定的 Go 版本。Workbench 有独立的 Node.js/pnpm 依赖与发布周期，应将两个仓库的确切版本一起记录、一起验证。

### 本机启动 Weave

先准备私有 PostgreSQL 数据库。服务端密钥只生成一次，重启时继续使用：

```sh
export DATABASE_URL='postgres://weave:<database-password>@127.0.0.1:5432/weave?sslmode=disable'
export JWT_SECRET="$(openssl rand -hex 32)"
export WEAVE_SECRET_KEY="$(openssl rand -hex 32)"
export WEAVE_API_URL='http://127.0.0.1:8080'

go build -o ./bin/weave ./cmd/weave
umask 077
./bin/weave bootstrap > bootstrap.json
export WEAVE_API_KEY="$(jq -r '.api_key // empty' bootstrap.json)"
./bin/weave serve
```

首次 bootstrap 创建管理员及其名下的 API key；重复执行保留原账号和密钥，只有新建时才返回原始 key。后续启动应从秘密存储读取原 key，不要用空的 bootstrap 字段覆盖。bootstrap 输出包含明文凭据，应私密保存，转移到受控秘密存储后删除工作目录中的副本。

`WEAVE_SECRET_KEY` 和 `WEAVE_SECRET_KEY_FILE` 只能设置一个。密钥为 32 字节，使用 64 位十六进制或标准 base64 编码；文件方式指向包含此值的普通文件。它应与数据库备份共同保全。这两个设置和 `DATABASE_URL` 都不应提供给 Workbench 或 MCP 子进程。

服务启动后，两个检查都应成功：

```sh
curl --fail http://127.0.0.1:8080/v1/health
curl --fail http://127.0.0.1:8080/v1/ready
```

### 连接 Workbench

在单独检出的 Workbench 仓库执行：

```sh
pnpm install --frozen-lockfile
pnpm build:workbench
WEAVE_COMMAND='/absolute/path/to/weave-next/bin/weave' \
WEAVE_API_URL='http://127.0.0.1:8080' \
WEAVE_API_KEY='<owner-bound-api-key>' \
pnpm workbench --host 127.0.0.1 --port 3080 --no-open
```

在 `http://127.0.0.1:3080/` 使用 Workbench 本机登录流程。`WEAVE_COMMAND` 指向配套的 Weave 二进制，由 Workbench 启动 MCP 进程。派发前还需要可用团队和已配置的执行环境。页面打开或健康检查通过，只能证明入口可用，不能代替真实任务验收。

### 容器平台

平台部署和验证统一使用 [docker-compose.platform.yml](docker-compose.platform.yml)：

```sh
cp .env.example .env
# 填入实际生成的 JWT_SECRET、WEAVE_SECRET_KEY、WEAVE_ADMIN_PASS
# 以及私有 POSTGRES_PASSWORD。不要把 shell 表达式写进 .env。
scripts/refresh-weave.sh
docker compose -f docker-compose.platform.yml ps
```

该配置启动 Weave 和数据库；Workbench 单独安装。Weave 的 8080 端口提供 API 和运行时维护页面，PostgreSQL 不发布主机端口。刷新脚本始终更新主检出目录对应的平台，即使从 Git worktree 调用也是如此。隔离验证应另设 Compose project、私有环境、新卷和端口覆盖。

可选的 `runtime` profile 使用 `runtime_data` 命名卷保存 `/data/runtime-workspaces`，包括 `.weave-public-events` 待上传记录。重建运行时容器会保留这些文件，删除该卷则会删除其中内容。

维护者使用 `WEAVE_ADMIN_USER`（默认 `admin`）及自己配置的密码登录，没有默认密码。先注册运行时并保存 token，再启用可选的 `runtime` profile。只配置本次部署实际需要的模型引擎。

## 架构与边界

| 目录 | 职责 |
|---|---|
| `internal/base` | 不依赖运行时的协作状态、任务队列、执行记录和持久化 |
| `internal/kernel` | 智能体、工作流、执行引擎、运行时和 MCP 机制 |
| `internal/build` | 团队构建、编译、评测和恢复机制 |
| `internal/app` | 面向 Workbench 的 API、MCP 连接、daemon 和产品组装 |
| `weave-app` | 内嵌运行时维护界面和本机运行时控制 |
| `cmd/weave` | 服务和维护命令入口 |

四个内部层级不得向上导入。Go 模块保持 `github.com/jinyitao123/weave`，使用 Go modules 解析依赖，不使用 `vendor/`。[Loom](https://github.com/jinyitao123/loom) 提供图执行机制；冻结工作流不能保证模型回答或外部动作完全相同。

MCP 写入闸拒绝 `write_tools` 明确声明的工具。未声明的工具不会仅凭名称自动阻断，CLI 引擎仍使用所在主机的权限。因此当前需要受信任的部署环境，不能把该机制当成覆盖所有外部动作的沙箱。

运行时只采集能够归属本次执行的受支持 UTF-8 文件，单文件上限 256 KiB，合计上限 512 KiB。明确承诺但没有保存的文件属于交付失败，不能把路径回执当成完整成果。预览上限和采集上限不同，完整下载只能读取平台已经保存的内容。用量未知不能显示为费用为零。

## 验证与记录

```sh
make ci
npm --prefix weave-app ci
make ui-embed
make compose-check
# 使用隔离数据库，不连接业务数据库：
TEST_DATABASE_URL='<isolated-postgresql-url>' make test-integration
```

`make test` 覆盖 `./internal/... ./cmd/...`，数据库集成验证需要 PostgreSQL。容器健康、工程检查、实际 Workbench 使用和 [20 项真实任务试点](docs/验收/2026-09-05-20项真实任务试点台账.md)是不同的证据，必须记录对应源码与实际结果，待验项目不能写成通过。

升级前备份 PostgreSQL、稳定的凭据密钥和 Workbench 的 `DSH_HOME`。数据库迁移单向递增，代码回退不等于数据库回退；恢复应先在隔离环境验证。

[架构文档目录](docs/架构/README.md)索引现行合同及仍支撑 Workbench 的执行与迁移资料。[8 月 31 日发布基线](docs/验收/2026-08-31-Weave-Workbench-v0.1-设计伙伴版发布基线.md)保留当时的确切版本组合与验收事实，不代表当前源码版本。

## 许可

Apache-2.0
