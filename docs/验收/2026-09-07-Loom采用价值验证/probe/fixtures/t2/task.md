# 数字工程修复任务

该夹具来自 2026-09-06 日冕团队 `digital-engineering-builder` 的真实故障：

- Lorentz gamma 测试使用了精度不足的硬编码期望值。
- macOS 环境没有 GNU `timeout`。
- 验证脚本在模型目录已经是当前工作目录的条件下再次进入 `outputs/model`。

需要检查三个文件，修复这三个问题，运行完整验收并生成机器核对清单。
