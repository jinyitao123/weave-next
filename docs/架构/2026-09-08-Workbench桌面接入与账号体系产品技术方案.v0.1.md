# Workbench 桌面接入与账号体系产品技术方案

日期：2026-09-08  
版本：v0.1，供产品与工程评审  
状态：代码现状已核对；本文新增能力均为提案，未实现、未部署、未完成真实用户验收。

## 1. 结论与目标

目标体验是：用户安装 Workbench，打开后登录账号，直接进入工作台。用户未显式修改服务时，使用产品预置的 Weave 服务；改过服务后，后续启动始终尊重已保存的选择。

推荐产品形态为“远程服务优先的桌面客户端”。工作对话由服务端 Workbench Host 承载，Weave 负责业务任务、权限和执行调度，Runtime 负责实际执行。桌面端负责展示、连接选择、文件上传下载和必要的系统集成。浏览器与桌面端复用同一套工作台。

主要补齐内容有八项：稳定的公开服务入口、账号登录与设备会话、Workbench 按账号隔离、按用户身份调用 Weave、连接切换和版本协商、远程材料与成果访问、桌面打包更新、可验证的部署迁移。

这不是单独增加一个登录表单可以完成的改动。真正的完成标准是：两个账号不会串对话或凭据，切换服务器不会串任务，关闭客户端不会取消已派发运行，换设备可以继续访问已持久保存的工作。

首期面向受控部署中的具名用户与一个组织工作区，不开展开放注册、计费、多组织统一账号或离线执行。架构保留扩展位置，首期交付不包含这些能力。

## 2. 现状、证据与修正

本次核验基线为当前 main：da0e5c7eb5ba0f85c6381bd395418dba77f8aaf9。此前部署工作树停留于 6463fe605824f2a318e1ed6abf7150ae5d2e9d44。两者在本表涉及的认证、连接配置和 Compose 文件上基本一致；workbench-app/src/index.ts 有 5 行差异。本次没有重新探测生产运行状态，不能把本地代码状态写成服务器当前状态。

| 范围 | 本次核实的实现 | 对方案的影响 |
|---|---|---|
| 账号 | Weave 已有用户名密码登录、用户表、bcrypt 密码校验、用户禁用、改密码和管理员创建用户 | 复用现有账户基础；此前“需要补账号登录”的表述应收窄为“补 Workbench 账号接入及会话生命周期” |
| 认证 | /v1/auth/login 返回 JWT 与用户；JWT 当前有效期为 24 小时；/auth/refresh 依赖仍有效的认证重新签发 JWT | 已有续签接口，但不是完整的可撤销设备会话、轮换 refresh token 体系 |
| 当前用户 | /v1/auth/me；JWT 中间件读取用户表，检查禁用状态并读取现行角色 | 复用现有用户状态核验；不得另建互不一致的用户真相源 |
| 组织 | /v1/workspace、成员接口及 tenant_id 已存在，账户查询也带 tenant | 先保留现有 tenant 与组织工作区的关系；不顺手重构成全局身份平台 |
| Workbench 入口 | BrowserAuth 用进程启动 token 换取带签名的浏览器 cookie，cookie 绑定请求 authority | 证明“可以访问某个 Host”，不等于“已识别个人用户和组织权限” |
| Workbench 调用 Weave | workbench-app 在启动时读取 apiUrl/apiKey，缺省地址为 http://127.0.0.1:18080 | 地址、凭据是 Host 级配置，不能直接用于多账号客户端切换 |
| MCP | workbench profile 启动 weave mcp serve，通过进程环境传入 WEAVE_API_URL、WEAVE_API_KEY | HTTP、后台投影和 MCP 三条路径必须统一身份；不能只改页面请求 |
| 对话 | Workbench SessionHeader 有 id、cwd、父会话等字段，没有明确的 Weave 用户或组织所有者字段 | 不能把现有 Host 对话存储直接当作共享多用户库 |
| 工作目录 | Workbench workspace/create 以目录路径创建工作区；会话和目录选择发生在 Host 上 | 桌面电脑上的路径不会自动变成远端可访问材料 |
| 模型 | workbench profile 的前台模型缺省是 unconfigured，用户配置后才有可用模型 | 只接上账号、API 和 Runtime，仍不足以保证首次对话可用 |
| 节点就绪 | 已有 teams/runtimes 检查；节点数和发布工作流数用于粗略判断 | 必须进一步验证具体任务的能力、资源与调度可行性 |
| 部署 | Compose 中 Workbench、Weave、PostgreSQL 分开运行，Runtime 是可选 profile | 可延续服务分工；新增入口和 Host 分配不能破坏既有 Runtime 协议 |
| 桌面发布 | 当前 apps 只有 cli 与 web，未发现已交付的 Electron/Tauri 桌面工程 | 桌面端属于新工作，不能报告为已有安装即用能力 |

源码依据见附录 A。已有 tenant 过滤、角色和成员机制不等于全链路隔离已经通过；附件、导出、事件流、任务命令、模型设置等仍须逐路验证。

## 3. 产品对象与边界

| 对象 | 产品含义 | 身份或归属规则 |
|---|---|---|
| 服务连接 ServiceConnection | 用户连接的某个 Weave 部署 | 公开 HTTPS origin 加持久 instance_id；域名负责定位，instance_id 负责识别部署 |
| 用户 User | 在该服务中登录的人 | 首期沿用现有 tenant_id 与 user_id；同名账号在不同实例上互不相干 |
| 组织工作区 Workspace | 成员、团队、运行节点和业务数据的权限范围 | 当前 Weave tenant_id 对应的组织范围；权限由服务端判定 |
| 项目 Project | 某项持续业务及其材料、任务与成果的归类 | 延续 Weave project_id，不用本机目录名充当项目主键 |
| 工作目录 WorkingDirectory | Workbench Host 或 Runtime 上的文件位置 | 当前 Workbench 目录型 workspace 是这一层，不直接等同于组织工作区 |
| 工作对话 Conversation | 用户监督和调整任务的持久对话 | 绑定 instance、workspace、owner_user、conversation_id；首期默认个人可见 |
| WorkTask | 从工作对话关联到 Weave 的持久业务任务 | 保留已有运行、恢复、交付事实；换连接不改任务归属 |
| 登录会话 AuthSession | 某账号在某设备或浏览器上的登录状态 | 可过期、续期和撤销；不承担业务运行生命周期 |
| Workbench Host | 提供对话 Agent、存储、MCP 与页面服务的后端进程 | 首期按用户与组织工作区隔离，位于服务器 |
| Runtime | 接受调度、运行团队成员的执行节点 | 使用节点凭据；与个人登录会话独立 |

产品界面统一使用“服务”“组织工作区”“项目”“工作目录”。内部暂存的历史 workspace 命名可以渐进调整，但协议和授权中必须显式说明是哪一层。

三个长期原则：

1. 数据属于确定的服务与组织。切换连接不会自动迁移或混合数据。
2. 对话可以继续监督任务，但任务事实以 Weave 为准。不能因为桌面窗口关闭而推导运行取消。
3. 客户端访问权限、监督 Agent 的工具权限、Runtime 的执行权限分别授予，不互相替代。

## 4. 用户旅程和页面设计

### 4.1 安装到首次进入

1. 安装完成，显示 Workbench 品牌和启动状态。安装包包含默认服务地址，不包含业务 API key、账号密码或节点 token。
2. 客户端读取连接选择，检查该服务的公开描述和客户端兼容性。
3. 没有有效登录时展示用户名、密码和“登录”。当前服务用简短名称展示，次级入口是“切换服务器”。
4. 使用现有账号认证。首期账号由管理员创建；不把后端首用户引导接口变成公开“注册”按钮。
5. 登录成功后确认组织成员身份。只有一个组织就直接进入，不让用户填写 tenant_id。
6. 恢复最近项目、对话和已持久草稿。组织尚未配置模型或可用节点时，进入工作台并显示可操作的准备事项。

产品默认地址应当是运营方控制的稳定 HTTPS 域名。上一轮部署所用 IP 可以作为过渡环境的内部记录，不能作为长期公开发布契约。本文不假定某个尚未提供的域名已经拥有或解析成功。

### 4.2 再次打开与多设备

- 有效登录直接进入最近工作；后台核验账号和权限。
- 到期或被撤销时回到登录页，保留所选服务；重新登录后恢复已持久保存的上下文。
- 第二台电脑登录同一服务、同一账号，可看到同一份服务端对话和任务记录。
- 未上传文件、仅存在本机且未同步的草稿，不承诺跨设备出现。草稿必须标明“已保存”或“仅此设备”。
- 首期不做双人同时编辑同一对话；同账号多窗口提交使用 conversation revision 与幂等键，冲突返回明确提示。

### 4.3 连接设置

“设置 → 服务连接”包含服务名称、地址、登录账号、连接状态、客户端/服务端版本，提供切换服务、退出账号、恢复产品默认服务。

选择优先级是“用户最近保存的选择 → 安装包默认服务”。开发环境覆盖不进入普通用户设置。企业强制固定地址属于后续 managed policy，不能暗中覆盖用户选择。

切换过程先检查候选地址，确认它是可支持的 Weave 实例，再登录目标服务。取消或目标失败时保留原连接；成功后再原子提交新的活动连接。

旧连接的事件订阅、在途读取和页面缓存退出活动状态。切换中的迟到响应依据 connection_generation 丢弃，不能覆盖新服务页面。新服务的账号 cookie、缓存、下载记录按连接隔离，不转发旧服务凭据。

切换服务不取消旧服务已派发任务，不把旧任务在新服务重新派发。不自动回退到本地或产品默认服务。存在尚未确认结果的写操作时，记录操作身份并在原服务对账，页面明确标出“提交结果待确认”。

### 4.4 日常任务闭环

用户在工作对话中提交业务材料和目标，确认方案后派发到 Weave。节点不足、需要补材料、人审问题、可恢复失败和最终成果返回该对话。执行详情按需展开，不要求用户理解 Host、JWT 或 MCP。

普通用户登录后不需要配置平台连接 API key。前台监督模型应由组织管理员准备好，团队执行模型仍按已有运行配置解析。两者都检查可用性，不因为“后端健康”而显示“可以开始工作”。

### 4.5 退出、关窗与断网

| 用户动作或故障 | 客户端行为 | 服务端行为 |
|---|---|---|
| 关闭窗口/退出应用 | 关闭订阅，保存已接受的草稿 | 已接受的业务运行继续；不凭空生成取消请求 |
| 退出账号 | 撤销本设备 AuthSession，清除该账号活动页面与会话 cookie | 停止该登录会话发起的新操作；已派发运行继续 |
| 断网 | 显示“连接中断”及最后更新时间；允许本地草稿，禁用未经确认的远程写入 | 运行按服务端状态继续；是否可恢复由现有执行机制决定 |
| Token/会话过期 | 尝试一次受控续期；失败后要求登录 | 不无限重试，不使用全局 API key 兜底 |
| 账号禁用/成员移除 | 终止访问，不展示旧缓存中的业务详情 | 新请求拒绝，长连接关闭，相关监督权限撤销；运行处置遵循组织运维规则 |
| 用户明确停止任务 | 对指定 run 提交停止命令，显示请求与实际停止结果 | 复用已有 cancel/draining/terminal 机制，保留幂等与精确目标 |

“关窗后继续”只承诺已被服务器持久接受的工作，不承诺未发送文本自动执行，也不扩张为所有尚未完成的恢复能力已经可靠。

## 5. 部署形态选择

| 路线 | 优点 | 需要承担的成本 | 本次判断 |
|---|---|---|---|
| A. 桌面展示客户端 + 远程 Workbench Host | Web/桌面一致；会话跨设备；桌面退出不带走监督进程 | Host 账号隔离、服务端存储、运维容量；本地文件须上传 | 推荐默认路线 |
| B. 桌面内置完整 Workbench Host + 远程 Weave | 本机文件和工具自然可用；个人 Host 易隔离 | 分发 Node/Go/插件依赖；对话跨设备需另做同步；休眠影响监督；本机能力授权复杂 | 未来确有本地工作需求时作为独立增量，不与 A 同期做 |
| C. 桌面直接调用 Go API，重写对话与工具层 | 客户端服务关系直观 | 重做已存在的监督 Agent、Session、MCP、事件投影和交互层 | 当前不采纳 |

推荐 A 是根据当前服务端已部署 Workbench、现有 Host 能力和目标旅程作出的工程判断，并非已做过三路线性能对照。若产品的硬要求改为“直接修改用户电脑上的项目”，必须重新评估 B，不能在 A 中偷偷开放任意本地执行。

### 5.1 桌面框架

暂推荐 Electron 作为首个实现候选：可复用现有 TypeScript 工程经验和浏览器交互，首版承担窗口、连接选择、下载与更新。Tauri 作为同阶段小样对照候选，其 API 能力边界需显式配置；不因包体积印象直接定胜负。

首次技术验证应对比同一工作台的启动、内存、中文输入、流式长对话、上传下载和安装更新。达到产品阈值后固定一种框架，不维护两套正式桌面端。平台尚未由用户指定，计划暂以当前设备适配的 macOS Apple Silicon 为试点，Windows x64 为后续矩阵；这不是替用户认定发布平台。

Electron 远程页面关闭 Node integration，启用 contextIsolation 与 sandbox；preload 仅暴露经过消息来源和参数校验的少量系统操作，不暴露文件任意读取、shell、完整 IPC 或 cookie 读取。官方依据见附录 B。这些是所选远程页面架构的实施条件。

## 6. 目标技术架构

~~~mermaid
flowchart LR
  D[Workbench 桌面客户端] --> E[统一 HTTPS 入口]
  W[Workbench 浏览器] --> E
  E --> G[账号与 Workbench 接入层]
  G --> I[Weave 用户 / 设备会话 / 权限]
  G --> H1[用户 A 的 Workbench Host]
  G --> H2[用户 B 的 Workbench Host]
  H1 --> B[带主体身份的 Weave 调用桥]
  H2 --> B
  B --> C[Weave 业务 API 与调度]
  C --> R1[远程 Runtime 1]
  C --> R2[远程 Runtime 2]
  C --> P[(PostgreSQL 与业务成果)]
  H1 --> S1[(A 的对话与材料卷)]
  H2 --> S2[(B 的对话与材料卷)]
~~~

图中的接入层、设备会话和用户 Host 分配是新增能力。Workbench Host 与业务 Runtime 是不同进程职责；增加 Host 实例不等于增加团队执行节点。

### 6.1 公开入口

客户端只配置一个公开 origin。统一入口在同一 origin 下提供登录页面、Workbench 页面、API、事件流及受控下载，不要求普通用户区分 3080 和 8080。

新 Runtime 的安装/注册地址也从这份服务端公开入口配置派生。内部容器地址 http://weave:8080 可以继续用于服务间通信，但不能出现在外部节点安装参数中。Workbench 列出的节点必须属于当前实例和组织，不能因为另一台服务有在线节点就认定当前服务可执行。

新增公开 GET /.well-known/weave-workbench 描述实例身份、显示名称、协议版本、支持能力与最小桌面版本。返回的路径默认限制在相同 origin。初版不接受描述文件任意指定其他域名接收登录凭据。

服务描述字段提议如下：

| 字段 | 含义 |
|---|---|
| schema_version | 服务描述自身格式版本 |
| instance_id | 数据库初始化时生成并持久保存；不得每次容器重启改变 |
| display_name | 用户可读的部署名称 |
| public_origin | 用户访问入口，与受控反向代理配置一致 |
| workbench_path / auth_path | 同源页面路径 |
| api_contract_version | API 能力协商版本，不等于 Git commit |
| min_desktop_version / recommended_desktop_version | 硬性兼容下限与建议版本 |
| capabilities | account_sessions、workspace_access、uploads、hosted_workbench 等显式能力 |

连接选择保留原输入和规范化 origin。拒绝 URL 中的用户名密码、未知协议、片段和不支持的子路径；禁止跟随跨 origin 跳转携带凭据。HTTP 只允许独立开发模式的本地回环地址，正式自建服务也使用有效 HTTPS。

### 6.2 网关与内部路由

接入层先认证当前用户及组织成员关系，再以服务端计算出的 host_key 路由到 Host。浏览器提交的 host_id、owner_user_id、tenant_id 不能决定访问权限。

公开入口不暴露 Host 的内部端口、启动 token 或文件系统地址。所有 /api RPC、WebSocket、附件、静态之外的下载和导出都走相同认证边界。只保护首页是不完整的实现。

内部 Host 继续使用现有 Connection/Gateway 协议，但账号型 Workbench profile 注入新的身份验证 provider，取代外部启动 token 登录。个人开发 profile 的现有 BrowserAuth 可保留；两种入口显式配置，正式服务不设置 token 兜底。

对代理身份使用可信通道或短期签名断言，绑定 instance、user、workspace、auth_session、host_instance、audience 与有效期。入口剥离客户端伪造的内部头。Host 只接受接入层来源，并再次检查断言与自身固定归属一致。

## 7. 账号与登录会话设计

### 7.1 首期采用第一方 Web 登录

桌面薄客户端与浏览器打开同一服务的第一方登录页面，由服务端设置不可由页面脚本读取的登录 cookie。首期不把 JWT 或 refresh token 交给桌面渲染进程，不额外建设 OAuth 授权服务器。

这条方案与未来“桌面原生组件持有独立 API 授权”区分明确。后者若纳入，使用系统浏览器授权及 Authorization Code + PKCE，遵循 RFC 8252，不把薄客户端 cookie 流程包装成已经实现 OAuth。

### 7.2 复用与新增

复用 weave_users、bcrypt、用户启停、现有角色和组织成员存储。新增服务端设备会话表，为浏览器/桌面建立可撤销的 opaque session；浏览器只保存高熵随机会话凭据，服务端保存其摘要。

首期通过专用 /v1/auth/sessions 路由实现账号型 Workbench 登录，内部复用现有 Authenticate。现有 /v1/auth/login、/auth/refresh JWT 契约先保留给已存在调用者，不能无版本地修改返回格式。

| 提议接口 | 语义 |
|---|---|
| POST /v1/auth/sessions | 用户名密码认证并建立 cookie 会话；返回用户展示信息，不返回平台 API key |
| GET /v1/auth/session | 返回当前用户、可用组织信息、会话到期时间与必要能力 |
| POST /v1/auth/session/renew | 延长仍允许续期的设备会话，受绝对有效期限制；登录态失效后重新登录 |
| DELETE /v1/auth/session | 幂等退出当前会话；即使已经撤销也返回成功 |
| GET /v1/auth/sessions | 查看该用户自己的登录设备 |
| DELETE /v1/auth/sessions/:id | 撤销该用户的指定设备；他人设备不可操作 |

会话 cookie 使用 Secure、HttpOnly、Path=/、host-only 属性和适当 SameSite 策略。采用 __Host- 前缀时不得设置 Domain。写请求校验 Origin 并使用 CSRF 防护，不能把 SameSite 当作唯一保证。cookie 凭据不放 URL、日志、页面状态、模型上下文或共享配置。

登录、个人会话、业务记录与私有文件响应默认禁止共享缓存。若客户端持久缓存业务内容，必须包含实例/组织/用户命名空间及退出清理策略；首期优先不做离线业务缓存，不能仅依赖页面切换隐藏旧内容。用户主动保存到本机的成果是明确导出的文件，退出账号不会擅自删除，下载时说明保存位置。

建议试点有效期为绝对 30 天、空闲 7 天，非“记住登录”会话由会话 cookie 控制浏览器生命周期。此数值是产品提议，需结合试点确定，不是现有实现。每次受保护请求都校验 session 撤销、账号禁用和当前组织权限；JWT 旧接口不提供新会话的撤销语义，必须分别说明。

新登录防止 session fixation。续期/轮换采用事务更新版本，同设备多标签竞争要有统一结果；若未来轮换 cookie secret，必须设计响应丢失和并发续期的有限恢复窗口，不能因为一次响应丢失锁死账号，也不能无限接受旧凭据。

### 7.3 撤销与长期连接

退出当前设备不退出其他设备。修改密码建议撤销所有现有设备会话，并为当前设备明确重新登录；禁用账号撤销所有个人访问。成员移除只取消相应组织访问。

WebSocket/SSE 建连时认证，运行中响应撤销事件，并设置周期性重新核验。试点目标为权限撤销后 10 秒内断开旧订阅；命令提交实时检查，不等待周期。撤销后内存页面、持久浏览器缓存和通知不再展示该账号详情。

### 7.4 首用户和成员加入

既有“用户总数为零时自动成为管理员”的逻辑只服务运维引导。正式部署使用已有管理员配置或一次性安装引导，且数据库事务/锁保证首管理员唯一，不开放空库被公网抢注册的路径。

首期由管理员在 Workbench 成员设置创建账号、分配成员角色，并提供受控重置密码入口。当前未确认存在完善的密码重置流程，因此它应进入实现清单。邮件邀请、邮箱验证、自助找回、SSO 和跨组织同一账号属于后续目标，不在登录页提供不可用按钮。

首期登录 realm 由部署方固定为该实例的试点组织，服务端据此复用现有 tenant 内认证，不让用户填写 tenant_id。后续多组织时再提供组织选择或统一账号映射；不能只按用户名猜组织，也不能默认当前 tenant 内账号就是跨组织全局账号。

## 8. Workbench Host 隔离及存储

### 8.1 首期隔离单位

采用每个 instance_id × workspace_id × user_id 一个逻辑 Host。多个登录设备复用同一逻辑 Host 的持久对话，但 AuthSession 各自独立。Host 的唯一活动实例由服务端租约保证，避免同一对话目录被两个进程同时写入。

首期物理隔离使用受控容器和独立数据卷，不仅给文件路径加用户前缀。单进程加筛选器不能隔离现有插件中的文件、设置、凭据、工具进程和全局注册表。

Host 只挂载自身对话、材料和必要只读发布物，不挂载全部用户数据、数据库管理凭据或 Docker socket。前台工具只能访问该隔离环境及被授予的业务接口。提示词“把工作派给团队”不能替代执行权限。

Host 管理器由受控运维进程负责，输入为已验证的逻辑 Host 标识和固定发布版本；不接受前端任意镜像、启动命令、挂载路径或环境变量。可放在 internal/app 的装配/运维边界，不引入上层依赖到 base/kernel。

### 8.2 对话所有权

新增 HostDirectory 与 ConversationBinding 等应用层元数据，记录用户、组织、Host、Session 和项目映射。Host 文件存储可以继续使用当前 JSONL 后端；不为本项目强制改造成 PostgreSQL 对话日志。

所有权从受认证主体写入，只读历史导入不得从文件夹名称或日志里的任意 user_id 自动授予。导出、附件引用、冷启动历史扫描和搜索都必须局限在对应 Host 卷。

对话私有与业务任务共享分开：首期个人对话不自动向组织所有成员公开，Weave 团队/项目/运行按既有服务端权限可见。增加共享对话时需要单独的 conversation membership，不能把知道 session_id 当作授权。

### 8.3 Host 状态机

逻辑状态：absent → provisioning → ready → draining → stopped；失败进入 failed，保留 retryable/reason。Provisioning 通过唯一 host_key 和租约去重。

关闭桌面端不会直接进入 stopped。活跃前台 turn、待确认写操作、后台工作持久化未完成时，不进行空闲回收。试点先限制具名用户数量并保持其 Host 常驻；空闲按需启动作为容量优化，在恢复测试通过后才打开。

恢复顺序为校验 Host 租约、加载自身数据卷、恢复持久对话、按 Weave 真相对账 WorkTask，再开放写操作。恢复不得重复派发已有业务任务。升级期间同一 Host 卷只允许一个写者。

## 9. Weave 身份传递与任务授权

### 9.1 一份主体上下文贯穿三条路径

新增 WeaveConnection/WeaveCredentialProvider 接缝，统一产出目标实例、组织、用户身份和当前请求凭据。workbench-app 的 HTTP 请求、后台 WorkTask 同步、weave mcp serve 的工具调用都消费它。

现有 apiKey 字符串在启动时冻结的做法不能承担可撤销个人权限。Go weaveclient 和 TypeScript 消费侧应能在每次请求时解析凭据；或通过受认证的内部代理统一代签，不能只给页面接入 cookie 而保留后台共用管理员 key。

首期推荐通过内部调用桥发放短期、限制 audience 与权限的 Host 委托凭据。委托绑定固定 user/workspace/Host，基于现行权限检查；它不是该用户所有设备的 refresh token，也不是不受限制的 API key。正常个人读写由来源 AuthSession 和当前成员资格约束。

MCP 子进程沿用已有 stdio 协议。通过受控本地代理或动态 credential provider 获取本次调用授权，避免每次续期重启全部 MCP 和前台 Agent。旧 WEAVE_API_KEY 配置只用于明确标记的单主体兼容/运维路径，不作为新账号版兜底。

### 9.2 监督进程与已授权运行

用户确认派发后，Weave 已保存的运行凭据和授权由执行系统管理，浏览器退出不撤销该次运行。每次新的派发、人工问题答复、停止、恢复或修改都重新校验当前主体权限。

监督 Agent 的一次已接受 turn 可以在窗口关闭后按已记录的 turn 授权继续，但不得超出原确认范围。退出/禁用时停止再接受个人写入；已提交结果未知的操作先对账。后台投影需要独立、可撤销的运行观察权限，限定已关联的 run，仅能读取状态和成果，不能借观察权限新派发任务。

业务运行失败恢复仍使用已有 run generation、attempt、lease 和幂等契约。本计划不新增第二套任务状态机，不借登录改造重写 Loom 或恢复机制。

### 9.3 权限矩阵的首期政策

| 行为 | 普通成员 | 管理员/工作区负责人 | 最终执行位置 |
|---|---|---|---|
| 读取自己的工作对话和草稿 | 允许 | 自己的对话允许；不默认获取他人私聊 | Host 归属校验 |
| 读取组织团队和允许访问的项目/运行 | 按现有成员与资源规则 | 按管理权限 | Weave API |
| 派发、停止、恢复、答复人工问题 | 仅授权资源和动作 | 同样检查目标与状态 | Weave API 事务/命令层 |
| 添加/撤销 Runtime | 默认不允许 | 允许的管理角色 | Weave API |
| 创建用户、修改角色、组织模型配置 | 不允许 | 对应管理权限 | Weave API/受控设置接口 |
| 切换服务、退出本设备 | 允许 | 允许 | 桌面设置/账号服务 |

这张表是目标政策，需逐接口对齐现有角色检查，不能把它当作当前每个 endpoint 已实现的证明。不存在明确资源规则的动作先收窄到管理员，评审后再逐项开放，避免前端隐藏按钮代替后端权限。

## 10. 材料、成果、模型与节点

### 10.1 材料路径

桌面选中文件后，经认证上传到对应用户/项目材料区。服务端生成 material_id、内容摘要、版本、大小和存储引用；任务输入引用已经完成上传且权限可读的材料。远端 Runtime 获取可访问的快照或受控下载地址。

桌面绝对路径只作为本地展示信息，不作为远端读文件命令。现有 Host 目录选择在远程模式下标注“服务器工作目录”，普通成员只能访问其材料区。首期无需同步整个本机目录；提供文件和有限目录批量上传即可。

上传必须有状态和幂等键，重试不能制造重复材料版本。首期可以先实现有大小上限的单文件上传；大文件断点续传在需求真实出现后纳入，不在界面假装支持。

### 10.2 成果路径

成果列表延续 Weave 的 final deliverable 事实，显示可预览/可下载文件和版本。下载经过任务/成果授权，禁止把远端绝对路径直接交给桌面“打开文件”。“保存到本机”由客户端选目标位置并报告成功或失败。

上传成功不等于 Runtime 已获取；运行 completed 不等于最终交付可用。验收必须证明材料摘要一致、执行节点可访问、成果下载后内容可打开。

### 10.3 默认可用性

首次进入的就绪信息拆成 service、identity、membership、host、supervisor_model、team_workflow、runtime_capacity、materials 八项。某项不满足只阻断依赖它的动作，不让用户陷入整页空白。

节点“在线”不等于能接当前任务。任务派发前校验引擎能力、可用槽位、模型或工具依赖及材料可达性。现有 readiness 的计数和 health 判断保留为概览，不能充当最终 dispatchable 判据。

组织管理员设置监督模型默认值。当前 Host 自带的 Models/settings 能改主机配置，账号化后需要区分用户偏好与组织凭据；普通成员不可通过通用 settings RPC 覆盖组织密钥。管理员配置及更新事件与用户 Host 的受控配置投影衔接，密钥由服务端凭据提供方解析。

### 10.4 本机作为 Runtime

默认安装 Workbench 不启动本机执行服务，也不自动登记节点。完整目标包含“设置 → 执行节点 → 启用此电脑”：说明可访问目录、引擎依赖、并发及休眠行为，经明确启用后领取独立节点凭据。

首期只管理远程已注册节点。后续本机节点需要处理系统服务、自动启动、节点撤销、资源占用、睡眠、断网和 Runtime 协议兼容；桌面账号退出与节点停止是两个明确动作，界面分别显示其状态。

## 11. 桌面连接与异常状态机

主状态为 booting → resolving_service → checking_compatibility → signed_out/restoring_session → entering_workspace → ready。异常是 service_unreachable、incompatible、session_expired、access_denied、host_unavailable，均有独立文案和可用操作。

| 场景 | 必须保持的事实 | 出口 |
|---|---|---|
| 新安装没有保存地址 | 使用发行包默认值 | 默认服务可用则显示登录；不可用则重试/切换 |
| 用户曾保存自建地址 | 更新后仍使用该地址 | 只在用户明确恢复默认时替换 |
| 自建服务不是 Weave | 不发送账号密码、不进入工作台 | 指出服务类型/协议不支持 |
| 证书或域名错误 | 不忽略证书错误、不携带旧 cookie 跨域跳转 | 修正地址或由部署方修复 |
| 同一地址返回不同 instance_id | 视为部署身份变化，不合并缓存和会话 | 清理旧活动认证、重新建立连接并登录 |
| 服务端升级导致不兼容 | 不继续提交无法理解的命令 | 保留可用上下文，显示更新客户端入口 |
| 登录成功但 Host 启动失败 | 保持账号有效，不循环登录 | 重试进入工作台或展示运维可定位的错误编号 |
| 提交后断线 | 状态为“结果待确认”，不直接显示失败或重发 | 用原操作幂等键查询原服务结果 |

桌面每个服务使用独立持久会话分区；切换账号时清理账号相关缓存、通知和页面状态。访问服务描述可先用不携带 cookie 的请求检查 instance_id，再载入该分区。instance_id 是部署标识，不替代 TLS 信任校验。

客户端不后台维护所有曾连接服务的业务订阅。首期只展示当前活动连接；旧服务运行的结果在下次切回后同步，避免未经设计的跨服务通知混杂。

## 12. 数据结构与模块改动

### 12.1 新增元数据的建议形状

以下是逻辑模型，迁移文件序号在实现时按当前 migrations 分配，不预占旧序号。

| 模型 | 关键字段与约束 |
|---|---|
| service_instance | 单实例持久 ID、公开 origin、显示名称、契约版本；备份恢复策略明确是否沿用原实例身份 |
| auth_sessions | id、tenant_id、user_id、secret_hash、device_label、created_at、last_seen_at、idle_expires_at、absolute_expires_at、revoked_at、version；密钥摘要唯一 |
| workbench_hosts | host_key 唯一、tenant_id、user_id、state、release_id、volume_ref、lease_owner、lease_epoch、lease_expires_at、last_error_code |
| workbench_conversations | tenant_id、owner_user_id、host_key、conversation_id、project_id、revision、created_at；组合唯一；不是重复存储 Weave run |
| host_delegations | Host 身份、user/workspace、用途、权限范围、到期和撤销版本；不得客户端自填授权 |
| materials | 尽量复用已有资源/成果存储；补所有者、摘要、版本、上传状态与任务引用，不无条件再建一套材料系统 |
| client_connection_preferences | 本地 connection_id、origin、instance_id、显示名、最近 workspace、用户选择标记、schema_version；无账号密码和业务 token |

### 12.2 改动位置

| 层/文件 | 保留 | 调整或新增 |
|---|---|---|
| internal/app/users/store.go | 用户表和密码校验 | 管理员重置流程、账号撤销事件衔接；避免第二份用户表 |
| internal/app/api/auth.go、middleware.go、server.go | 既有 JWT/API key 接口 | 新设备会话 API、cookie 认证、权限核验、撤销通知、路由清单 |
| internal/base/db/migrations | PostgreSQL 迁移机制 | 会话、Host 注册、对话归属等应用元数据的存储迁移 |
| internal/app 下新增接入/Host 管理装配 | 现有分层 | 身份代理、Host 唯一租约、发布物分配和内部路由；具体包名在实现设计冻结 |
| internal/app/weaveclient、internal/app/mcpstdio | 协议和工具语义 | 动态凭据或受控代理适配；主体可追踪；避免固定管理员 key |
| workbench/packages/client/connection | Connection 和 RPC 框架 | 提取可替换认证 provider；账号模式的 HTTP/升级请求一致校验 |
| workbench/packages/api/gateway | 已有 Remote/事件流 | 携带可验证主体和撤销；覆盖 stream/result 等旁路 |
| workbench/packages/bundle/workbench-app | WorkTask 投影、命令与交付读取 | 注入 WeaveConnection；去掉账号模式中的全局凭据闭包；补归属与 readiness |
| workbench/packages/api/session-controller、workspace-controller | 会话及目录能力 | 账号 Host 绑定、目录范围限制、个人对话授权和多设备写入冲突 |
| workbench 的 settings、credentials、attachments 相关包 | 现有扩展接缝 | 私有/组织配置分离，材料权限与受控模型提供方 |
| workbench/packages/client 下新产品 UI 包 | Slots、locale、主题规范 | 登录/账号、连接、成员管理与远程材料界面；准确标注目录位置 |
| workbench/apps/desktop（拟新增） | web 业务 UI | 桌面窗口、连接偏好、持久分区、受控下载、安装与更新 |
| docker-compose.platform.yml、部署脚本 | 现有数据库/Weave/Runtime 服务关系 | 统一入口、Host 模板、持久卷、健康检查和发布版本兼容 |
| .github/workflows 与发布文档 | 已有 Go/镜像检查 | 桌面平台构建、签名、安装升级验证、兼容矩阵 |

所有 Workbench 依赖仍放在 workbench，保持固定 pnpm 版本和根 Makefile 验证入口。新增 desktop launcher 需要在仓库入口规则中被明确识别为 Workbench 产品入口；不恢复独立 DSH 品牌或业务 CLI。

当前 AGENTS.md 仍有迁移期不加功能的边界。本次只产出方案；实施时把账号化与桌面化登记为新的产品阶段及明确范围，不用修改边界来顺带加入计费、SSO 或第二套调度。

## 13. 部署、版本与迁移

### 13.1 发布版本

中心服务、Workbench Host、桌面应用、Runtime 分别记录版本及 commit，但用显式契约版本判断兼容。初期维护当前正式桌面版本和前一受支持版本的兼容矩阵；破坏性变更必须声明最低版本。

不能要求所有桌面用户跟随 GitHub main 每次提交更新。服务端 main 的自动部署应先经过包含客户端兼容的验证，再提升版本；桌面通过签名的稳定发行渠道更新。服务描述提供兼容信息，不能任意指定可执行文件下载后直接运行。

桌面更新源默认绑定发行方，由发行配置控制；切换自建业务服务器不会自动更换应用签名信任根。自建方分发品牌安装包则承担自己的签名和更新渠道，属于后续发行模式。

### 13.2 迁移现有单 Host 数据

1. 盘点现有账号、API key owner、DSH_HOME、会话、目录、模型配置和运行引用；备份数据并记录校验摘要。
2. 指定现有单 Host 数据的实际拥有者及组织。没有证据的历史数据进入待归属状态，不公开给所有新账号。
3. 在独立预发环境创建该用户 Host，恢复数据并核对对话/任务/成果关系。
4. 为第二名用户创建空白隔离 Host，验证无法读取第一名用户的对话、设置、附件和目录。
5. 新入口启用账号模式，固定旧 Host 的写入边界，防止两个实例同时写同一数据卷。
6. 验收新链路后撤销新产品路径使用的共享业务 key 和外部启动 token 入口；保留有明确用途的运维 key，不无差别删除凭据。

回滚以兼容版本和数据副本为单位。新版本写入不兼容会话格式后，不能直接让旧 Host 打开同一卷；应先阻断写入、保存新数据、恢复兼容快照并说明数据时间边界。优先前向修复，避免声称任意降级都无损。

### 13.3 运维最小闭环

除 /health、/ready 外，要能验证真实账号登录、Host 分配、MCP 的用户主体、模型一次实际响应、测试节点执行和成果下载。日志关联 request_id、instance、workspace、user、host、conversation、run 与版本，不记录密码、cookie、Authorization 或原始材料。

Host 容器是该架构新增的容量成本。试点先测单 Host 空闲/活跃内存、冷启动、5 个具名用户的并发，再决定上限。不得依据理论值声称当前服务器能承载多少人。

## 14. 完整目标与首期边界

| 能力 | 首期必须有 | 后续完整目标 |
|---|---|---|
| 默认连接 | 稳定 HTTPS 默认入口、用户改址持久保存 | 企业受管配置、多个已保存服务 |
| 账号 | 现有账号登录、退出、续期、设备撤销、管理员建号/重置 | 邀请、自助找回、SSO、跨组织统一账号 |
| 数据归属 | 一组织内具名用户隔离、跨设备读同一服务端对话 | 显式共享对话、组织切换与细粒度协作 |
| 桌面 | 一个试点平台的真实安装包、连接/登录/下载/更新 | 其他平台、系统通知、深链接 |
| 材料 | 受控上传、任务引用、成果下载 | 大文件断点续传、本机目录同步 |
| 执行 | 远程 Runtime 跑完一项真实业务任务 | 可选本机 Runtime，明确目录与资源授权 |
| 离线 | 状态说明及本机草稿，不重放未知命令 | 如有需求再设计缓存/冲突/离线工作 |
| 扩容 | 少量用户、隔离 Host、容量上限 | 按需启动、跨主机 Host 调度和共享存储 |

首期最终出口包含桌面安装和真实交付；前面的账号 Web 里程碑只代表一个中间结果。不能用“登录成功”替代整个首期完成。

详细工作包、验收用例、资源估算、失败出口和评审记录见配套[实施与验收计划](../计划/2026-09-08-Workbench桌面接入与账号体系实施验收计划.md)。

## 附录 A. 当前源码依据

- [Weave 账号与 JWT](../../internal/app/api/auth.go)
- [认证中间件](../../internal/app/api/middleware.go)
- [用户存储](../../internal/app/users/store.go)
- [API 路由装配](../../internal/app/api/server.go)
- [组织工作区接口](../../internal/app/api/org.go)
- [Runtime 列表与凭据](../../internal/app/api/runtimes.go)
- [Weave HTTP 客户端](../../internal/app/weaveclient/client.go)
- [MCP stdio 适配](../../internal/app/mcpstdio/server.go)
- [Workbench profile](../../workbench/packages/bundle/workbench-app/cordis.patch.yml)
- [Workbench 业务投影与连接](../../workbench/packages/bundle/workbench-app/src/index.ts)
- [就绪检查](../../workbench/packages/bundle/workbench-app/src/readiness.ts)
- [浏览器 Host 认证](../../workbench/packages/client/connection/src/browser-auth.ts)
- [Connection 入口](../../workbench/packages/client/connection/src/index.ts)
- [Gateway](../../workbench/packages/api/gateway/src/index.ts)
- [SessionHeader](../../workbench/packages/core/session/src/types.ts)
- [工作目录控制器](../../workbench/packages/api/workspace-controller/src/index.ts)
- [Workbench 扩展架构](../../workbench/docs/architecture.md)
- [平台部署](../../docker-compose.platform.yml)
- [工作对话既有方案](2026-09-05-Workbench-工作对话方案.md)

## 附录 B. 外部技术依据

以下为 2026-09-08 查阅的官方资料。它们支持所列机制，不证明本项目已经完成相关实现。

- [Electron Security](https://www.electronjs.org/docs/latest/tutorial/security)：远程页面的 Node integration、上下文隔离、IPC 与导航控制要求。
- [Electron Context Isolation](https://www.electronjs.org/docs/latest/tutorial/context-isolation)：preload 与页面上下文分离及受控桥接。
- [Tauri Capabilities](https://v2.tauri.app/security/capabilities/)：本地 API 与远程页面能力授权边界；作为框架小样评估依据。
- [RFC 8252](https://www.rfc-editor.org/info/rfc8252/)：未来原生 API 授权采用外部浏览器与 PKCE 的依据；首期第一方 Web 登录不宣称实现此协议。
