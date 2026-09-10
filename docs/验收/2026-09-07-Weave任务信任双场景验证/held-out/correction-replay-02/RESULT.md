# 成员级纠偏真实运行证据

本目录记录 2026-09-10 通过内置 Workbench 派发的冻结本地文本样本。主运行 `run-02401d26-78a3-5025-b3d4-da6462140992` 在父运行停于 fanout 后，对数据计算员提交一次成员纠偏，并在 Workbench 显示的具体影响范围中确认应用。

结论为 `PASS_WITH_QUALITY_FINDING`：数据计算员新增一个物理任务，独立复核员没有新增任务；独立复核员的任务 ID、产物 ID 与文件哈希保持不变。数据计算员修订文件仅发生两处指定变化，最终交付 5/5 检查通过，成本未超过模板预算，评价绑定到精确交付版本并记录为“可以采用”。

限制：该样本的 `external_effects=none`，不能替代日冕外部材料恢复或外部副作用 exactly-once 验证。负责人在协调节点生成了角色式中间文件，属于单独的职责边界质量缺陷，不影响物理重放对账。首个样本 `run-52698216-dad3-51c6-ae86-f00ba484ff73` 因观察开始晚于 fanout 窗口而未提交纠偏，只作为时序失败记录，不纳入机制判定。

关键证据：

- `verification-summary.json`：机器结论与边界。
- `correction-ready.json`：确认前的恢复计划和物理身份。
- `final-activity.json`：应用后的完整运行活动。
- `artifact-comparison.json`：初始、修订、兄弟与最终文件的元数据、内容和 SHA-256。
- `final-delivery.json`：交付合同与 5 项检查。
- `user-assessment.json`：精确交付版本的采用评价。
