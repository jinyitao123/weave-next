# Weave Runtime 维护界面

Workbench 是任务协作的产品入口。本目录仅保留部署运维需要的运行节点维护界面，不再承载聊天、收件箱、项目、任务派发或控制中心。

保留的能力包括工作区账户登录、会话恢复与退出，运行节点列表、注册、名称与资源池配置、删除，以及节点连接状态和引擎能力。浏览器显示安装指令；Tauri 桌面壳另提供本机运行时进程的启动、检查与停止。

前端 API 仅消费 `/v1/auth/login`、`/v1/auth/refresh`、`/v1/auth/me` 和 `/v1/runtimes` 及其节点子路径。Workbench 所需业务 API 的实现仍由 Go 服务负责。

## 开发与检查

```sh
npm ci
npm run dev
npm run lint
npm run check:tokens
npm run build
```

开发页默认监听 `127.0.0.1:5173`，`/v1` 代理到 `127.0.0.1:8081`。使用 `VITE_WEAVE_PROXY_TARGET` 修改开发代理目标；`VITE_WEAVE_API_URL` 可指定浏览器直接访问的 API 地址。构建产物是 `dist/`，部署入口由仓库根目录的构建与容器配置统一管理。

桌面壳使用 `npm run tauri dev`。修改 Rust 部分后运行 `cargo fmt --check` 和 `cargo check --locked`。原有凭据存储身份、运行时令牌环境变量和运行工作目录保持不变；退出桌面壳仍清理它启动的本机运行时进程。

旧业务页面、路由、专用 API 客户端、聊天流处理、Markdown/Mermaid 阅读依赖和旧前端页面验收脚本已退出维护。任务与成果的界面验收在 Workbench 进行。设计 token 检查继续覆盖运行时界面，`app.css` 的既有基础样式字面值仍保留明确豁免。
