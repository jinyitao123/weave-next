# 日冕计划 Pre-Phase A · v6 正式交付修订轮 · 推进 / 能源 / 热控域 —— 复核后复用说明与域入口（INDEX）

**节点**：`corona-prephase-a-recovery-v6-propulsion-energy-analyst`（推进/能源/热控复核）
**职责**：推进比较 · 能源预算 · 热控约束 · 技术成熟度（本域的推进/能源/热控子集）
**本轮动作**：复核冻结 `outputs/review/propulsion/` → **原样复用**；运行有界自动检查（`pet_check.py`）；未发现新的可复现缺陷，未做任何数值修改。
**唯一素材（只读）**：`recovery-materials/2026-09-10-corona-v6-formal-r1-source/outputs/review/propulsion/`（本次正式交付修订的冻结源）
**官方参数源（只读）**：`recovery-materials/2026-09-10-corona-v5-source/v4-outputs/model/baseline.yaml`（存在，SHA-256 `09351b046422984b2bc6373d3d5316df56204224d55c0340bf70afee0978230c`，与权威基线一致）
**权威验收**：`acceptance.json`（SHA-256 `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`）
**外部副作用**：`none`；所有文件仅写入本节点 `workdir/outputs/`。

> 本 README 即本域的**入口 / 索引**，供 `verification-integrator` 汇总进根级 `outputs/review/REVIEW_INDEX.md`。本域**非最终验收**；十一项硬门槛（G1–G11）的最终逐项判定属独立验证与总装员。本节点只完成推进/能源/热控三个专业子域的比较与判据，不构造统一模型、不制造数字应用、不冒充独立总装员。

---

## 0. 复用 / 修改自述（自我声明）

- **结论：复用（数值件修改 = 0）。** 本轮对冻结源 `review/propulsion/` 逐项核对后**原样复用**，未发现新的可复现缺陷，未改写、未重做、未顺带重构。
- **逐字节复用（SHA 与冻结 `manifest.json` 一致，已实测核验）**：`06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md`、`07_corona_v6_pet_comparison_matrix.csv`、`scripts/pet_check.py`。
- **本轮重跑生成**：`checks/pet_check_output.txt`、`checks/pet_results.json`（由本节点真实运行 `pet_check.py` 产生，退出码 `0`；`pet_results.json` 与冻结**逐字节一致**，`pet_check_output.txt` 计算内容与冻结**逐字节一致**，仅尾部 `[written]` 路径行随本节点 workdir 变化）。
- **本索引 README 为本节点重写**：仅作域入口重新表述，并**修正冻结 README 所引用素材路径为本次正式修订源 `corona-v6-formal-r1-source`**（冻结版 `README.md` 写的是中间源 `corona-v6-corrected-source`，与 `run_input`/`formal-r1-source` 指定不符，为避免所属地描述矛盾，本入口按 `formal-r1-source` 如实标注）。不涉及任何数值或结论改动。
- **冻结源已修正的 DEF-01**（v4 版陈述「最大相对误差 2.58e-8」为**错误**：2.58e-8 实为**次大**；正确最大值为 **1.926e-7**，来自重力锚点）：该修正已在冻结源中改妥（见 §3 附）。本节点复用其**修正版**，并经本节点**独立复算**再次印证（见 §3.5）。
- **本节点不代做**：统一模型（`model-owner`）、数字应用与图纸（`digital-engineering-builder`）、其他专业评审包（systems/physics/archive/cost_risk）、根级 `REVIEW_INDEX.md` 与 `FINAL_ACCEPTANCE.md`（`verification-integrator`）。

---

## 1. 交付件清单（相对本节点 `outputs/` 的真实路径）

| 相对路径 | 内容 | 处理 |
|---|---|---|
| `review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` | 推进/能源/热控复核报告（比较 + 成立条件 + 终止条件 + 验证清单 + DEF-01 修正） | 复用（SHA 与 manifest 一致，`e5398fb5…`） |
| `review/propulsion/07_corona_v6_pet_comparison_matrix.csv` | 三速度推进/能源/热控比较矩阵（机器可读） | 复用（SHA 与 manifest 一致，`6d5db202…`） |
| `review/propulsion/scripts/pet_check.py` | 有界自动检查脚本（单线程/有限次数/无循环/退出即止，**未启动任何服务**） | 复用（SHA 与 manifest 一致，`c125a7f2…`） |
| `review/propulsion/checks/pet_check_output.txt` | 本轮**真实重跑**输出（与冻结计算内容逐字节一致，仅尾行 `[written]` 为本节点 workdir 路径） | **本轮重跑生成** |
| `review/propulsion/checks/pet_results.json` | 机器可读结果（与冻结逐字节一致，SHA `91027dc7…`） | 本轮重跑（内容与冻结一致） |
| `review/propulsion/README.md` | 本入口/索引 + 复用自述 + 成立/终止/验证清单 | 本轮重写（素材路径如实标注为 `formal-r1-source`） |

### 1.1 复用件逐项核验（SHA-256，对照冻结源 `manifest.json`）

| 相对路径 | SHA-256 | 与 manifest 一致 |
|---|---|---|
| `review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` | `e5398fb56bfa1da46e9e12843694cf05a5152378bd24e34c114020f1da4f3901` | ✅ 一致 |
| `review/propulsion/07_corona_v6_pet_comparison_matrix.csv` | `6d5db2029da2a36be4be1b2a753ee07e9dac9973d0c599dd430f1d33b2be624b` | ✅ 一致 |
| `review/propulsion/scripts/pet_check.py` | `c125a7f2f3b539e7a96f34fc893ff512e50d5d795f3852f4c52d0becf9aa8bf6` | ✅ 一致 |
| `review/propulsion/checks/pet_results.json` | `91027dc77f03c4f80b0f090a7edae1e283a62e1381827c67e63dc379e1f95e55` | ✅ 一致（本轮重跑逐字节一致） |
| `review/propulsion/checks/pet_check_output.txt` | 本轮重跑；计算内容与冻结逐字节一致，尾行 workdir 路径随本节点而异 | 计算内容一致（尾行除外） |

> 冻结源 `manifest.json` 中被测清单**不含 `__pycache__` / `*.pyc`**；本目录同样不含任何运行缓存（已实测）。

---

## 2. 本轮有界自动检查（已实跑，退出即止）与可复现性

- **命令**：`python3 outputs/review/propulsion/scripts/pet_check.py`（单进程、有限次数、无循环、无长驻服务；脚本自身即设计为有界即退）。
- **退出码**：`0`。
- **参数源**：脚本 `BASE` 指向冻结基线 `recovery-materials/2026-09-10-corona-v5-source/v4-outputs/model/baseline.yaml`（real path 存在，SHA-256 `09351b046422984b…` 与权威基线一致）。
- **可复现性**：本轮重跑输出与冻结 `pet_check_output.txt` **计算内容逐字节一致**，唯一差异为尾部 `[written] …workdir` 路径行（反映本节点自己的 workdir）。`pet_results.json` 逐字节一致（SHA 相同）。
- **未启动任何服务**：已确认（脚本无循环、无服务启动逻辑、退出码 0）。
- **本次真实工具调用**：运行脚本并捕获 stdout → `checks/pet_check_output.txt`；脚本自写 `checks/pet_results.json`；stderr 为空。

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

**本节点已实跑并留证**（本轮重跑）：V-1 派生量 ✓；V-2 0.03c 五项锚点 5/5 PASS（**最大相对误差 1.926e-7 = 重力锚点**；次大 2.579e-8 = 5mt 动能 → 印证 DEF-01 修正正确）✓；V-3 束功率 16–400× ✓；V-4 平衡温度与扫掠 ✓；V-5 相对论修正 <0.2% ✓；V-6 未启动服务/脚本有界（退出码 0）✓。证据：`checks/pet_check_output.txt`、`checks/pet_results.json`。

**待他节点/待数据（如实声明，不标通过）**：V-7 十一项硬门槛独立复跑（`verification-integrator`）；V-8 应用三速切换/JSON 导出（`digital-engineering-builder`→`verification-integrator`）；V-9 束阵列规模与成本（`cost-risk-analyst`）；V-10 载荷长期可靠性/档案语义/载人边界（`archive-reliability-designer`/`systems-architect`）；V-11 帆材料 α/ε/A 实测（数据）；V-12 ISM 微尘通量/密度（数据）；V-13 Starshot 0.2c/100GW 锚点升 A 级（独立核验，现为 B 级）。

### 3.4 锚点复算（0.03c，`acceptance.json` 容差）——本轮重跑实测

| 锚点 | got | exp | 相对误差 | 容差 | 判定 |
|---|---|---|---|---|---|
| `travel_years_at_0_03c` | 1.416667e2 | 1.416667e2 | 2.353e-10 | 0.001 | PASS |
| `kinetic_energy_1mt_at_0_03c_j` | 4.044398e22 | 4.044398e22 | 1.067e-9 | 0.01 | PASS |
| `kinetic_energy_5mt_at_0_03c_j` | 2.022199e23 | 2.022199e23 | **2.579e-8** | 0.01 | PASS |
| `gravity_1km_2rpm_m_s2` | 4.386491e1 | 4.386490e1 | **1.926e-7** | 0.01 | PASS |
| `dust_1mg_0_03c_j` | 4.044398e7 | 4.044398e7 | 1.067e-9 | 0.01 | PASS |

**总评**：`5/5 在容差内`；最大相对误差 **1.926e-7**（重力锚点），次大 **2.579e-8**（5mt 动能锚点）。锚点期望值取自冻结 `model/baseline.yaml`（本节点已实读该文件，SHA-256 `09351b04…`）；与 `model/params.json`、`physics` 参考计算的交叉一致性属冻结复核报告 §0 陈述（G2/G10 域内量，除 physics CSV 4 位有效数字舍入），本节点**未独立复跑该跨域交叉**，此处如实标注。

### 3.5 独立性复核（本节点不调用 `pet_check.py`，直接由冻结常量重算，印证 DEF-01）

本节点另用一份**独立**短脚本（不调用 `pet_check.py` 的逻辑），从冻结 `baseline.yaml` 常量（`speed_of_light_m_s`、`proxima_distance_ly`、`ly_m`、`sec_per_year`）直接重算五项锚点：

- `travel_years_at_0_03c`：rel_err 2.353e-10 → PASS
- `kinetic_energy_1mt_at_0_03c_j`：rel_err 1.067e-9 → PASS
- `kinetic_energy_5mt_at_0_03c_j`：rel_err 2.579e-8 → PASS
- `gravity_1km_2rpm_m_s2`：rel_err 1.926e-7 → PASS
- `dust_1mg_0_03c_j`：rel_err 1.067e-9 → PASS

> 独立复核结果与 §3.4 一致：**MAX = 1.926e-7（重力锚点），次大 = 2.579e-8（5mt 动能锚点）**。据此确认冻结版 DEF-01 修正（「v4 所述最大 2.58e-8 实为次大；正确最大值为 1.926e-7」）**成立**，且**不影响 G3**（5/5 仍在容差内）。

---

## 4. 诚实边界（本节点）

- 本节点**未**独立复跑十一项硬门槛 G1–G11（属 `verification-integrator`），**不**作整体 `overall` 判定；**未**修复数字应用（属 `digital-engineering-builder`）；**未**接管本域之外（systems/physics/archive/cost_risk）的任何评审项。
- 本节点以真实工具调用实测并记录：本轮重跑退出码 0、锚点 5/5 PASS、复用件 SHA 与 manifest 一致、无 `__pycache__`、无 stderr、未启动任何服务。
- **未**将 `unknown`/`assumption` 当作已验证事实；T-2 / T-3 为**警示性**而非结论性判据（依赖尚未核实的 α/ε/A 与 ISM 尘密）。
- 未对主机做无关广泛搜索（仅定位所列冻结基线/素材路径，即 `formal-r1-source` 与 `v5-source/v4-outputs/model/baseline.yaml`）。

## 5. 供 verification-integrator 聚合的 propulsion 入口信息

- **域索引/入口**：`review/propulsion/README.md`（本文件）
- **主复核报告**：`review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md`
- **机器可读矩阵**：`review/propulsion/07_corona_v6_pet_comparison_matrix.csv`
- **可重跑脚本**：`review/propulsion/scripts/pet_check.py`
- **重跑证据**：`review/propulsion/checks/pet_check_output.txt` · `review/propulsion/checks/pet_results.json`
- 以上路径均存在于本节点 `outputs/`（即唯一最终目录的 propulsion 部分）；`outputs/review/propulsion/` 下无 `__pycache__` / `*.pyc`（已实测）。

## 6. 结论（本节点专业判据层）

- 推进/能源/热控三子域在 `0.01c/0.03c/0.05c` 框架下的比较**已在本次有界自动检查中复跑确认**，派生物理量与冻结基线逐项一致，0.03c 五项锚点 5/5 在容差内（最大相对误差 1.926e-7，重力锚点；次大 2.579e-8，5mt 动能锚点）。
- **近端先导成立档位**以 **0.01c–0.03c** 最宽松（束功率最低、热约束最宽、到透镜区/前驱区数月可达）；**0.05c** 覆盖最快但束功率与热约束最紧，为延伸档。
- **第一道物理门槛是热控（T-2 帆束流生存）与能源（T-3 微尘撞击防护）**，两者均依赖尚未核实的材料物性与介质尘密，属**警示性判据**。
- **载人路线 C** 在推进/能源/热控层**不支持**作近端回报，保留观察/边界定位。
- **DEF-01（v4 最大相对误差陈述错误）已由冻结源修正，并经本节点独立复算再次印证**；其余 v4 内容复用、无改写、无重做。
- 以上为**专业判据**，非最终验收；十一项硬门槛的最终逐项判定属 `verification-integrator`（G11 的 `review_index_paths` 与 `overall` 判定须以最终目录真实存在 `review/systems/verification/traceability_matrix.md` 为前提，与本域无涉）。
