# Weave 服务部署记录

本文保留 2026-09-25 至 2026-09-27 的历史部署观察。公开副本已遮蔽主机、运维账号、维护者本机路径和具体配置位置；记录中的健康状态只代表对应日期，不代表当前可访问或当前产品支持状态。

## 主机与目录

| 项目 | 配置 |
| --- | --- |
| SSH 主机 | `<WEAVE_SERVICE_HOST>:22` |
| SSH 用户 | `<SSH_USER>` |
| 系统 | Ubuntu 24.04，amd64，4 vCPU，约 4 GB 内存 |
| 上游仓库 | `jinyitao123/weave-next`，私有仓库 |
| 跟随分支 | `main` |
| API | `http://<WEAVE_SERVICE_HOST>:8080` |
| 历史 Web Workbench 网关（已退役） | `http://<WEAVE_SERVICE_HOST>:3080` |
| 历史 Git 源码缓存 | `<SERVICE_DATA_PATH>`（新部署不依赖此目录） |
| 部署状态与版本目录 | `<SERVICE_DATA_PATH>` |
| 当前成功版本 | `<SERVICE_DATA_PATH>` |
| 服务配置 | `<SERVICE_DATA_PATH>`，权限 `600` |
| Workbench 数据 | `<SERVICE_DATA_PATH>` |
| Workbench 工作目录 | `<SERVICE_DATA_PATH>` |
| Compose project | `weave-main` |
| 数据库卷 | `weave-main_pgdata`，不发布数据库端口 |

密码与应用 API key 不进入 Git。维护者本机凭据存储位置、钥匙串条目名称和服务器配置目录均已从公开副本移除；应用管理员凭据与 SSH 凭据分开保管。

## 更新链路

### 当前容量门禁

部署 runner 在同一 `deploy.lock` 内，构建前检查状态目录、备份目录及 `docker info` 返回的实际 `DockerRootDir` 所在文件系统的普通 `Available` 字节。默认构建至少需要 8 GiB；备份前以及备份完成、切换容器前分别重新检查，至少需要 1 GiB。Docker 与状态目录使用不同磁盘时，每处都必须满足门槛，不相加，也不把 root 保留空间计入可用容量。

阈值由 runner 环境变量 `WEAVE_DEPLOY_MIN_BUILD_FREE_BYTES`（默认 `8589934592`）及 `WEAVE_DEPLOY_MIN_ACTIVATE_FREE_BYTES`（默认 `1073741824`）配置，只接受非空正整数的字节数。检查使用 GNU `df -B1 --output=avail`；无法读取 Docker 根目录、命令失败、未知或非法容量输出均拒绝继续。构建前拒绝时不构建或切换；备份前拒绝时不备份或切换；备份后拒绝时保留已有备份并不切换。

容量检查不是空间预留，也不预测构建峰值或数据库备份大小；其他进程仍可能在检查后占满磁盘。runner 不自动清缓存、删镜像、恢复数据库、重试或降低阈值，容量不足由运维另行处理。服务健康与业务验收仍需独立验证。

### 部署步骤

1. `main` 推送触发 `Go CI`。
2. 所有 CI job 通过后，`Deploy main` 检查该 SHA 仍是当前 `main`。
3. Actions 按完整 SHA 检出已通过 CI 的提交，生成 Docker 构建所需源码归档并计算 SHA-256；CI 验收文档保留在仓库中，但不进入构建包。通过专用 SSH 部署命令一并传送短期令牌、归档摘要和归档字节。
4. 服务器核对归档大小与 SHA-256，在该 SHA 的独立发布目录解包；验证后原子更新固定 runner，再构建两份镜像。
5. 构建后通过 GitHub API 再次确认 `main` 仍是该 SHA，备份数据库和配置，再启动服务。
6. 当时核验 `/v1/health` 的 SHA、`/v1/ready`、鉴权 API 和 Workbench 网关；网页 Workbench 现已退役，这条历史探针不代表当前受支持入口。
7. 全部成功才更新 `current` 和 `last-success.json`。

推送分支必须是 `main`；PR 的 CI 不触发部署。正在构建的旧提交发现主线变化时
退出，保留现有服务。Actions concurrency 与服务器 `flock` 防止重复部署并行切换。
工作流也允许手动运行，但仍要求当前 `main` 已有成功的 CI 记录。

构建失败不会切换容器。启动或健康检查失败会令 Actions 失败，备份与日志保留在
部署状态目录；不会自动恢复数据库或宣称镜像回退等于数据库回退。
更新存在短暂服务切换，不承诺零停机。Runtime 主机独立部署，不随中心服务更新自动升级。

## 仓库连接与密钥

- GitHub 变量：`WEAVE_DEPLOY_HOST`、`WEAVE_DEPLOY_USER`。
- GitHub Secrets：`WEAVE_DEPLOY_SSH_KEY`、`WEAVE_DEPLOY_KNOWN_HOSTS`。
- Actions 公钥在 `authorized_keys` 中限定为固定部署命令，禁止普通 shell 和端口转发。
- Actions 把本次 job 的仓库只读 `GITHUB_TOKEN`、源码归档摘要和归档字节通过 SSH 标准输入交给更新脚本。
  服务器只通过 GitHub API 核对当前 `main`；源码内容来自 Actions 对 CI 已验证 SHA 的检出。
  该短期 token 只用于 API 请求，不保存到磁盘、Git 配置、镜像或构建进程。
- 构建镜像源由 `GOPROXY`、`NPM_REGISTRY`、`DEBIAN_MIRROR` 配置；应用源码保持 Git 提交内容。

## 首次安装与恢复

服务器 authorized key 的固定命令入口位于 `<SERVICE_DATA_PATH>`。如果 `Deploy main` 报该文件缺失，不直接远程运行 `deploy-main.sh` 或手动切容器。使用维护 SSH 别名 `<DEPLOY_SSH_ALIAS>` 执行源码中的 `scripts/bootstrap-deploy-main.sh`；脚本只在固定状态目录安装 `deploy-dispatch.sh` 与 `deploy-main.sh`，已有任一正式文件时拒绝覆盖，不启动或切换服务。

Dispatcher 只接受 `deploy <40位小写 SHA>` 并原样转交 stdin。runner 按顺序读取一次性 GitHub job token、64 位归档摘要和源码归档；先核对大小与 SHA-256，再解包到对应发布目录。不得用不匹配或不完整归档覆盖已有发布目录。服务器不再通过 Git smart-HTTP 拉取源码；GitHub API 只用于构建前后核对 `main`。SHA 对齐、CI 前置、锁、数据库备份、镜像构建、切换及健康核验条件不变。

旧 Workbench bind mount 的数据目录由部署状态脚本读取私有部署配置中的绝对路径。只对缺失路径创建空目录；已存在目录保留原内容和权限。拒绝符号链接、文件路径以及两个路径相同或重叠。Weave PostgreSQL 与 runtime/workspace 使用的命名卷不删除、不重建。

**2026-09-25 归档发布过程：** 完整 Go CI、Workbench 与 Compose 检查通过后，提交 `b8122a5479496a3c6c8c8451954640b00a6b1f5b` 经标准 `Deploy main` 部署成功。Actions 以该 CI 已验证 SHA 生成构建源码归档，124 核验摘要、解包并构建平台与 Workbench 镜像；部署前复核 main，再备份数据库与配置，启动后验证 `/v1/health`、`/v1/ready`、认证 API 与 Workbench gateway。当时 `last-success.json` 与 `/v1/health` 均为该 SHA。

排障确认旧固定 runner 与仓库新版脚本不一致，且服务器 Git smart-HTTP TLS 中断；已通过现有 compare-and-swap 更新固定 runner，后续发布由 Actions 传递源码归档，不再依赖主机直接抓取 Git pack。首次完整归档包含验收文档，上传耗时过长；发布包现排除经 CI 检查但 Docker 构建不消费的 `docs/`，保留代码、Dockerfile、Workbench 与部署脚本并核对摘要。

部署前还发现服务私有配置中的 Postgres 密码、管理员登录配置和 API key 与保留数据库不匹配。通过旧测试环境配置和数据库网络只读认证核验后，仅恢复服务器配置中的现有测试管理员凭据并清空失效 API key；原生 bootstrap 在发布中创建了服务 API key。没有重置数据库角色或业务账号密码，四个现有业务数据卷均保留；本次切换前数据库备份为 847143 bytes。

**2026-09-27 当日观察：** 经本机当时已有的 SSH 转发读取 `127.0.0.1:18080/v1/health`，服务返回 `status=ok`、`build_commit=135fcaa371df0c22961c596e898d53f687df7d3d`；该提交与本仓 `main` 一致。此状态未在本轮复验。员工消息、Pi 续看和三场景的实际业务读回维护在产品总仓，不以本页的服务健康代替验收。

**2026-10-06 Runtime 兼容性变更（`codex/claude107-runtime`，待评审）：** 该分支调整 daemon 对 Claude CLI 的运行时兼容处理。已在独立 Windows 环境核验：第一方 Claude 登录状态分类；子进程失败与超时被拒绝；进程重启后重放已持久化的结果，且不再次运行 CLI。Windows runtime 二进制可构建，daemon `go vet` 通过。核验边界：本次不能证明断电后 rename 元数据的持久性，也不能证明 Windows ACL 私密性，这两项仍是已记录的限制。本记录不涉及安装脚本、调度配置或中心服务部署，未在 `<SERVICE_DATA_PATH>` 所指的部署上复验。

## 历史部署探针

以下命令记录了当时的服务探针。主机需由维护者在本机私下配置；本文未复验该部署，也不据此声明当前可用。

```sh
curl --fail http://<WEAVE_SERVICE_HOST>:8080/v1/health
curl --fail http://<WEAVE_SERVICE_HOST>:8080/v1/ready
```

在 GitHub Actions 查看 `Go CI` 和 `Deploy main` 的结果。服务器的
`last-success.json` 保存最近一次完整验证的 SHA；`logs/` 保存每次更新日志，
`backups/` 保存切换前数据库与配置。密钥备份与数据库备份必须共同保全。

自动部署不配置模型供应商、不导入业务数据、不注册永久 Runtime。
部署健康验证与真实模型任务验收分别记录。当前入口使用 HTTP；HTTPS 域名与证书尚未配置。
