# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave 是 GooeyPi 桌面与 Forge 产品背后的团队编排和执行服务。**

员工和开发者使用 [GooeyPi 桌面加 Forge](https://github.com/jinyitao123/weave-workbench) 完成受支持的产品流程。Weave 负责团队与成员定义、已发布工作流版本、调度、执行状态、租约、额度、取消、恢复、用量和运行时接入。Forge 负责员工身份、业务记录与动作、权限、审批及最终业务状态。

已退役的网页 Workbench 代码已删除。HTTP 与 MCP 接口用于产品集成和服务运维，不构成第二套员工业务界面。

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
