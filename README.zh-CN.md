# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave Workbench 的完整产品仓库。**

用户在 Workbench 选择团队、确认任务、看进度、处理问题、领取成果。Weave 保存输入和工作流版本，协调执行，记录等待与恢复，并保存产出的工作。

Workbench 是唯一受支持的页面入口。运行节点的接入与任务相关状态放在 Workbench，服务诊断和机器进程维护使用 `weave` 命令、系统服务或 Compose。独立业务 CLI 和 Codex/Claude 客户端接入教程退出现行支持；Codex、Claude 等执行引擎继续作为运行时的实现选择。

当前面向有维护者支持的私有设计伙伴试点。现行要求见[产品合同](docs/架构/2026-09-05-Weave-当前产品合同.md)、[工作对话方案](docs/架构/2026-09-05-Workbench-工作对话方案.md)和[统一验收清单](docs/验收/2026-09-05-Workbench统一验收清单.md)。历史版本和测试记录不能证明当前工作树已经通过验收。

## 快速开始

先安装带 Compose 的 Docker Desktop 或 Docker Engine，然后在 macOS、Linux 或 WSL2 的仓库目录执行：

```sh
./scripts/install-weave.sh
```

安装器会创建仅当前用户可读的 `.env`，启动 PostgreSQL 和 Weave，创建 Workbench 凭据，注册本机运行时，启动 Workbench，并打印可直接登录的浏览器地址。再次执行会继续使用原配置和数据。有匹配的正式版本镜像时会直接拉取 amd64 或 arm64 镜像；没有发布镜像的源码版本会在本机完成构建。

打开页面后只需添加一个模型供应商，再创建或导入团队。首次派发前，Workbench 会显示服务、凭据、团队和运行时是否已经就绪。

```sh
docker compose -f docker-compose.platform.yml ps
docker compose -f docker-compose.platform.yml logs -f workbench runtime
docker compose -f docker-compose.platform.yml stop
```

安装中断时，可用 `docker compose -f docker-compose.platform.yml logs weave workbench runtime` 查看失败服务。端口冲突可在首次安装前设置 `WEAVE_API_PORT` 或 `WORKBENCH_PORT`；已有 `.env` 时直接修改其中对应值。升级时保留 `.env`、`data/workbench` 和 `data/workspaces`，删除它们会丢失凭据或本机 Workbench 数据。

## 职责

| 组成 | 负责什么 |
|---|---|
| Workbench | 工作对话、选团队、确认任务、进度、人工决策、恢复操作和成果阅读 |
| Weave API 与执行服务 | 持久任务、冻结工作流、排队、执行状态、权限、恢复、用量和成果保存 |
| Runtime | 在配置好的机器上执行当前任务，返回实际产出和可提供的活动记录 |
| Workbench 设置 | 运行节点注册、连接状态、容量、改名和撤销 |

Workbench 宿主连接 Weave HTTP API，并启动 `weave mcp serve` 作为内部连接。浏览器只使用 Workbench，服务凭据留在宿主。底层诊断不进入页面。

执行结束不等于成果可以采用。成果必须属于正确的运行且可以读取；文件缺失、活动不完整、用量未知和等待原因都要如实显示。恢复应保留已保存的工作，已请求停止和已确认停止必须区分。

## 维护者安装

使用 PostgreSQL 16、[go.mod](go.mod) 指定的 Go 版本、[.node-version](.node-version) 指定的 Node.js 22.19 或更高版本，以及 `workbench/package.json` 指定的 pnpm 版本。后端与 Workbench 由同一提交一起构建和验证。

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

配置 `WEAVE_API_URL` 与 `WEAVE_API_KEY` 后，可用 `weave doctor` 一次检查服务版本、依赖就绪状态和运行节点注册状态。机器进程由 `docker compose`、systemd 或 launchd 启停，避免浏览器直接控制宿主进程。

### 连接 Workbench

在当前仓库执行：

```sh
make workbench-install
make workbench-build
cd workbench
WEAVE_API_URL='http://127.0.0.1:8080' \
WEAVE_API_KEY='<owner-bound-api-key>' \
pnpm workbench --host 127.0.0.1 --port 3080 --no-open
```

在 `http://127.0.0.1:3080/` 使用 Workbench 本机登录流程。Workbench 默认从 `PATH` 启动 `weave mcp serve`；只有需要选择另一个受信任二进制时，才把 `WEAVE_COMMAND` 设为绝对可执行路径。显式路径不存在时，启动器会直接失败，不再打开连接不完整的 Workbench。派发前还需要可用团队和已配置的执行环境。页面打开或健康检查通过，只能证明入口可用，不能代替真实任务验收。

Workbench 会在每条新运行节点连接命令中放入完整的 Weave 地址。部署服务时将 `WEAVE_RUNTIME_SERVER_URL` 设为公网或其他可路由的 Weave URL；裸机 Workbench 遇到回环 API 地址时，会回退到优先内网 IPv4 并保留已配置的 API 端口。

### 容器平台

平台部署和验证统一使用 [docker-compose.platform.yml](docker-compose.platform.yml)：

```sh
cp .env.example .env
# 填入实际生成的 JWT_SECRET、WEAVE_SECRET_KEY、WEAVE_ADMIN_PASS
# 以及私有 POSTGRES_PASSWORD。不要把 shell 表达式写进 .env。
./scripts/install-weave.sh
docker compose -f docker-compose.platform.yml ps
```

该配置统一启动数据库、Weave、Workbench 和访问网关；可选运行时仍由 profile 控制。Weave 的 8080 端口只提供 API，Workbench 默认使用 3080，PostgreSQL 不发布主机端口。迁移已有部署时，应把 `WORKBENCH_DATA_PATH` 和 `WORKBENCH_WORKSPACE_PATH` 指向原数据位置。隔离验证应另设 Compose project、私有环境、新卷和端口覆盖。

可选的 `runtime` profile 使用 `runtime_data` 命名卷保存 `/data/runtime-workspaces`，包括 `.weave-public-events` 待上传记录。重建运行时容器会保留这些文件，删除该卷则会删除其中内容。

维护者在 Workbench 设置中注册运行节点并保存 token，再启用可选的 `runtime` profile。服务端没有独立维护页面。只配置本次部署实际需要的模型引擎。

## 架构与边界

| 目录 | 职责 |
|---|---|
| `internal/base` | 不依赖运行时的协作状态、任务队列、执行记录和持久化 |
| `internal/kernel` | 智能体、工作流、执行引擎、运行时和 MCP 机制 |
| `internal/build` | 团队构建、编译、评测和恢复机制 |
| `internal/app` | 面向 Workbench 的 API、MCP 连接、daemon 和产品组装 |
| `workbench` | 唯一业务入口及其 TypeScript 运行底座 |
| `cmd/weave` | 服务和维护命令入口 |

四个内部层级不得向上导入。Go 模块保持 `github.com/jinyitao123/weave`，使用 Go modules 解析依赖，不使用 `vendor/`。[Loom](https://github.com/jinyitao123/loom) 提供图执行机制；冻结工作流不能保证模型回答或外部动作完全相同。

MCP 写入闸拒绝 `write_tools` 明确声明的工具。未声明的工具不会仅凭名称自动阻断，CLI 引擎仍使用所在主机的权限。因此当前需要受信任的部署环境，不能把该机制当成覆盖所有外部动作的沙箱。

运行时只采集能够归属本次执行的受支持 UTF-8 文件，单文件上限 256 KiB，合计上限 1 MiB。明确承诺但没有保存的文件属于交付失败，不能把路径回执当成完整成果。预览上限和采集上限不同，完整下载只能读取平台已经保存的内容。用量未知不能显示为费用为零。

## 验证与记录

```sh
make ci
make workbench-install
make workbench-check
make compose-check
# 使用隔离数据库，不连接业务数据库：
TEST_DATABASE_URL='<isolated-postgresql-url>' make test-integration
```

`make test` 覆盖 `./internal/... ./cmd/...`，数据库集成验证需要 PostgreSQL。容器健康、工程检查、实际 Workbench 使用和 [20 项真实任务试点](docs/验收/2026-09-05-20项真实任务试点台账.md)是不同的证据，必须记录对应源码与实际结果，待验项目不能写成通过。

升级前备份 PostgreSQL、稳定的凭据密钥和 `WORKBENCH_DATA_PATH` 指向的目录。数据库迁移单向递增，代码回退不等于数据库回退；恢复应先在隔离环境验证。

[架构文档目录](docs/架构/README.md)索引现行合同及仍支撑 Workbench 的执行与迁移资料。[8 月 31 日发布基线](docs/验收/2026-08-31-Weave-Workbench-v0.1-设计伙伴版发布基线.md)保留当时的确切版本组合与验收事实，不代表当前源码版本。

## 许可

根仓库使用 MIT 许可证。`workbench/` 保留原 MIT 许可证和第三方声明，适用于该目录中的原始及衍生代码。
