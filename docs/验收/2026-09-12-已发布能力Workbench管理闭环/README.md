# 已发布能力 Workbench 管理闭环验收

日期：2026-09-12
状态：3C 管理切片通过；不代表完整故障矩阵或业务结果验收通过。

实现提交：`248c118c`

## 验收环境

- 独立工作树：`/Users/jinyitao/Developer/weave-next-capability-service`
- 独立 PostgreSQL 工作区：`capability-browser-acceptance`
- 本地 Weave：`http://127.0.0.1:18081`
- 本地 Workbench：`http://127.0.0.1:3099`
- 页面语言和尺寸：简体中文，`1440 × 1000`

浏览器通过 Workbench 的真实 Host 页面操作。Host 使用自身管理员凭据访问 Weave；脚本和证据文件不保存 Host 凭据或一次性应用密钥。

## 页面操作与读回

1. 从 Workbench 设置进入“已发布能力”。
2. 页面创建稳定调用应用，签发带 `invoke/read/cancel` 范围的一次性密钥，并关闭密钥提示。
3. 页面创建能力，从已准入的“订单材料检查流程”发布不可变版本 1，结果只允许返回 `summary` 与 `issues`。
4. 页面把该版本授权给刚创建的应用。
5. 应用密钥通过公开服务接口提交一笔结构化订单材料调用，服务返回 HTTP 202 和稳定 Invocation。
6. 页面刷新后看到该调用，并从页面提交停止原因。
7. 使用同一应用密钥查询，读回 `execution_status=cancelled`、`cancellation_status=confirmed` 和 `result_availability=unavailable`。

[结构化读回](evidence/ui-lifecycle.json)记录应用、能力、版本、授权、调用身份和终态；[页面截图](evidence/ui-lifecycle.png)显示真实管理入口、版本授权、一次性凭据使用状态和已停止调用。截图在密钥提示关闭后采集。

页面文本复核未出现调用输入中的订单号与材料，也未出现凭据哈希。管理快照保留最近调用、执行状态、结果可用性和停止确认，调用输入与存储密钥不进入浏览器模型。

## 工程验证

- `make workbench-check`：1 项 Workbench profile E2E 及 403 项 bundle/client/layout 测试通过，生产构建和 TypeScript 检查通过。
- `TEST_DATABASE_URL=... make test`：仓库规定的 Go 范围全部通过，能力管理的真实 PostgreSQL 生命周期和管理员取消用例实际执行。
- `make depguard productguard budgetguard`：分层、产品边界和确切代码预算通过。

这一轮没有运行真实模型，所以它只证明管理、协议、持久化与浏览器操作闭环。无工具真实引擎、两个业务的最终结果和 F01–F14 全矩阵仍按实施计划继续验证。
