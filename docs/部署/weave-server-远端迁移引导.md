# weave-server 远端迁移引导

这个一次性流程使用 `weave-next` 已有的受限部署 SSH 通道，在同一主机上建立
`weave-server` 独立部署入口。它不复制旧数据库、旧发布目录或旧服务配置，也不为
Windows 或旧 Host 增加兼容路径。

## 准备

新的 ed25519 私钥只保存在 `weave-server` 仓库的 `WEAVE_DEPLOY_SSH_KEY`
Secret 中。首次环境配置通过旧仓一次性 Secret `WEAVE_SERVER_BOOTSTRAP_ENV` 注入，
引导成功后立即删除。一次性引导只读取本机
`/Users/jinyitao/.config/weave-server-deploy/id_ed25519.pub`，把对应公钥作为 workflow
input 注入。旧的 `WEAVE_DEPLOY_SSH_KEY` 仍只用于本次连接，新公钥会被安装为单独的
forced-command 条目。同一个公钥不能同时绑定旧、新两个 forced-command。

工作流继续使用已有配置：

- Variables：`WEAVE_DEPLOY_HOST`、`WEAVE_DEPLOY_USER`
- Secrets：`WEAVE_DEPLOY_SSH_KEY`、`WEAVE_DEPLOY_KNOWN_HOSTS`

## 执行

1. 合并并正常部署包含本引导逻辑的 `weave-next` main 提交，使远端稳定部署脚本支持
   bootstrap 请求。
2. 在维护者本机运行 `./scripts/run-bootstrap-server-deploy.sh` 做只读检查；确认
   公钥和工作流可用后，运行 `./scripts/run-bootstrap-server-deploy.sh --dispatch`。
   该命令读取公钥文件并向 `Bootstrap weave-server deployment` 注入确认文本和公钥，
   不读取私钥。
3. 工作流通过已固定的 known_hosts 建立旧部署连接，安装新 forced-command、公钥、
   `~/weave-server-source`、`~/.local/share/weave-server-deploy` 和
   `~/.config/weave-server`。
4. 下载保留一天的 `weave-server-known-hosts` artifact，把其内容设置为
   `weave-server` 仓库的 `WEAVE_DEPLOY_KNOWN_HOSTS` Secret。
5. 确认 `weave-server` 仓库已经设置与该公钥匹配的 `WEAVE_DEPLOY_SSH_KEY`，再补齐
   Server 部署所需 Variables、GHCR 读取凭据和远端 `server.env`。

主机扫描结果只有在其公钥与旧仓已固定的 known_hosts 相符时才会成为 artifact。
工作流不会打印或上传任何私钥。artifact 是经过既有信任锚核对后的公开主机密钥，
不是登录凭据。

## 远端结果

引导从一次性 Secret 原子写入新的 `server.env`，同时生成 `required-settings.txt`，
不会读取或迁移旧配置。已有配置只有与输入逐字一致时才允许重复执行；不同内容会拒绝覆盖。首次 forced-command 调用会用
Server 仓短期令牌取得对应 main 提交，然后切换到 Server 仓自带的正式部署脚本。

新入口使用 OpenSSH `restrict` 和固定命令，拒绝交互 shell、PTY、端口转发以及非
`deploy FULL_SHA GHCR_USER`、`rollback FULL_SHA GHCR_USER` 的命令。首次正式发布前
不能回滚；之后回滚由 Server 仓保存的 release 目录负责。
