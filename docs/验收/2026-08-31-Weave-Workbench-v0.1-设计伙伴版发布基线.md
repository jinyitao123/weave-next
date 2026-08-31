# Weave Workbench v0.1 设计伙伴版发布基线

## 1. 发布结论

本版本用于受邀设计伙伴的本地或私有部署试点，不是公开多租户 SaaS。它验证一项产品承诺：用户选择或匹配专业团队后，任务由 Weave 持续托管；Workbench 展示团队、成员、运行时、阶段、输入输出、纠偏影响与最终交付物；关闭对话或重启应用不会丢失已经持久化的工作任务。

首版只支持部署方参与安装和故障处理。自助购买、自动计费、公共网络暴露、多区域容灾、团队市场和任意外部副作用补偿不属于本发布范围。

## 2. 冻结的兼容组合

| 组件 | 仓库 | 功能基线 | 发布标签 |
|---|---|---|---|
| Weave | `github.com/jinyitao123/weave-next` | `728f5f2` | `v0.1.0-design-partner.2` |
| Workbench | `github.com/jinyitao123/weave-workbench` | `3ca8c40` | `v0.1.0-design-partner.2` |

两个标签共同构成一个产品版本，不单独承诺与其他提交组合兼容。Workbench 必须显式连接这一版 Weave 的 HTTP API 和本地 MCP 进程。

## 3. 支持的部署形态

设计伙伴版支持单组织私有部署：

- PostgreSQL 16 保存 Weave 的团队、运行、纠偏、活动和交付物事实。
- Weave API 运行在受信任网络内；首次试点优先绑定本机或内网地址。
- Workbench 与 `weave mcp serve` 运行在同一台受信任主机。
- 浏览器只连接 Workbench；`WEAVE_API_KEY` 留在 Workbench 宿主和 MCP 子进程中。
- `WEAVE_SECRET_KEY` 或 `WEAVE_SECRET_KEY_FILE` 只提供给 Weave 服务端，且数据库整个生命周期内保持稳定。

公共互联网暴露、反向代理 TLS、企业身份源和跨组织管理需要由部署方另行提供，本版本不把这些能力伪装成内建能力。

## 4. 首次安装

### 4.1 前置条件

- macOS arm64 是本发布已经完成业务闭环验证的平台。
- PostgreSQL 16、Go、Node.js 22.19 或更新的受支持版本、pnpm。
- 一个可由 Weave Runtime 使用的模型提供方。
- 两个仓库均检出本文件第 2 节指定的标签，且工作树干净。

### 4.2 初始化 Weave

为服务端配置数据库、JWT 签名密钥和稳定的凭据加密密钥：

```sh
export DATABASE_URL='postgres://weave:weave@127.0.0.1:5432/weave?sslmode=disable'
export JWT_SECRET="$(openssl rand -hex 32)"
export WEAVE_SECRET_KEY="$(openssl rand -hex 32)"
export WEAVE_API_URL='http://127.0.0.1:8080'

go build -o ./bin/weave ./cmd/weave
./bin/weave bootstrap > bootstrap.json
export WEAVE_API_KEY="$(jq -r '.api_key' bootstrap.json)"
./bin/weave serve
```

生产式服务管理应把 `JWT_SECRET` 和 Weave 凭据密钥放入权限受限的文件或秘密存储。可用 `WEAVE_SECRET_KEY_FILE` 替代环境变量；两者只能设置一个。`bootstrap.json` 包含一次性明文凭据，复制所需信息后必须转移到受控秘密存储并从工作目录删除。

服务启动后，以下两个地址都必须成功：

```sh
curl --fail http://127.0.0.1:8080/v1/health
curl --fail http://127.0.0.1:8080/v1/ready
```

### 4.3 启动 Workbench

在 Workbench 仓库执行：

```sh
pnpm install --frozen-lockfile
pnpm run build:workbench
WEAVE_COMMAND='/absolute/path/to/weave-next/bin/weave' \
WEAVE_API_URL='http://127.0.0.1:8080' \
WEAVE_API_KEY="$WEAVE_API_KEY" \
pnpm workbench -- --no-open
```

浏览器访问 `http://127.0.0.1:3080/`。macOS 试点也可以把业务 API key 保存到系统钥匙串服务 `weave-workbench-api-key`，避免每次从命令行传递。

## 5. 首次使用验收

部署不能只以进程启动成功为完成。使用浏览器完成以下验收：

1. 在团队列表看到至少一个 active 且具有默认工作流的团队。
2. 直接选择该团队，或在对话中提出业务诉求并让 Workbench 推荐团队。
3. 确认任务简报后异步派发，页面立即形成可恢复的工作任务。
4. 查看团队成员、当前阶段、运行时、成员输入输出与工具活动。
5. 对一个成员提交纠偏，等待安全点和影响计划出现。
6. 确认纠偏后看到任务继续执行。
7. 关闭浏览器并重新打开，确认同一任务及进度恢复。
8. 完成后打开最终交付物，确认内容、版本和所属运行正确。

任一步骤只能显示真实持久化事实。连接缺失、活动数据不完整或运行时不可用时必须明确提示，不能用前端轮询状态或聊天文本伪装成功。

## 6. 更新与回退

更新前完成以下动作：

1. 停止接收新任务，等待活动运行到终态或安全点。
2. 记录 Weave 与 Workbench 当前标签。
3. 使用 `pg_dump` 生成 PostgreSQL 一致性备份，并验证备份文件可读取。
4. 备份 Workbench 的 `DSH_HOME`，其中包含会话和工作任务投影。
5. 检出新标签、构建并运行数据库迁移。
6. 重做第 5 节的浏览器验收，再恢复业务入口。

Weave 数据库迁移是单向递增的。代码回退不能被当作数据库回退。若新迁移已经执行，需要回到旧版本时，应停止写入，恢复更新前的数据库与 `DSH_HOME` 备份，再启动旧标签。不要手工删除迁移账本或纠偏、活动表。

## 7. 故障与恢复验收

每个试点环境至少执行一次：

- 运行中关闭浏览器，再打开并恢复任务。
- 运行中重启 Workbench，确认宿主重新接管非终态任务的状态同步。
- 在安全点重启 Weave，确认已提交的纠偏、影响计划和检查点仍存在。
- 让 Runtime 暂时离线，确认页面显示阻塞而不是完成；Runtime 恢复后再继续。
- 从备份恢复到隔离环境，确认团队、运行、交付物和 Workbench 会话可读取。

恢复演练不得对正在使用的唯一生产数据库执行。数据库恢复必须使用隔离实例。

## 8. 安全边界

- Workbench 使用的业务 API key 至少需要 `org`、`chat` 和 `runs`，团队创建或管理场景才需要 `admin`。
- API、活动、纠偏和交付物查询都必须按 workspace 与调用身份读取；未知或其他租户的运行返回未找到。
- `WEAVE_SECRET_KEY` 不进入 Workbench、MCP 配置、浏览器产物、模型上下文或日志。
- Workbench 不向浏览器发送 `WEAVE_API_KEY`；浏览器动作先写入宿主会话，再由宿主调用 Weave。
- 设计伙伴版默认不直接暴露到公共互联网；需要远程访问时由部署方提供 TLS、网络访问控制和身份入口。

## 9. 试点数据与晋级条件

每个真实任务记录团队匹配是否正确、首次有效产出耗时、人工纠偏次数、恢复次数、终态、交付物是否被采用以及模型用量。累计至少 20 个真实 FDE 深度任务后再决定是否进入公开商业版。

公开商业版的最低晋级条件是：没有跨租户数据泄露；备份恢复与异常重启可重复通过；首次用户能在 15 分钟内完成首次派发；绝大多数非业务失败可以在页面中解释并恢复；真实用户愿意连续提交下一项工作。计费、组织权限、法律条款和支持响应在晋级时补齐，不提前扩张首版产品面。

## 10. 本次签发记录

2026 年 8 月 31 日在 macOS arm64 的本地设计伙伴环境中，只通过 Workbench 页面完成真实业务闭环：

- 用户提出“为日冕计划选择首个设计伙伴试点工作包”的自然语言诉求。
- Workbench 从 8 个团队中匹配 `corona-mission-baseline`，通过默认工作流异步派发运行 `run-e180b396-7ac9-5b86-a58d-677b83216267`。
- 运行期间重启 Workbench，页面恢复同一任务并继续从 4/9 推进到 9/9；Weave 后台执行未中断。
- 页面可展开 5 名成员的职责、运行位置、阶段、开始时间、耗时、输入来源、上游内容摘要、输出引用和定向纠偏入口。
- 最终页面显示 `loom` 运行时、`inocube-oneapi · gpt-5.6-luna`、12 个交付物，其中包括 1 个可直接展开的最终 Markdown《日冕计划共同论证基线包》。
- 最终交付物把推荐结论限定为有输入门槛的条件性首选，明确区分事实、假设、未知项、停止条件和后续专业工作包。
- 页面诚实显示成员输入与工具活动的部分记录状态；该缺口不影响团队、阶段、运行时、阶段产物和最终交付物闭环，但在全量审计场景补齐前不能宣称完整轨迹。
- 验收中发现前台 agent 曾额外创建 DSH 目标、轮询并保存重复交付物；Workbench `3ca8c40` 已把团队路由规则注册为不会被会话 agent preset 覆盖的独立提示词段。新会话页面已确认禁止影子目标、前台轮询和重复交付物的规则真实进入模型上下文。

因此 `.2` 可作为受邀设计伙伴版使用，不作为公开自助商业版发布。
