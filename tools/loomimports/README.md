# Loom 引用边界

`make depguard` 同时检查 Weave 各包对 Loom 执行接口（图引擎 `loom`、`loom/stdlib`、`loom/pgstore`、`loom/provider/*`）的直接引用。`loom/contract` 是共享的消息与工具词汇，不计入。

目标边界：团队编排留在 Weave，单个成员的循环、检查点、重放和终态以 Loom 为准，二者只通过成员执行器端口与 Loom 适配包相遇（见总仓整改方案 W8）。现在有 30 多个包直接引用 Loom，所以这里不是一步到位的禁令，而是只减不增的机器检查：

- `baseline.json` 记录当前每个“包、引用种类”下引用它的文件数。新增引用 Loom 的包、新增引用种类、或文件数增加都会失败；引用被移除后必须同步降低基线，不保留可重新使用的额度。
- 新代码需要驱动一个成员运行时，走成员执行器端口或 `loomruntime`、`loomadapter` 这样的适配包，不在业务包里直接构图。

```sh
go test ./tools/loomimports
go run ./tools/loomimports -inventory
make depguard
```

完成一个收口批次后，审查输出并仅降低或删除已解决项。扫描只读 import 声明，排除 `_test.go`。
