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

## 管理端部署连接

管理员在管理端“部署连接”查看业务系统与团队服务地址，点击“下载组织连接文件”。来源为安装器现有连接记录的独立无凭据文件，通过`WEAVE_ORGANIZATION_CONNECTION_FILE`配置并只读挂载；服务不读取完整安装状态，也不根据浏览器地址推断部署地址。接口为`GET /v1/admin/organization-connection`，复用管理端会话或Bearer管理员身份，要求admin／owner与admin调用范围；匿名401、非管理员403、文件缺失或无效503。

下载只含数值version=1及forgeOrigin、weaveOrigin，与安装器新导出文件使用同一标准字节。部署文件更新后，下一次下载重新读取；地址变化先显示新地址供管理员核对。员工从桌面已有“导入组织连接”入口选择文件、确认、重启，再以本人账号登录。下载成功不代替实际连接或登录验收。

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

## 管理端查看运行与刷新目录

管理员和所有者在管理端的运行页读取本工作区全部团队运行，包括员工从桌面、飞书发起的任务；开发者只读取本人提交的。每一步产出的结果经 `GET /v1/admin/tasks/:id/outputs/:output` 按需读取，单项最多返回 256 KiB。读取他人运行的活动、步骤结果或导出证据时，在 `weave_run_view_audit` 记录查看人、角色、运行和时间，不记录内容；同一查看人、同一运行、同一入口一小时内只记一条。追加要求和创建合并请求仍只限提交者本人，停止与重试沿用运行自身的归属判断。

`POST /v1/development/business-capabilities/refresh` 让开发者主动刷新业务能力目录快照：浏览器用当前登录人重新取得的一次性 Forge 会话调用，服务端核对它与当前管理端会话是同一账号后同步读取目录并替换快照；读取失败保留旧快照。

## 受控修改与草稿待办

团队草稿除了整份保存，还可以按“受控修改”逐项修改，供桌面助手等入口使用：调用方用草稿和业务动作目录里的准确名称指明成员、流程、步骤和动作，服务端解析名称、应用修改并做与保存相同的校验；不能写任意 JSON 路径，也不会增加目录之外的业务动作。

- `GET /v1/development/operations` 列出可修改的类别，每类一行；`GET /v1/development/operations/:topic` 给出该类每种修改的说明和例子。调用方用到哪类再读哪类。
- `POST /v1/teams/:id/development/operations` 提交 1 至 24 项修改，带 `expected_revision` 和 `request_id`。任何一项不合法时整组不保存，并指出是第几项；`dry_run` 只返回会带来的变化，不写入。保存过的请求按 `request_id` 记在 `weave_team_development_requests`（操作人、修订前后、各类修改的数量和逐项说明，不含修改内容），同一请求重发返回首次结果，同一标识换了内容返回冲突。
- `POST /v1/development/teams` 按 `request_id` 新建一个团队，返回团队和草稿修订。`kind` 为 `business`（默认）时带负责人、一位业务办理员和基础流程；为 `code` 时带负责人、在 Claude 上编码与在 Codex 上验证的两位成员，以及验证未通过时退回编码（最多三轮）的流程，与管理端原来新建的代码团队相同。同一请求重发返回同一个团队；中途失败后重发会接着已建好的部分完成，不会多建。管理端、桌面和 MCP 入口都用这个接口。
- `GET /v1/teams/:id/development` 多返回 `issues`：草稿还差什么、挡住试跑还是发布，或只是建议。例如成员带业务动作而流程没有选定必须办成的动作、流程说明为空。

成员的模型也可以用受控修改指定（`member` 与 `member_add` 的 `model`）：只对使用内置引擎的成员有效，只能取当前组织已启用的模型，这份清单随“成员”一类的说明一起返回；组织还没有启用任何模型时修改被拒绝并说明原因。新建的团队，负责人和成员都使用组织已启用的第一个模型；负责人要执行流程的第一步，没有模型时同样列入草稿待办。

组织共享的模型来自模型供应方。开发者和管理员可以查看并配置：

- `GET /v1/development/model-providers` 列出本组织共享的供应方：标识、名称、接口地址、提供的模型，以及是否随部署提供。不返回密钥。
- `PUT /v1/development/model-providers/:id` 新增或更换一个供应方，提交 `name`、`base_url`、`models` 和 `api_key`。更换时省略 `api_key` 即沿用已保存的密钥；隐藏占位符不能当作新密钥。`base_url` 是兼容OpenAI的接口基址，运行时追加 `/v1/chat/completions`。密钥到达后即加密保存，任何接口都不再返回。随部署提供的供应方和个人的供应方不能在这里修改；一个模型只能由一个组织供应方提供，重复时返回 `409 model_already_provided`，同组织的占用检查与保存共用事务和数据库锁，并发请求只成功一个。

成员怎样运行也用受控修改设置，写法在“成员的引擎与运行设置”一类（`member_settings`），该类说明同时返回可选的模型和节点：

- `member_engine` 更换引擎、执行节点和模型。外部引擎的成员按节点名称指定节点，内置引擎的成员从组织已启用的模型中选；负责人不能更换引擎。
- `member_limits` 设置执行限制和工具循环轮次，`member_memory` 设置记忆，两者只对内置引擎的成员有效。
- `member_tools` 禁止成员使用工具，或一次移除成员带有的全部 MCP 服务、技能库技能、允许和需确认的工具；`member_handoff` 设置允许的交接方式，流程已在用的方式不能取消。

### 成员的外部工具

成员可以带工作区的 MCP 服务、技能库技能，以及允许直接使用或使用前需确认的工具。写法在“成员的外部工具”一类（`member_external`），该类说明同时返回当前可用的 MCP 服务和技能库技能名称；管理端从 `GET /v1/development/member-tools` 读取同一份清单：

- `member_mcp` 按服务名称添加或移除 MCP 服务。只有已启用、走 HTTP、并且连通检测成功过的服务能添加。
- `member_library_skill` 按名称添加或移除技能库技能，只对内置引擎的成员有效。准备试跑时，新加的技能固定为技能库当时的最新版本，成员原有的技能保持原来固定的版本。
- `member_tool_rule` 在“允许直接使用”（`allow`）或“使用前需确认”（`ask`）里添加或移除一个工具名称，只对内置引擎的成员有效。

带这些配置的成员可以试跑和发布。MCP 服务里声明为只读的工具，试跑和正式工作中都真实调用；其他工具（包括没有声明的）进入写入闸门，试跑和正式工作中都被拒绝执行，所以试跑不会写入外部系统，结果也与发布后一致。使用外部引擎的成员带 MCP 服务时，须先选定执行节点。

保存受控修改不改变已发布的团队；试跑和发布仍走原来的接口和条件。

### 团队导出与导入

团队可以导出成一个团队包，在另一个工作区导入成新团队，管理端团队页和桌面助手都提供这两个入口：

- `GET /v1/teams/:id/development/export` 返回已保存草稿的团队包（`format: weave-team-package`，`version: 1`）：团队名称和目标、成员的设置与关系、流程和触发设置。成员在包里用 `lead`、`member-1` 这样的标识互相指代，不带工作区内的编号；MCP 服务和技能库技能按名称记录，技能库技能连同最新版本的正文一起带上。包里不含密钥、运行记录和发布状态。
- `POST /v1/development/teams/import` 带 `request_id` 和 `package`，用团队包新建一个未发布的团队草稿，不覆盖已有团队，也不发布；包不超过 4 MiB，须有一位负责人和至少一位成员（合计最多 32 位），流程最多 20 条。同一请求重发返回同一个团队。
- 导入时，模型、执行节点、MCP 服务和技能库技能都在本工作区按名称查找：找不到的模型清空，节点按名称和引擎匹配，MCP 服务只在已启用且连通过时带上，同名技能库技能直接引用，没有的技能改为成员自带技能。每项被去掉或改动的内容都写在返回的 `adjustments` 里，首次创建时返回一次。
本轮验收（2026-10-16）：Mac管理端24文件151项检查、完整构建及独立PostgreSQL16的`make ci`通过；Compose与部署脚本检查通过。可见Chrome中的新建／导出、未保存禁用导出、跨工作区导入新草稿、缺失依赖提示及确认、重新导出读回和原团队保留已验证。补修无效流程引用的前置校验，拒绝文件不再留下团队，真实PG及页面回归通过。GitHub Actions被账号计费限制阻止启动；原生PG没有pgvector，对应既有条件跳过保留。桌面实际文件窗口与配套副本／锁同步由总仓PR继续验证，本轮未部署。

### 团队草稿以外的操作

归档团队、刷新业务动作目录，以及运行、执行节点、代码仓库、飞书和客户端会话的操作不属于团队草稿，由“工作区操作”提供，桌面助手与 MCP 入口共用：

- `GET /v1/development/workspace-operations` 列出类别；`GET /v1/development/workspace-operations/:topic` 给出该类每项操作的写法、是否需要确认、当前账号能否办理。
- `POST /v1/development/workspace-operations` 办理一项操作，请求体为 `{operation, confirmed, interactive, secrets, forge_token}`。每项操作都是以调用者本人的凭据调用管理端已经在用的接口，权限、校验和审计与那些接口一致；节点和代码仓库按名称引用。
- 不可撤回的操作（归档、移除、停止、收回、更换令牌、创建拉取请求）第一次调用只返回 `confirmation_required` 和后果说明，带 `confirmed: true` 再次调用才办理。
- 密钥不是操作的字段。操作在 `enter` 里写出要填写的密钥项，服务端向能弹出输入框的调用方（`interactive: true`，即桌面）返回 `needs_input`，由它收取后放在 `secrets` 里重发；操作产生的密钥（节点接入令牌和安装命令）放在 `reveal` 里与结果分开返回，由同一调用方显示。刷新业务动作目录需要本人的 Forge 登录，同样只由桌面提供。非交互的调用方办理这三种操作返回 `409 desktop_only`。

## 团队结果与试跑输出

团队按“完成／需要补充”交付的结构化结果，缺项条数和每段长度都不设上限，整个规范化结果不超过 256 KiB；超过时以 `result_too_large` 明确拒绝，不截断、不丢缺项。员工消息里的摘要最多 4000 字，被截短时结尾注明是预览，完整结果随续办上下文和本人打开的工作读取。

开发试跑每一步的输出都完整保存在 `weave_team_development_trial_output_bodies`。运行活动只给前 64 条、每条 64 KiB 预览，并标明全文是否保存；`GET /v1/runs/:id/trial-outputs?offset=N` 按顺序续读，`GET /v1/runs/:id/trial-outputs/:output` 读取一条全文，权限与运行活动相同。单条超过 16 MiB 记为保存失败，不当作完整输出。

成员在同一次运行中担任两个步骤时，补偿 CLI 事件只取当前步骤自己的执行记录。

## 并行分支里的多个步骤

一个并行分支可以依次有多个步骤（最多 16 步）：分支里的成员步骤用 `success` 边接到同一分支的下一步，最后一步用 `join` 边进入汇合，后一步可以读取前一步的结果。汇合只看到每个分支最后一步的结果，按分支第一步的标识归入汇合结果。整个分支在一个分支任务里依次执行；每一步完成后记入 `weave_fanout_leg_steps`，分支任务因失败或重试再次执行时，已完成的步骤不会再跑，它的工具调用和业务动作不会重复。受控修改 `step_add` 以 `placement: serial` 接在分支里的某一步之后，新步骤留在同一分支；管理端流程画布在分支步骤上提供“在本分支添加下一步”，把并行改为依次执行时各分支的步骤保持原来的先后顺序。

## 登录密钥

管理端登录页提供两种方式：账号登录（已接入 Forge 等账号系统时）和密钥登录。密钥登录除了接受签发的 API Key，还接受部署时设置的登录密钥：`WEAVE_LOGIN_KEY`。持有它的人以工作区管理员身份进入管理端，也可以把它作为 `Authorization: Bearer` 凭据调用 MCP 入口和其他接口。

- 没有设置 `WEAVE_LOGIN_KEY` 时，登录密钥是默认值 `admin`，从任何地方都可以使用。此时服务启动日志、登录页和登录后的每个页面都会提醒正在使用默认密钥。公网可达的部署应当设置自己的密钥。
- 登录密钥代表的管理员是 `WEAVE_LOGIN_WORKSPACE` 指定工作区里最早的管理员；没有指定时取整个部署里最早的管理员。还没有管理员的部署不能用登录密钥登录，需要先完成初始化。
- 登录密钥不保存在数据库里，改动环境变量并重启后立即生效，旧密钥随即失效。
## 在 Forge 里使用管理端

Forge原生Setup创建管理员及组织后即可进入管理端。身份交换从已验证会话的`activeOrganizationId`取得组织，沿既有持久绑定事务自动建立工作空间和员工映射；不要求先填写`WEAVE_FORGE_DEFAULT_WORKSPACE`或运行组织绑定命令。已有明确工作空间映射继续保持其原生组织边界。企业业务准备由管理员登录小满后办理，不影响Weave登录。

配置或身份服务不可用返回503，会话失效401，账号或组织无权403。管理端分别提示具体原因；部署信任配置仍由部署管理员维护，普通员工不输入服务或组织技术编号。

配置了 `WEAVE_ADMIN_FORGE_URL` 的部署，允许那一个 Forge 把管理端放进它自己的页面（Forge“系统设置 → 集成 → Weave 管理端”），其他任何页面都不能内嵌；没有配置时管理端不能被任何页面内嵌。

在 Forge 页面里打开时不需要再登录：管理端照常向 Forge 发起一次账号登录，把确认码交给外层的 Forge 页面，由它用当前登录人的 Forge 会话确认，随后的兑换、身份交换和注销与手动确认之后完全相同。两个页面之间只传确认码。

- 每次打开都按当前 Forge 登录人重新进入，不沿用原有的管理端会话；普通员工进入后仍看到“当前账号不能进入管理端”。
- 内嵌时先清除上一管理端会话；确认被拒绝、超时或设备登录不可用时，只回到登录页，不恢复上一账号。清除失败时停止进入，关闭页面会取消未完成的确认。
- 外层页面 8 秒内没有回应时（不在 Forge 里、Forge 没有启用设备登录等），回到管理端自己的登录页。
- Forge 与 Weave 须在同一站点下（同一主机的不同端口，或同一主域的子域），浏览器才会在内嵌页面里保留管理端的会话。

## MCP 入口

`POST /v1/mcp` 让外部智能体（编码助手、客户自有的智能体）开发和使用团队。传输为 JSON-RPC，提供 `initialize`、`tools/list`、`tools/call`；凭据与其他接口相同，放在 `Authorization: Bearer` 里。

它不带自己的业务逻辑：每个工具都是以调用者本人的凭据去调用管理端和桌面已经在用的接口，所以校验、权限和审计与那些接口一致。

- 开发团队（需要开发权限）：`weave_team_list`、`weave_development_read`、`weave_development_guide`、`weave_development_apply`、`weave_team_create`、`weave_development_trial`、`weave_development_trial_read`、`weave_development_publish`。草稿和运行按名称呈现；某一类修改的写法用到时再读。
- 团队草稿以外的操作：`weave_workspace_guide`、`weave_workspace_apply`，对应上文“团队草稿以外的操作”。要填写或显示密钥、或要用到本人 Forge 登录的操作不经这里办理；会让团队开始工作的操作（追加要求、发起代码任务）与交接工作共用每分钟 10 次的限额。
- 使用团队：`weave_team_search`、`weave_team_describe`、`weave_work_submit`、`weave_work_read`、`weave_work_cancel`。交接的工作不带业务动作授权，材料第一版只接受文本；一件工作只有提交人、管理员和所有者能读取，只有提交人能停止。

凭据有三种，任选其一：

- **账号登录。** 支持登录的 MCP 客户端（多数编码助手）只需填入口地址。客户端首次调用得到 401，按其中指向的说明（`/.well-known/oauth-protected-resource`、`/.well-known/oauth-authorization-server`）登记自己并打开浏览器；本人在 Weave 的页面登录——账号经 Forge 确认，或密钥——再按名称允许这个客户端，客户端即得到自己的会话。会话两周有效，只能用于 MCP 入口；本人的角色以登录时核实的为准，账号被停用或会话被收回后立即失效。回跳地址只接受本机地址和 HTTPS 地址。
- **登录密钥**或**签发的 API Key**：直接作为 `Authorization: Bearer` 凭据，不经过浏览器。

每个调用方每分钟可调用 120 次；其中会让团队开始工作的三个工具（试跑、交接工作、发布）另计，每分钟 10 次。调用方指客户端的会话；直接使用密钥时指这把密钥。超出时工具返回 `rate_limited` 并说明多少秒后重试。计数在单个服务进程内，多进程部署各自计数。

管理端“集成”页列出已登录的 MCP 客户端并可收回：每个人看到并收回自己的，管理员看到并收回本工作区全部的。对应接口为 `GET /v1/mcp/sessions` 与 `DELETE /v1/mcp/sessions/:id`。服务端只保存令牌摘要；已结束的会话记录保留，作为谁从哪个客户端登录、由谁收回的记录。
