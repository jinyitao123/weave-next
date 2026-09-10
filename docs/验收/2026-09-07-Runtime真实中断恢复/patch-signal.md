# 原生进程被杀的原因被早期警告覆盖

M2 已通过 17 项模型测试，随后对真实 CLI 进程组注入 SIGKILL。Runtime 正确保留了 9 份失败产物，但 `finishClaudeRun` 优先采用 stderr 上早期的模型别名警告，遮住了操作系统的进程终止状态。原错误被 `ClassifyFailure` 归为不可重试的工作错误，见 `cases/M2/evidence/failure-classification-before.json`。这个错误分类会妨碍现有恢复入口对基础设施失败作正确判断。

修复先检查真实 `exec.ExitError` 的 Unix `WaitStatus.Signaled()`，为进程信号退出生成 `runtime_process_interrupted:` 原因，再使用原有失败分类入口。普通非零退出仍沿用原行为，仅在 stderr 出现 “signal: killed” 不足以判定进程中断。Windows 没有这项 Unix 信号证据，保持不推断。

显式取消与截止时间仍优先于信号判断，防止用户停止被误当成可恢复故障。已有任务身份的 Executor 仍拒绝自动重派；分类为可重试只描述失败性质，不授权盲目重复有副作用的执行。

## 验证

- 使用真实子进程先写入无关 stderr 警告，再执行 SIGKILL。修复前错误被警告覆盖，回归失败；修复后正确保留进程中断原因。日志为 `evidence/patch-signal/red-test.log` 和 `green-test.log`。
- 普通退出 1 并输出 “signal: killed” 的反例保持工作错误；显式 context 取消保持取消语义；已有任务身份不触发自动重派。
- engine、teamrun、runtimes 定向测试与最终全量 `make test` 均通过；全量测试设置了本次隔离 PostgreSQL。分层与产品边界检查通过，Windows engine 分支交叉编译通过（未在 Windows 执行）。

此复验是真实 OS 信号驱动的适配器回归，模拟 CLI 的短脚本不调用模型。没有在修复后另跑完整成员场景，不把它称为第二次真实模型中断验收。M2 历史回执与故障前后产物保持原样。修复没有恢复私有 CLI 上下文，也没有把失败产物自动挂入下一次执行。
