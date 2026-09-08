---
description: "保存桌面服务选择并拒绝迟到响应，保持 Host 和执行协议独立。"
kind: "package-library"
---

# 桌面连接选择

[English](README.md) | 中文

## 概要

桌面源码库保存服务选择，并阻止旧连接的响应更新当前页面。可信适配器提供服务身份与认证结果，注入的存储只保存非敏感偏好。该源码库没有应用启动入口、包注册或生产调用方。

## 目录

- [使用源码库](#use-this-library)
- [理解实现](#understand-the-implementation)
- [进一步阅读](#further-exploration)
- [模型体验](#model-experience)
- [已知限制与后续工作](#known-limitations-and-deferred-work)
- [开发备注](#dev-note)

-----

<a id="use-this-library"></a>
## 使用源码库

在桌面源码目录中导入 [ConnectionSelection](src/connection-selection.ts)，提供独占的原子偏好存储和首次启动默认地址。向 `select` 传入可信的服务描述与认证适配器，成功时得到不可变的连接范围。适配器或存储失败时保留已保存的选择；存储损坏会明确报错，需要存储负责方恢复数据或按用户选择重置设置。

每次操作前捕获连接范围，更新当前页面前检查 `isCurrent`。开始切换会使旧范围失效，切换结束前不能捕获新范围；取消或候选失败后可以为此前的活动身份重新捕获范围；如果在活动 origin 检测到实例被替换，则清除就绪状态并保留固定偏好，等待处理。`disconnect` 清除本地就绪状态并保留服务选择。旧业务回执仍须归属原服务保存，本库不能取消或对账服务器工作。

在 `workbench` 目录使用现有固定版本工具链执行下列检查，无需注册新包。

```sh
./node_modules/.bin/tsc -p apps/desktop/tsconfig.json
./node_modules/.bin/vitest run apps/desktop/tests/connection-selection.spec.ts
./node_modules/.bin/oxlint apps/desktop/src apps/desktop/tests
```

-----

<a id="understand-the-implementation"></a>
## 理解实现

<details>
<summary>实现职责</summary>

选择代数只属于当前对象，与现有 Host 传输代数分开。保存的偏好仅含版本、origin 和预期实例。连接范围绑定其创建对象，A→B→A 或替换对象后，旧响应不会重新生效。该策略不含 HTTP 解析器、凭据、业务状态或重试派发器。[HTTP 夹具](tests/fixture-service.ts)解析自己的 `/fixture/` 响应，并在测试清理阶段关闭监听器。

</details>

-----

<a id="further-exploration"></a>
## 进一步阅读

[决策记录](../../.agents/notes/implemented/architecture/2026-09-08-desktop-connection-selection.zh.md)解释了选择策略与传输独立的原因。[消费侧清单](../../../docs/验收/2026-09-08-Workbench桌面并行准备/消费侧契约清单.md)区分源码已有字段和待接入要求。

<a id="model-experience"></a>
## 模型体验

无。本源码库不注册工具、事件、提示词或产品界面。

<a id="known-limitations-and-deferred-work"></a>
## 已知限制与后续工作

- 注入存储必须保证原子写入；本库不提供抗崩溃磁盘持久化或跨进程协调。
- 可信适配器须隔离候选凭据、拒绝跨 origin 重定向、重新核对认证实例并完成中止后的网络清理；客户端身份比较不能证明服务器授权。
- 这里只记住选中的服务。历史操作身份与回执由业务操作负责方保留，本库不会重放它们。
- Electron、系统凭据存储、安装更新及真实账号与 Host 接入均不属于本源码库。

<a id="dev-note"></a>
### 开发备注

<details>
<summary>维护上下文</summary>

[并行任务计划](../../../docs/计划/2026-09-08-Workbench桌面接入并行任务计划.md)维护接入顺序和后续工作。

</details>
