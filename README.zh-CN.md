# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave 是 GooeyPi 桌面与 Forge 产品背后的团队编排和执行服务。**

员工和开发者使用 [GooeyPi 桌面加 Forge](https://github.com/jinyitao123/weave-workbench) 完成受支持的产品流程。Weave 负责团队与成员定义、已发布工作流版本、调度、执行状态、租约、额度、取消、恢复、用量和运行时接入。Forge 负责员工身份、业务记录与动作、权限、审批及最终业务状态。

已退役的网页 Workbench 代码已删除。HTTP 与 MCP 接口用于产品集成和服务运维，不构成第二套员工业务界面。服务端二进制在 `/admin` 提供管理端，供开发者和运维管理节点、环境、团队与任务，源码见 [web/admin](web/admin/README.md)。

## 职责

| 组成 | 负责什么 |
|---|---|
| Weave | 团队与成员定义、冻结工作流、调度、执行、租约、额度、取消、恢复、用量和运行时接入 |
| Forge | 员工身份、业务对象与动作、访问权限、审批和权威业务结果 |
| GooeyPi 桌面加 Forge | 受支持的员工与开发者产品体验；业务结果在 Forge 中独立回读 |

执行结束不等于成果可用。已保存的产出必须关联正确运行且可读取；文件缺失、活动不完整、用量未知和等待原因都要如实报告。恢复应保留已保存工作，请求停止与确认停止必须区分。

## 开发与验证

使用 [go.mod](go.mod) 声明的 Go 版本。主要检查命令如下：

```sh
make test
make depguard base-depguard
make productguard
make compose-check
```

`make test` 覆盖 `./internal/... ./cmd/...`；数据库集成检查需要 PostgreSQL。容器健康和工程检查只能证明组件行为。产品验收从桌面正常路径开始，并在 Forge 中独立读回业务结果。

## 服务运维

使用 `go build -o ./bin/weave ./cmd/weave` 构建运维二进制。`weave bootstrap` 用于建立服务运维凭据；输出应保存在私有秘密存储中。Weave 不提供本地用户密码登录或首用户注册。员工和开发者在受支持的产品流程中通过 Forge 完成身份认证。

产品开通、部署边界和当前验收状态见[产品总仓](https://github.com/jinyitao123/weave-workbench)。平台 Compose 文件用于开发与组件验证。

默认 Docker 构建和 Compose 的 `weave` 服务使用 `server` target，只包含 Go 服务、健康检查所需的 Node 及六平台 Runtime Host 下载产物，不安装 Codex、Claude Code 或 OpenCode。Loom 可以使用已配置的模型服务在主服务内执行。CLI 成员以及借 CLI 提供推理的 providerless Loom，需要已注册、对应引擎和认证均已就绪的执行节点。

在执行节点安装所需的 CLI 引擎，通过运行时运维入口注册节点，再使用其节点令牌启动现有 `weave runtime` 命令。`/install.sh` 和 `/install.ps1` 安装 Runtime Host，由 Host 上报其执行环境中可用的引擎。容器执行节点可显式构建 `docker build --target executor -t weave-executor .`；Compose 的可选 `runtime` profile 构建该 target，使用独立的 `WEAVE_RUNTIME_IMAGE` 标签，默认是 `weave-executor`。`WEAVE_PLATFORM_IMAGE` 仅指向主服务镜像。

`WEAVE_LOCAL_RUNTIME_ENABLED` 默认是 `false`，主服务 Compose 也保持关闭。运维人员明确选择同机 Host 时，仍可在自有部署中设为 `true`，并自行提供所需 CLI。此开关不会改写已发布工作流冻结的节点绑定，也不会替代缺失的外部节点。

[架构入口](docs/架构/README.md)导航 Weave 当前分层、契约和迁移证据。带日期的验收记录保留当时观察到的结论与限制，不能证明当前工作树或部署仍然通过。

## 许可

本仓使用 [MIT 许可证](LICENSE)。

## 飞书机器人

可选双向消息接入，默认关闭。发送实现改编自[nexus](https://github.com/jinyitao123/nexus/blob/100890744f1189d149f4000919539fb057dd4c75/orchestration/internal/notify/feishu.go)，保留应用token内存缓存并补充HTTP/JSON/业务码/消息回执检查、禁止重定向和来源消息去重。接收及任务适配在`internal/app/api/feishu_*.go`；不增加SDK依赖、执行队列、员工角色、业务审批或待办体系。共享契约由[产品总仓](https://github.com/jinyitao123/weave-workbench/blob/main/contracts/v1/README.md#飞书机器人双向接入)维护。

部署方可以从安全的环境配置提供以下变量作为部署应用，不把值写入Git或模型输入；部署应用只服务尚未在管理端保存自己应用的工作区：

| 变量 | 含义 |
| --- | --- |
| `WEAVE_FEISHU_APP_ID` | 自建应用App ID |
| `WEAVE_FEISHU_APP_SECRET` | 自建应用密钥 |
| `WEAVE_FEISHU_VERIFICATION_TOKEN` | 事件订阅Verification Token |
| `WEAVE_FEISHU_ENCRYPT_KEY` | 事件订阅Encrypt Key |
| `WEAVE_FEISHU_TENANT_KEY` | 唯一允许接入的飞书租户 |

工作区管理员（`admin`、`owner`）也可以在管理端“集成”页保存本工作区自己的应用（`/v1/integrations/feishu/app`）：三项密钥用服务端凭据密钥（`WEAVE_SECRET_KEY`）加密保存，任何接口都不返回原文，审计只记字段名；保存与删除带修订号，首次保存或更换应用须填全三项密钥；一个App ID只能属于一个工作区，也不能与部署应用相同，因此原有按App ID隔离的收发查询仍限于该工作区。工作区应用使用独立回调地址`/v1/integrations/feishu/events/<回调键>`，可在页面一键检查令牌申请；保存后，部署应用对该工作区的旧绑定只回复重新绑定提示，也不再投递其通知。

团队默认不出现在飞书中，须在团队详情“接入”页开启（`/v1/teams/:id/feishu-access`），可指定飞书发起时使用的已发布流程（默认团队流程），并分别关闭完成、需要补充、需要人工确认、失败、取消五类通知；关闭的类别记为`suppressed`，不重试，不影响Forge收件箱。

飞书自建应用启用机器人、私聊消息接收事件`im.message.receive_v1`及`im:message:send_as_bot`发送权限，订阅地址设为部署方可访问的`/v1/integrations/feishu/events`。协议参见[发送消息](https://open.feishu.cn/document/server-docs/im-v1/message/create)与[官方事件实现](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/event/event.go)。普通事件回调核对签名、时间、Verification Token、App ID与租户；URL校验按飞书官方协议仅验证加密challenge/token，不要求签名头，再持久化消息回执并应答；既有员工运行事件worker处理该回执，使接收请求不等待模型执行。仅接受真人私聊文本。

员工在GooeyPi桌面“设置／企业账号／飞书”取得绑定码，向机器人发送`绑定 <码>`，然后可发送`团队`、`开始 <团队名称> <任务>`、`查看`、`继续 <补充要求>`和`确认 <JSON>`。绑定码只保存摘要，10分钟单次有效；连接到期不超过员工登录会话到期及8小时，过期需本人重新登录/绑定。解除连接可从桌面或机器人`解绑`办理；解绑不取消已接单工作。

发起与修订复用现有固定输入、已发布流程、成员队列和成果记录，授权业务动作空集。继续绑定原目标、原输入与上一版成果；团队人工确认沿用当前交互ID、schema和既有续办服务。涉及Forge正式业务、文件或授权续签须回桌面办理。运行事件分别保留Forge与飞书真实回执；飞书仅通知该来源工作绑定的本人，其他员工和群聊不能代办或读取。本地schema隔离的PostgreSQL回归实际运行已发布的确定性流程，覆盖接单去重、成果修订、人工停等/确认、终态推送及账号隔离；该证据不表示飞书线上或业务闭环已验收。

飞书消息请求使用稳定`uuid`。其原生去重有时间窗，断网后超窗重试仍可能重复消息，因此不能宣称严格一次投递；固定输入和任务准入保留平台幂等。每个私聊按接收顺序办理；服务或投递失败保留原消息和固定版本，稍后重试，不能切换到新父任务。运行状态和业务状态仍由各自服务拥有。

## 业务能力目录快照

开发者、管理员或所有者通过 Forge 身份登录时，服务端用该会话读取一次 Forge 业务动作目录，按工作区保存定义快照；读取失败不影响登录，也保留上一份快照。快照只含动作名称、效果、参数和可用状态，不含令牌或业务数据，只用于管理端挑选成员可用的业务动作。发布与运行仍按当前目录和员工委托校验，快照过期不放宽权限。管理端通过 `GET /v1/development/business-capabilities` 读取，需要开发者及以上角色。
