# CommerceAgentBench 样本服务部署记录

这是 2026-09-08 的历史部署与验收记录。原观察为固定版本基准环境的 MCP 连通验证通过，首个空白样本的原始评分器判定任务未完成。这证明当时的环境和评分链可用，不证明 Weave 已完成业务任务，也不证明该服务现在仍可访问。

公开副本已遮蔽公网主机、运维账号、私有配置路径和钥匙串条目名称。当前主机地址、服务凭据及原始 verification 文件均不随仓库发布。

## 当时记录的服务范围

| 项目 | 公开副本中的记录 |
|---|---|
| 服务主机与 MCP 地址 | 由维护者在本机环境私下配置 |
| HTTP 认证 | 使用独立 API token；值不保存在仓库 |
| 基准版本 | `084489800bfd3f9f239503eda9e754bc267e98f5` |
| 固定镜像 | `acciolyk/accio_bench@sha256:1e9cf5c72a56794175b7d06ece036b92e296e6b7e9e9a7fa244026f6acea3859` |
| 网关服务单元 | 私有部署配置，不在公开副本记录 |

业务工具当时通过 HTTP MCP 访问样本服务，SSH 用于安装、准备样本、评分和维护。浏览器打开服务根地址只显示服务信息，不是 Gmail 网页应用。

## 历史样本操作

每个新任务准备独立样本并使用新的 `run_id`。当时在服务器安装目录执行：

```sh
bin/bench prepare api-gmail-vendor-brief-mcp --run-id <new-run-id>
bin/bench status <new-run-id>
bin/bench verify <new-run-id>
bin/bench cleanup <new-run-id>
```

准备完成后，网关读取该样本清单，入口为 `/runs/<new-run-id>/mcp`；多服务样本使用 `/runs/<new-run-id>/<service_name>/mcp`。评分记录位于样本的 `verifier/` 子目录。清理会移除样本容器和临时评分密钥，但保留任务及评分记录。

原始 `task.md` 使用的回环 MCP 地址相对于执行任务的 Runtime。若复现历史任务，应由 Runtime 通过受管 HTTP 转发连接到维护者本机配置的服务地址；不得把本地回环地址冒充公网地址或改写任务后声称原文未变。

## 本机 Runtime 桥接

当时的本机 Runtime 使用 `runtime_bridge.py` 完成连接，MCP initialize 和 `tools/list` 返回 17 个工具。桥接需要独立 `X-Weave-Bridge-Token`，无认证请求返回 `401`。脚本现要求部署者私下提供服务地址和钥匙串条目环境参数，代码不再包含固定远端主机或钥匙串服务名称：

```sh
export WEAVE_BRIDGE_TARGET_RUNS_URL='<configured HTTP runs base URL>'
export WEAVE_BRIDGE_KEYCHAIN_ACCOUNT='<private Keychain account>'
export WEAVE_BRIDGE_API_TOKEN_SERVICE='<private API-token service label>'
export WEAVE_BRIDGE_TOKEN_SERVICE='<private bridge-token service label>'
python3 runtime_bridge.py --run-id <new-run-id>
```

桥接只绑定本机回环接口。`WEAVE_BRIDGE_TARGET_RUNS_URL` 必须由维护者配置为以 `/runs` 结尾的 HTTP 基址。脚本只转发 MCP 请求，不读取评分文件或执行样本业务动作。

## 历史结果与限制

当时的健康检查、MCP initialize 和工具列表均成功，共列出 17 个工具；无认证返回 `401`，私有文件和不存在的样本返回 `404`。原始 verification 数据属于本机验收材料，已从公开说明中移除。

空白样本的原始评分器得到 `passed=false`、业务检查 2/7 通过、最终二元分数 0，评分器自身退出码为 0。该样本没有业务操作，此结果符合当时预期。通用 `agent_execution_passed` 字段不能作为 Weave 实际完成业务任务的证据。

当时网关只转发 `/mcp`，不公开私有答案、评分器、容器管理或状态修改接口；只验证了有限 JSON MCP 请求，未声明长期 SSE 或服务器重启后复用原样本。
