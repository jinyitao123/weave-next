# Weave 管理端

面向开发者与运维的网页管理端：节点、环境、团队、任务及其证据。产品范围与页面设计以总仓 [Weave管理端设计](https://github.com/jinyitao123/weave-workbench/blob/main/docs/architecture/Weave管理端设计.md) 为准，依据为决策 003。管理端不承载业务记录、审批、员工待办和组织管理。

## 打包方式

构建产物复制到 `internal/app/adminui/dist`，由 `go:embed` 打进 Weave 服务端二进制，在 `/admin` 与接口同源提供。未构建时二进制照常启动，访问 `/admin` 返回"未随本次构建打包"。

```sh
make admin-install   # npm ci
make admin-check     # 类型检查与测试
make admin-build     # 构建并复制到嵌入目录；之后再 go build
```

镜像构建（`Dockerfile`）先构建管理端，再编译服务端。

## 本地开发

先启动本地 Weave（默认 `http://127.0.0.1:8080`），再在本目录运行 `npm run dev`；开发服务器把 `/v1` 转发到 `WEAVE_URL`，保持同源 Cookie。

## 登录

- 默认入口 `/admin/` 使用 Forge 管理员或开发者账号。配置 `WEAVE_ADMIN_FORGE_URL` 为浏览器可访问的 Forge 地址；未配置时页面提示配置不足，不自动切换为 API Key。浏览器直接向 Forge 登录，密码不经过 Weave；Forge 需要把管理端地址加入可信来源并允许跨域。拿到的 Forge 会话只用于一次交换，随后释放。
- 运维明确使用 `/admin/?login=api_key` 时显示既有 API Key 表单，密钥仍由 `weave bootstrap` 签发。默认页面不提供登录方式切换；后端 API Key 功能、角色和 scope 边界不变。

登录成功后，凭据只保存在 HttpOnly、SameSite=Strict 的同源 Cookie 中；页面脚本无法读取。所有写请求必须带 `X-Weave-Admin: 1`，跨站来源一律拒绝。

## 主线归并与验证边界

2026-10-09 将候选 `0c374067` 的管理端归并到含飞书修复的来源主线 `158b4965`。成员角色不能访问管理端任务或代码环境接口；开发者和管理员沿既有角色及 API Key scope 校验办理。主线身份自查、退休接口边界、取消处理和默认服务端不安装 CLI 的部署分层保留。

代码环境与原生任务准入在同一事务冻结，请求指纹同时固定环境存在性、环境及分支和跟进来源。相同请求读回原冻结上下文，环境编辑不改变已提交任务；更换上下文的同号请求拒绝且不追加代码环境或执行记录。节点先检出实际交付 HEAD，再核验证命令前后的 HEAD、提交树、索引和已跟踪文件；发生变化时证据不能判为验证通过。

原候选未部署的 `0178` 至 `0183` 六项管理端迁移顺延为 `0179` 至 `0184`，保留主线 `0178_feishu_integration.sql` 原样。没有改写已部署迁移。

本轮完整 Go 检查、管理端类型检查及 9 项前端用例通过；隔离 PostgreSQL 下的真实 HTTP 权限／请求重放、git 交付树、代理、跟进与原飞书用例通过竞态检查。本机 Compose 配置检查因缺少 Docker CLI 未通过，需由具备该工具的 CI 核对。该证据只证明组件与边界；真实 Claude／Codex 双节点网页闭环、生产部署和业务结果均未在本轮验证。
