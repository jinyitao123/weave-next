# 日冕计划 Pre-Phase A · 统一计算模型（digital-engineering-builder 交付）

**基线**：`corona-prephase-a-v1`（v1.0.0，`frozen_for_validation`）
**速度轴**：`0.01c / 0.03c / 0.05c`（S-0.01c / S-0.03c / S-0.05c）
**负责人**：digital-engineering-builder（数字产品与工程图开发员，runtime `41233bee-…`）
**交付日期**：2026-09-09
**单一事实源**：`model/params.json` == `model/baseline.yaml`（= inputs/lead/baseline/baseline.yaml，self-contained 副本）

> 本目录是 v4 复验的**统一计算内核**，其数学与 `../../app/model.js` 完全同源（见 `verification/` 的交叉一致性检查）。所有专业、图纸、应用的派生量必须来自此处，不得自行定义常量或情景。

---

## 1. 文件清单

| 文件 | 作用 |
|---|---|
| `corona_model.py` | 计算内核：常数、三情景派生函数、参考锚点复算 |
| `params.json` | 机器可读参数（= `baseline.yaml`；含派生用到的 AU/透镜/前驱/Starshot 锚点） |
| `baseline.yaml` | 冻结基线 self-contained 机器可读副本 |
| `anchors_reverified_2026-09-09.txt` | 本轮真算的 0.03c 五项参考锚点证据（承lead） |
| `mini_yaml.py` | 无第三方依赖的 YAML 子集解析器（用于 baseline.yaml 一致性比对） |
| `tests/test_model.py` | `python3 -m unittest tests.test_model`，覆盖常数/情景/锚点/派生/一致性/事实纪律 |

---

## 2. 使用

```bash
cd outputs/model
python3 -m unittest tests.test_model        # 模型测试（全绿即 `Ran N tests OK`）
python3 corona_model.py                     # 打印三情景全派生量 + 参考锚点复算（ALL_OK）
python3 mini_yaml.py baseline.yaml          # 基线字段视图
```

所有命令**有界退出**（0=通过 / 非 0=失败），不会启动任何前台开发服务器。

---

## 3. 公式（与 app/model.js 逐式同源）

| 量 | 公式 | 标签 |
|---|---|---|
| `V_KMPS(CS)` | `CS · c / 1000` | derived_result |
| `TIME_ALPHA(CS)` | `4.25 ly / CS`（yr；匀速、未计加减速） | derived_result |
| `TIME_GLENS(CS)` | `550 AU / (CS · LY_M / AU_M)`（yr） | derived_result |
| `TIME_PRECURSOR(CS)` | `1000 AU / (CS · LY_M / AU_M)`（yr） | derived_result |
| `E_SAIL_1G(CS)` | `0.5 · 1e9 kg · (CS·c)²`（J，经典式） | derived_result |
| `E_5MT(CS)` | `0.5 · 5e9 kg · (CS·c)²`（J） | derived_result |
| `E_DUST_1MG(CS)` | `0.5 · 1e-6 kg · (CS·c)²`（J） | derived_result |
| 人工重力 a | `(2π·rpm/60)² · r`（m/s²） | derived_result |
| 1 g₀ 转速 | `√(g₀/r) · 60/(2π)`（rpm） | derived_result |
| TNT 当量 | `E / (4.184e6 J/kg · 1000)`（吨） | derived_result（基于 assumption） |
| `P_REL(CS)` | `(0.2 / CS)²`（相对 0.2c 束动力比） | derived_result（缩放律为 assumption） |

> 相对论动能（经典式的修正度量）：`(γ−1)·m·c²`；0.05c 时修正 <0.2%，故经典式可接受（derived_result）。

---

## 4. 参考锚点复算（acceptance.json，0.03c，容差内）

`model/reference_check()` 对五项锚点逐项算相对误差并判容差；全部 OK 时 `all_ok=True`。
证据见 `anchors_reverified_2026-09-09.txt`（max rel_err 2.58e-8）。

| 锚点 | 期望 | 容差 |
|---|---|---|
| `travel_years_at_0_03c` | 141.6666667 | 0.001 |
| `kinetic_energy_1mt_at_0_03c_j` | 4.0443983e22 | 0.01 |
| `kinetic_energy_5mt_at_0_03c_j` | 2.0221991e23 | 0.01 |
| `gravity_1km_2rpm_m_s2` | 43.8649 | 0.01 |
| `dust_1mg_0_03c_j` | 40443983 | 0.01 |

---

## 5. 真值标签与事实纪律

- 派生量标记 `derived_result`；`0.2c/100GW/克级` 为 Starshot **B 级锚点**（`assumption`/`verified_fact(量级)`）；TNT 当量、光帆功率随 v² 缩放的**缩放律**为 `assumption`。
- **未知项显式保留**：推进效率、尘埃通量、降低成本证据等缺直接证据 → `unknown`，不得用假设掩盖。
- **禁用语**：`construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed` 在本模型与全部产物中禁止出现。

---

## 6. 边界（如实说明）

1. 本模型为 **Level-0 / Pre-Phase A 概念先期论证**，不构成施工/制造/飞行认证级依据。
2. 本节点**未修改** `corona-prephase-a-v1` 参数（未引入基线变更，符合 CR-002 约束 6）。
3. 11 项硬门槛的**独立复跑**属 verification-integrator（runtime `84da6261-…`），本节点不代做最终判定。
