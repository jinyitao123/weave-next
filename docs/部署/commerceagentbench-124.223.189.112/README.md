# CommerceAgentBench 公网部署

2026-09-08。已部署固定版本基准环境，公网 MCP 连通验证通过。首个空白样本的原始评分器正常执行并判定任务未完成；这证明环境及评分链可用，不代表 Weave 已完成业务任务。

## 访问与配置

| 项目 | 当前值 |
|---|---|
| 服务器 | `124.223.189.112` |
| 系统账号 | `ubuntu` |
| 公网入口 | `http://124.223.189.112:18071` |
| 健康检查 | `http://124.223.189.112:18071/health` |
| 首个样本 MCP | `http://124.223.189.112:18071/runs/trust-stage1-vendor-brief-20260908-remote001/mcp` |
| HTTP 认证 | `Authorization: Bearer <API token>` |
| 服务器安装目录 | `/home/ubuntu/commerceagentbench` |
| 基准版本 | `084489800bfd3f9f239503eda9e754bc267e98f5` |
| 固定镜像 | `acciolyk/accio_bench@sha256:1e9cf5c72a56794175b7d06ece036b92e296e6b7e9e9a7fa244026f6acea3859` |
| 网关服务 | `commerceagentbench-gateway.service`，已启用开机启动 |

业务工具调用直接访问公网 IP 和端口，不依赖 SSH 隧道。SSH 仅用于安装、准备新样本、评分和维护。当前是 HTTP 接口；浏览器打开根地址显示服务信息，不是 Gmail 网页应用。

用户提供的服务器密码已保存到当前 Mac 的系统钥匙串，账号为 `ubuntu`，服务名为 `weave-commerceagentbench-124.223.189.112`。独立 API token 也保存在钥匙串，账号为 `ubuntu`，服务名为 `weave-commerceagentbench-public-api-124.223.189.112`。可在 macOS“钥匙串访问”中按服务名查找。密码和 token 不写入仓库、任务输入或验收证据。

服务器的 API token 文件是 `/home/ubuntu/commerceagentbench/gateway-token`，权限 `0600`。机器可读的非敏感配置见 [connection.json](connection.json)。

## 复用

每个新任务准备独立样本，使用新的 `run_id`。以下命令在服务器安装目录执行：

```sh
bin/bench prepare api-gmail-vendor-brief-mcp --run-id <new-run-id>
bin/bench status <new-run-id>
bin/bench verify <new-run-id>
bin/bench cleanup <new-run-id>
```

准备完成后，网关自动读取该样本清单，入口为 `/runs/<new-run-id>/mcp`。一个样本包含多个服务时，使用 `/runs/<new-run-id>/<service_name>/mcp`。评分结果及每次评分记录保存在 `runs/<run_id>/verifier/`；清理移除该样本容器和临时评分密钥，保留任务及评分记录。

原始 `task.md` 中的 `127.0.0.1:3071/mcp` 相对于执行任务的 Runtime。若需要保留原任务文本，Runtime 端须用普通 HTTP 转发将该本地端口连接到上述公网 MCP 地址，或者在任务执行工具中明确绑定该公网地址。不得把本地回环地址当成服务器公网地址，也不得改写业务任务后声称原文未变。

本机 Runtime 已使用本目录的 `runtime_bridge.py` 完成这项连接，初始化和 17 个工具读取通过，见 `runtime-bridge-verification.json`。转发器要求独立的 `X-Weave-Bridge-Token`，令牌保存在系统钥匙串并只配置到 Weave 受管 MCP 连接；未携带令牌的本机请求返回 `401`。在当前 Mac 上可用以下方式启动另一测次的连接；先结束已有转发进程，避免同时占用端口：

```sh
python3 runtime_bridge.py --run-id <new-run-id>
```

脚本从系统钥匙串读取 API token，绑定 `127.0.0.1:3071`，固定转发到该 `run_id` 的公网 HTTP 地址。它不读取任务评分文件，也不执行任何业务动作。这个回环地址仅用于兼容原始任务文本，跨主机连接本身直接走公网 HTTP。

网关仅转发 `/mcp`，不公开私有答案、评分器、容器管理或状态修改接口。当前实现适用于此基准的有限 JSON MCP 请求；尚未声明支持长期 SSE。样本容器是一次性运行环境，未验证服务器重启后继续原样本；重启维护后应检查运行状态，并为新测次准备新样本。

## 已验证

从本机直接访问公网地址，健康检查、MCP initialize、tools/list 均成功，共列出 17 个工具。无认证返回 `401`，私有文件和不存在的样本返回 `404`。结果见 [verification.json](verification.json)。

原始评分器基线检查得到 `passed=false`，业务检查 2/7 通过、最终二元分数 0，评分器自身退出 0。这个样本尚无业务操作，结果符合预期。Harness 中通用的 `agent_execution_passed` 字段不是本次 Weave 执行证据，不能用于宣称 Agent 已完成工作。

此次安装没有修改现有 Weave 的 `8080`、Workbench 的 `3080` 或数据库容器。部署内容仅包含本目录网关文件、独立安装目录及新增网关服务。
