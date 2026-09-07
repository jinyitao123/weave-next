# 日冕计划 · 统一计算模型 (outputs/model/)

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **等级**: Level-0 / Pre-Phase A 概念级；不含施工/制造/飞行认证声明 [verified_fact]
- **批准情景**: 单一 0.03c（0.01c/0.05c 为 CCR-001 待确认变更，未纳入）[verified_fact]

## 文件

| 文件 | 说明 |
|---|---|
| `baseline_frozen.yaml` | 冻结基线（唯一参数源，SHA-256 与上游逐字节一致） |
| `derive_params.rb` | 受控派生 YAML→JSON，转换前核验 SHA-256，不匹配即失败退出 |
| `parameters.json` | 派生的机器可读参数（由 derive_params.rb 生成） |
| `corona_model.py` | 计算内核：任务书 §4.1 八类计算，公式见 ICD-FML-001..005 |
| `run_tests.py` | 自动测试运行器（有界、自行结束、无网络） |
| `output/` | 测试实际运行生成的结果 JSON |

## 运行

```bash
cd outputs/model
python3 run_tests.py
```

依赖：python3（标准库）、ruby（标准库 psych YAML）。命令自行结束，退出码 0=PASS，1=FAIL。

## 输出文件

- `output/reference_values.json` — 五项参考值（供 acceptance.json 容差比对）
- `output/model_results.json` — 八类计算完整结果（每项含单位/公式/输入/事实标签）
- `output/route_sensitivity.json` — 三路线敏感性比较
- `output/test_results.json` — 逐项 PASS/FAIL 与原始数值

## 边界与纪律

- 航行时间为匀速巡航下限，不含加速/减速/航向修正；区分“飞离太阳系”与“抵达目标恒星系统”。
- 动能仅为自身动能下限，**不得**表述为完整推进能源预算（未计效率/排气动能/推进剂/减速/备用/建造损耗）。
- 非相对论与相对论动能分列，不得混用。
- 克级光帆可行性不得外推至无人载荷或载人飞行器。
- 尘埃通量、粒径分布、推进效率、材料性能等缺直接证据项保留 [unknown]。
