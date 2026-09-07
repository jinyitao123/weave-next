# 已完成产物的 Ruby 依赖漏收

## 问题与修复范围

C 组真实模型实现通过 `run_tests.py` 调用 `derive_params.rb`，从冻结 YAML 派生参数。当前 Runtime 允许保存 Python、Shell、JavaScript 等源码，但 `.rb` 未包含在收集类型表中。最终模型答复没有把这个依赖作为显式单文件交付声明，运行仍显示 completed，回执仅保存 22/23 份文件。

只使用持久回执重建目录并执行 `python3 run_tests.py`，Ruby 报 `No such file or directory`，测试退出 1；原始工作目录执行相同命令退出 0。问题在文件保存链路，不能靠浏览器或模型自报通过发现。

修复只在 `internal/kernel/runtimes/artifacts.go` 增加 `.rb → text/x-ruby`，继续使用原有常规文件、UTF-8、单文件/总量、旧文件和路径约束。没有自动重试，没有自动执行下载的源码，没有改变任务身份或接入新恢复引擎。

## 复验

1. 用未修复收集器重新处理真实 C 组未修改的输出与最终答复，得到的 22 份文件逐份与原完成回执一致，确认复现相同缺口。
2. 修复后处理完全相同的输入，得到 23 份文件；唯一新增文件为 `model/derive_params.rb`，其余 22 份哈希不变。
3. 仅从修复后的回执重建全新目录，17/17 模型检查通过，退出 0。没有从原工作目录补文件。
4. 现有应用/模型/验证依赖跨 Runtime wire 测试加入 Ruby 依赖，修复前失败、修复后通过。最终版本 `make test`（含隔离 PostgreSQL）、`make depguard`、`make productguard` 已全部通过，见 `evidence/final-checks/`。

这是对真实产物的确定性保存缺口复验，没有新模型调用，不能作为模型性能或 Loom 收益样本。原 C 回执保持 22 份文件的历史状态，未事后修改。C 的 TNT 单位错误也仍存在，这个补丁只修复 Runtime 漏文件。

源证据见 `cases/C/evidence/independent/receipt-only-model-tests.json`、对应日志，以及 `evidence/patch-ruby/` 的前后收集回执、文件差异与测试记录。
