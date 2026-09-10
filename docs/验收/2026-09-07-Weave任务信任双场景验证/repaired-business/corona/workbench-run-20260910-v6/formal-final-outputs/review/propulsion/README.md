# 日冕计划 Pre-Phase A · v6 正式交付修订轮 · 推进 / 能源 / 热控域 —— 复核后复用说明与域入口（INDEX）

**节点**：`corona-prephase-a-recovery-v6-propulsion-energy-analyst`（推进/能源/热控复核）
**职责**：推进比较 · 能源预算 · 热控约束 · 技术成熟度（本域的推进/能源/热控子集）
**本轮动作**：复核冻结 `outputs/review/propulsion/` → **原样复用**；运行有界自动检查；未发现新的可复现缺陷，未做任何修改。
**唯一素材（只读）**：`recovery-materials/2026-09-10-corona-v6-corrected-source/outputs/review/propulsion/`
**权威验收**：`acceptance.json`（SHA-256 `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`）
**外部副作用**：`none`；所有文件仅写入本节点 `workdir/outputs/`。

> 本 README 即本域的**入口 / 索引**，供 `verification-integrator` 汇总进根级 `outputs/review/REVIEW_INDEX.md`。本域**非最终验收**；十一项硬门槛（G1–G11）的最终逐项判定属独立验证与总装员。

---

## 0. 复用 / 修改自述（自我声明）

- **结论：复用（修改 = 0）。** 本轮对冻结源 `review/propulsion/` 逐项核对后**原样复用**，未发现新的可复现缺陷，未改写、未重做、未顺带重构。
- **冻结源已修正的 DEF-01**（v4 版陈述「最大相对误差 2.58e-8」为**错误**，2.58e-8 实为**次大**；正确最大值为 **1.93e-7**，来自重力锚点）：该修正已在冻结源中改妥（见 §3 附）。本节点复用其**修正版**，无需再改。
- **本节点不代做**：统一模型（`model-owner`）、数字应用与图纸（`digital-engineering-builder`）、其他专业评审包（systems/physics/archive/cost_risk）、根级 `REVIEW_INDEX.md` 与 `FINAL_ACCEPTANCE.md`（`verification-integrator`）。

---

## 1. 交付件清单（相对本节点 `outputs/` 的真实路径）

| 相对路径 | 内容 | 处理 |
|---|---|---|
| `review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` | 推进/能源/热控复核报告（比较 + 成立条件 + 终止条件 + 验证清单 + DEF-01 修正） | 复用（SHA 与 manifest 一致） |
| `review/propulsion/07_corona_v6_pet_comparison_matrix.csv` | 三速度推进/能源/热控比较矩阵（机器可读） | 复用（SHA 与 manifest 一致） |
| `review/propulsion/scripts/pet_check.py` | 有界自动检查脚本（单线程/有限次数/无循环/退出即止，**未启动任何服务**） | 复用（SHA 与 manifest 一致） |
| `review/propulsion/checks/pet_check_output.txt` | 本轮**真实重跑**输出（与冻结计算内容逐字节一致，仅尾行 `[written]` 为本节点 workdir 路径） | **本轮重跑生成** |
| `review/propulsion/checks/pet_results.json` | 机器可读结果（与冻结逐字节一致） | 本轮重跑（内容与冻结一致） |
| `review/propulsion/README.md` | 本入口/索引 + 复用自述 | 本轮新建 |

### 1.1 复用件逐项核验（SHA-256，对照冻结源 `manifest.json`）

| 相对路径 | SHA-256 | 与 manifest 一致 |
|---|---|---|
| `review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` | `e5398fb56bfa1da46e9e12843694cf05a5152378bd24e34c114020f1da4f3901` | ✅ 一致 |
| `review/propulsion/07_corona_v6_pet_comparison_matrix.csv` | `6d5db2029da2a36be4be1b2a753ee07e9dac9973d0c599dd430f1d33b2be624b` | ✅ 一致 |
| `review/propulsion/scripts/pet_check.py` | `c125a7f2f3b539e7a96f34fc893ff512e50d5d795f3852f4c52d0becf9aa8bf6` | ✅ 一致 |
| `review/propulsion/checks/pet_results.json` | `91027dc77f03c4f80b0f090a7edae1e283a62e1381827c67e63dc379e1f95e55` | ✅ 一致 |
| `review/propulsion/checks/pet_check_output.txt` | 本轮重跑；计算内容与冻结逐字节一致，尾行 workdir 路径随本节点而异 | 计算内容一致（尾行除外） |

> 冻结源 `manifest.json` 中被测清单**不含 `__pycache__` / `*.pyc`**；本目录同样不含任何运行缓存（已实测）。

---

## 2. 本轮有界自动检查（已实跑，退出即止）与可复现性

- **命令**：`python3 review/propulsion/scripts/pet_check.py`（单进程、有限次数、无循环、无长驻服务；本机无 `timeout`，脚本自身即设计为有界即退）。
- **退出码**：`0`。
- **参数源**：脚本 `BASE` 指向冻结基线 `recovery-materials/2026-09-10-corona-v5-source/v4-outputs/model/baseline.yaml`（real path 存在，SHA-256 `09351b046422984b…` 与权威基线一致）。
- **可复现性**：本轮重跑输出与冻结 `pet_check_output.txt` **逐字节一致**，唯一差异为尾部 `[written] …workdir` 路径行（反映本节点自己的 workdir）。`pet_results.json` 逐字节一致。
- **未启动任何服务**：已确认（脚本无循环、无服务启动逻辑、退出码 0）。

---

## 3. 成立条件 / 终止条件 / 验证清单（本域专业判据层）

下列条目**位于** `06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` 的 §4 / §5 / §6；此处仅列编目与**本轮重跑后的判定**，完整论述与真值标签见报告。

### 3.1 成立条件（Establishment Conditions，需**同时**满足）

| 编号 | 条件（子域） | 本轮判据 |
|---|---|---|
| E-1 | 束功率近端可获（推进） | `P_REL` 0.25–6.25 GW，量级最宽松为 0.01c–0.03c |
| E-2 | 帆束流热生存（热控） | 平衡温度随 α/ε/A 变化；A=100m² 且 α=5% 时 0.05c 仍 2352 K，**为第一道物理门槛** |
| E-3 | 微尘撞击可防护（能源/热控） | 1mg 撞击 0.03c=40 MJ、0.05c=112 MJ；依赖 ISM 尘密（未核） |
| E-4 | 速度成就能回报（推进） | 到透镜区/前驱区 63.5 天–19 个月，三速度均满足 |
| E-5 | 相对论修正可忽略（能源） | 修正率 7.5e-5–1.88e-3（<0.2%）成立 |
| E-6 | 载人尺度不纳入近端回报（推进） | 到比邻星 85–425 年，超出单次任务尺度 |
| E-7 | 物质级动能不误作来源（能源） | 1mt 动能 1.07e6–2.69e7 Mt TNT 为**束供给**结论 |

**推荐成立档位**（本域视角，供总装/负责人合成）：路线 A 以 **0.01c–0.03c** 为近端先导原型最易成立；0.05c 为延伸档；路线 C（载人）在推进/能源/热控层**不支持**作近端回报。

### 3.2 终止条件（Termination Conditions，触发任一则终止/降档）

| 编号 | 触发条件 | 本轮判定 |
|---|---|---|
| T-1 | 束功率超可行阵列规模（量级 > 数十 GW） | 未触发（量级提示，待 cost-risk 核） |
| T-2 | 帆平衡温度超材料上限 | 0.05c 最紧 → **警示性**判据，依赖 α/ε/A 实测 |
| T-3 | 微尘撞击无法防护且无低尘走廊 | 0.03c=40MJ / 0.05c=112MJ/1mg → 警示性，依赖 ISM 尘密 |
| T-4 | 近端目标需 > 单任务尺度 | 未触发（三速度均数月–1.6 年） |
| T-5 | 相对论修正率 > 阈值 | 未触发（<0.2%） |
| T-6 | 误用物质级动能/「到比邻星」作近端承诺 | **禁止性表述**，违反 `forbidden_claims` |

### 3.3 验证清单（Verification Checklist）

**本节点已实跑并留证**（本轮重跑）：V-1 派生量 ✓；V-2 0.03c 五项锚点 5/5 PASS（**最大相对误差 1.926e-7 = 重力锚点**；次大 2.579e-8 = 5mt 动能 → 印证 DEF-01 修正正确）✓；V-3 束功率 16–400× ✓；V-4 平衡温度与扫掠 ✓；V-5 相对论修正 <0.2% ✓；V-6 未启动服务/脚本有界 ✓。证据：`checks/pet_check_output.txt`、`checks/pet_results.json`。

**待他节点/待数据（如实声明，不标通过）**：V-7 十一项硬门槛独立复跑（`verification-integrator`）；V-8 应用三速切换/JSON 导出（`digital-engineering-builder`→`verification-integrator`）；V-9 束阵列规模与成本（`cost-risk-analyst`）；V-10 载荷长期可靠性/档案语义/载人边界（`archive-reliability-designer`/`systems-architect`）；V-11 帆材料 α/ε/A 实测（数据）；V-12 ISM 微尘通量/密度（数据）；V-13 Starshot 0.2c/100GW 锚点升 A 级（独立核验，现为 B 级）。

### 3.4 锚点复算（0.03c，`acceptance.json` 容差）——本轮重跑实测

| 锚点 | got | exp | 相对误差 | 容差 | 判定 |
|---|---|---|---|---|---|
| `travel_years_at_0_03c` | 1.416667e2 | 1.416667e2 | 2.353e-10 | 0.001 | PASS |
| `kinetic_energy_1mt_at_0_03c_j` | 4.044398e22 | 4.044398e22 | 1.067e-9 | 0.01 | PASS |
| `kinetic_energy_5mt_at_0_03c_j` | 2.022199e23 | 2.022199e23 | **2.579e-8** | 0.01 | PASS |
| `gravity_1km_2rpm_m_s2` | 4.386491e1 | 4.386490e1 | **1.926e-7** | 0.01 | PASS |
| `dust_1mg_0_03c_j` | 4.044398e7 | 4.044398e7 | 1.067e-9 | 0.01 | PASS |

**总评**：`5/5 在容差内`；最大相对误差 **1.926e-7**（重力锚点），次大 **2.579e-8**（5mt 动能锚点）。同批读数与 `model/baseline.yaml`、`model/params.json`、`physics` 参考计算交叉一致（G2/G10 域内量，除 physics CSV 4 位有效数字舍入）。

---

## 4. 诚实边界（本节点）

- 本节点**未**独立复跑十一项硬门槛 G1–G11（属 `verification-integrator`），**不**作整体 `overall` 判定；**未**修复数字应用（属 `digital-engineering-builder`）。
- 本节点以真实工具调用实测并记录：本轮重跑退出码 0、锚点 5/5 PASS、复用件 SHA 与 manifest 一致、无 `__pycache__`。
- 未将 `unknown`/`assumption` 当作已验证事实；T-2 / T-3 为**警示性**而非结论性判据（依赖尚未核实的 α/ε/A 与 ISM 尘密）。
- 未启动任何服务；未对主机做无关广泛搜索（仅定位所列冻结基线/素材路径）。

## 5. 供 verification-integrator 聚合的 propulsion 入口信息

- **域索引/入口**：`review/propulsion/README.md`（本文件）
- **主复核报告**：`review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md`
- **机器可读矩阵**：`review/propulsion/07_corona_v6_pet_comparison_matrix.csv`
- **可重跑脚本**：`review/propulsion/scripts/pet_check.py`
- **重跑证据**：`review/propulsion/checks/pet_check_output.txt` · `review/propulsion/checks/pet_results.json`
- 以上路径均存在于本节点 `outputs/`（即唯一最终目录的 propulsion 部分）；`outputs/review/propulsion/` 下无 `__pycache__`。
