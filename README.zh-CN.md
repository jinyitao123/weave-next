# Weave

[English](README.md) | [中文](README.zh-CN.md)

**Weave 是 GooeyPi 桌面与 Forge 产品背后的团队编排和执行服务。**

员工和开发者使用 [GooeyPi 桌面加 Forge](https://github.com/jinyitao123/weave-workbench) 完成受支持的产品流程。Weave 负责团队与成员定义、已发布工作流版本、调度、执行状态、租约、额度、取消、恢复、用量和运行时接入。Forge 负责员工身份、业务记录与动作、权限、审批及最终业务状态。

`workbench/` 中的网页 Workbench 已退役，不是受支持的浏览器入口或独立业务客户端；其中代码仅作为迁移材料保留，直到删除。HTTP 与 MCP 接口用于产品集成和服务运维，不构成第二套员工业务界面。

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

产品开通、部署边界和当前验收状态见[产品总仓](https://github.com/jinyitao123/weave-workbench)。平台 Compose 文件用于开发与组件验证，不表示已退役的网页 Workbench 仍是受支持客户端。

[架构入口](docs/架构/README.md)导航 Weave 当前分层、契约和迁移证据。带日期的验收记录保留当时观察到的结论与限制，不能证明当前工作树或部署仍然通过。

## 许可

本仓使用 [MIT 许可证](LICENSE)。`workbench/` 保留其原有 MIT 许可证及该目录原始和衍生代码的第三方声明。
