# 验证探针

本目录只用于维护者验收，不是新的业务入口。

不调用模型的证据复核，直接运行 `python3 docs/验收/2026-09-06-Loom替换验证/probe/verify_evidence.py`。它读取已归档的输入、事件、进程身份和检查点记录，并重新生成 `evidence/verification-summary.json`。

重新开展内核实验需要已配置的 Claude CLI、可写 PostgreSQL、Python 3 和仓库 Go 工具链。调用会产生真实模型用量。将连接信息通过进程环境传入，不写入报告或命令行参数。

1. 将 `LOOM_PROBE_ROOT` 指向一个新建的独立实验目录；将 `LOOM_PROBE_BASELINE` 指向要复算的真实基线文件。启动 `mcp_server.py`，服务仅监听 `127.0.0.1:18193`，验证工具实际读取该文件。
2. 按当前主机配置设置 `LOOM_PROBE_DATABASE_URL`、`LOOM_PROBE_CLI_PATH`、`LOOM_PROBE_CLI_VERSION`。数据库应已有 Loom 的 `loom_store` 表。探针只使用 `checkpoint:loom-probe:*` 命名空间。
3. 在仓库根目录以文件路径构建探针，例如 `go build -o "$LOOM_PROBE_ROOT/probe" docs/验收/2026-09-06-Loom替换验证/probe/main.go`。中文目录不适合作为 Go 包导入路径，应指定文件。
4. 使用 `probe <实验目录> <case> <phase>` 执行下表。每次调用是独立进程；`crash` 预期退出 86，不能被外围脚本当成普通构建失败后跳过恢复验证。

| case | phase 顺序 | 预期 |
|---|---|---|
| `toolloop` | `run` | 模型驱动三个真实工具调用后结束；原生 CLI 工具调用会使实验失败 |
| `mem` | `crash` → `resume` | 计算检查点后退出；新进程找不到内存检查点 |
| `pg` | `crash` → `resume` | 保存的普通检查点可读取，但最后一步计算再次执行 |
| `pgsafe` | `crash` → `resume` → `fork` | 显式 after_step 安全点恢复不重复工具；从复核检查点分支，只重新汇总 |

`main.go` 使用仓库现有 `runtimellm` 与 `engine` 适配器；为单独检验内核，在本机承接推理进程。它不通过工作流发布器，不能用其成功掩盖产品冻结路径的失败。工具服务、步骤拆分与故障注入均为本实验提供。

事件文件包含真实工具回执和进程 ID。分支记录另外检查源历史检查点字节哈希前后相同。所有数据留在实验目录，进程环境和凭据不写入记录。实验完成后停止本次工具服务。
