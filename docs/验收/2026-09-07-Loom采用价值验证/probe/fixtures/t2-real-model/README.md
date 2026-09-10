# 日冕计划 · 统一计算模型（outputs/model/）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **职责节点**: luna-digital-engineering-builder（计算内核 / SVG图纸 / Web应用 / 可编辑概念模型）
- **级别**: Level-0 / Pre-Phase A 概念级 [verified_fact]（不声称施工级、制造级或飞行认证级设计）
- **批准情景**: 单一 0.03c 巡航速度（baseline 1.0.0 锁定；0.01c/0.05c 为 CCR-001 待确认变更，模型不预先实现）

## 组成

| 文件 | 说明 |
|---|---|
| `baseline_frozen.yaml` | 唯一参数源（自上游 mission-lead 节点复制整合，SHA-256 与冻结登记一致）[verified_fact] |
| `corona_model/miniyaml.py` | YAML 子集加载器（本机无 PyYAML，仅用标准库；子集外语法报错而非误解析） |
| `corona_model/baseline.py` | 基线加载 + SHA-256 实际核对 + 追溯键 + 情景锁定防呆 |
| `corona_model/physics.py` | 规范公式 ICD-FML-001..005（与应用 `app/calc.js` 逐式对应） |
| `corona_model/computations.py` | 任务书 §4.1 八类计算 |
| `run_model.py` | CLI：运行八类计算 → acceptance.json 容差比对 → 写 `model_output.json` 与 `../app/data/baseline_params.json` |
| `tests/` | unittest 自动测试（基线/公式/八类计算+容差/纪律/CLI 契约） |
| `model_output.json` | 由 `run_model.py` 生成的本轮计算结果（含追溯键与容差比对结果）[derived_result] |

## 运行（有界命令）

```bash
cd outputs/model
python3 -m unittest discover -s tests -v   # 自动测试，正常路径有界退出
python3 run_model.py                        # 生成 JSON；参考值超差时退出码 1
```

## 边界纪律 [verified_fact]

- 动能仅为飞行器自身动能下限，未计效率/排气动能/推进剂/减速/备用/建造损耗，不得表述为完整推进能源预算（RQ-MDL-004）。
- 航行时间为匀速巡航值，不含加速、减速与航向修正；"飞离太阳系"与"抵达目标恒星系统"为两种任务终态（RQ-SCP-004）。
- 三路线可分离，禁止跨路线可行性外推（RQ-SCP-003）；先锋探测器 0.2c 为其路线内部工况（ICD-RTE-003）。
- 效率网格、路线代表质量等假设值均标 [assumption]/[unknown]；旧稿"1mg≈450MJ"已撤回，模型不复现。
