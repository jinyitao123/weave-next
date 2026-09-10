# 日冕计划 · Pre-Phase A · v6 · physics 域索引与复用声明（PHYSICS DOMAIN ENTRY）

**节点**：`corona-prephase-a-recovery-v6-mission-physics-analyst`（任务物理与轨迹 · 公式复核 / 单位检查 / 轨迹数量级 / 速度情景）
**日期**：2026-09-10
**本轮职责**：复核冻结 v6 物理成果中的 **公式 / 数量级 / 三速度情景（0.01c/0.03c/0.05c）/ 未知项**；**优先原样复用**，仅在有证据要求的可复现缺陷时修改。
**输入基线**：只读冻结源 `recovery-materials/2026-09-10-corona-v6-corrected-source/outputs/review/physics/`（manifest SHA-256 `59c911db…03f`）
**权威回判定源**：`/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json`（SHA-256 `f4777aaf…60ec4`）的 `reference_checks` 五项

> 本文件是 physics 域的**索引/入口**，供 verification-integrator 汇总进根级 `outputs/review/REVIEW_INDEX.md`。以下路径均为**真实相对本节点 `outputs/`** 的路径，且已实测存在。

---

## 1. 域索引 / 入口（供 REVIEW_INDEX 引用）

| 层级 | 路径（相对本节点 outputs/） | 说明 |
|---|---|---|
| **域入口** | `review/physics/README.md` | 本索引文件 |
| 主复验报告 | `review/physics/physics_v6_review_verification.md` | v6 物理复核报告（公式/单位/三情景/锚点/数量级/未知项/缺陷处置） |
| 独立复算脚本 | `review/physics/physics_v6_independent_recompute.py` | 自包含、从第一性原理重写的独立复算（不 import v4 模块） |
| 独立复算证据 | `review/physics/physics_v6_recompute_evidence.txt` | 独立复算真实运行输出（公式/单位/三情景/锚点/CSV 逐值对比） |
| 修正版复算脚本 | `review/physics/physics_recompute_v6_fixed.py` | v4 `physics_recompute.py` 修正版（仅改 [C] 标签 + 补 1g 行） |
| 修正版复算证据 | `review/physics/physics_v6_fixed_recompute_evidence.txt` | 修正版真实运行输出（与冻结源逐字一致） |
| **追加·本节点独立交叉复核脚本** | `review/physics/physics_v6_this_node_independent_check.py` | 本节点自写的第一性原理复核（动态读权威 acceptance.json，非硬编码） |
| **追加·本节点独立交叉复核证据** | `review/physics/physics_v6_this_node_independent_check_evidence.txt` | 上脚本真实运行输出（含 FD-01 数量级闭环判定） |

## 2. 复用 / 修改自述（自我声明）

| 文件（相对 outputs/） | 处置 | 依据 |
|---|---|---|
| `review/physics/physics_v6_review_verification.md` | **复用（零改动）** | SHA-256 `194a37f7…80c` 与冻结源一致；内容、公式、缺陷处置均已复核正确 |
| `review/physics/physics_v6_independent_recompute.py` | **复用（零改动）** | SHA-256 `dd42776e…c93c` 一致 |
| `review/physics/physics_v6_recompute_evidence.txt` | **复用（零改动）** | SHA-256 `24fd9a90…4f6` 一致；已重跑脚本，输出与冻结证据**逐字节一致** |
| `review/physics/physics_recompute_v6_fixed.py` | **复用（零改动）** | SHA-256 `d9a8687b…e5e` 一致 |
| `review/physics/physics_v6_fixed_recompute_evidence.txt` | **复用（零改动）** | SHA-256 `fdeb2d46…82b` 一致；已重跑脚本，输出与冻结证据**逐字节一致** |
| `review/physics/physics_v6_this_node_independent_check.py` | **本轮新增** | 本节点自写交叉复核（非冻结件，为支撑 G3/G10 追加证据） |
| `review/physics/physics_v6_this_node_independent_check_evidence.txt` | **本轮新增** | 上脚本真实运行输出 |

**结论**：**整体复用（5 件复用 + 2 件新增）**。**未修改任何冻结物理件**。本轮在冻结 v6 物理包内**未发现新的可复现缺陷**；v4 唯一的「1e9 kg 被标成 `E_SAIL_1G`」标签缺陷已在冻结 v6 包中修正，本节点重跑确认修正生效且数值零改动。

**FD-01 修正脚注（不得回退项）已确认**：冻结源 `drawings/FD-01_speed_axis_scenario_comparison.svg` 第 63 行存在
>「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」
本节点独立核算 1 mt（1e9 kg）在 0.01c–0.05c 的 ½mv² 为 **[4.4938e21, 1.1234e23] J**，与该展示文本一致（相对差 ~8e-4 / 3e-3，纯显示位数舍入所致），**纳入 G10 判定口径**。

---

## 3. 复核结论（本节点职责内）

### 3.1 公式 / 单位检查 — 全部通过
- 闭合式 `v=βc`、`t=D/v`、`E=½mv²`、`a=ω²r`、`γ=1/√(1−β²)`、`E_k=(γ−1)mc²`、`P/P_ref=(β/β_ref)²` 均正确。
- 量纲：速度 `[L·T⁻¹]`、能量 `[M·L²·T⁻²]=J`、人工重力 `[L·T⁻²]`、γ/束功率比无量纲 —— 10/10 一致，无维度不匹配。

### 3.2 三速度情景（0.01c / 0.03c / 0.05c）— 确认
速度轴确为 `0.01c/0.03c/0.05c`，与 `model/baseline.yaml`、`model/params.json` 的 `cruise_speed_c` 逐值一致。三个情景（S-0.01c/S-0.03c/S-0.05c）全部物理量独立复算一致：到比邻星航时 425 / 141.7 / 85 年；1mt 动能 4.49e21 / 4.04e22 / 1.12e23 J；1g 帆 4.49 / 40.44 / 112.3 GJ。

### 3.3 数量级 — 成立
- 尘埃威胁：1 mg 尘埃 0.01c→1.07 kg、0.03c→9.67 kg、0.05c→26.85 kg TNT 当量（物理必要；通量/密度为 unknown，U3）。
- 束功率降幅：固定 m、L 时 `P∝v²`，0.2c→0.01c/0.03c/0.05c 分别为 400×/44.4×/16× 降幅（不含效率修正，U1/U2）。
- 可达域：550 AU 透镜、120 AU 日球层、1000 AU 前驱的抵达时间均在数月量级。

### 3.4 未知项 — 登记完整（未以假设掩盖）
U1 束传递效率、U2 帆反射/耦合效率与质量/加速距离随速档变化、U3 尘埃通量/密度、U4 引力透镜焦点精确值（采用近似量级）、U5 克级可行性不可外推至 ≥1t 无人/载人、U6 Starshot 0.2c/100GW B 级锚点、U7 推进选型、U8 人工重力医学必需性。均标注 `unknown`/`assumption`，`E=½mv²` 仅计动能下限，非完整推进能源预算。

### 3.5 0.03c 五项锚点 vs 权威 acceptance.json — 5/5 通过在容差内
| 锚点 | computed | expected | rel_err | 判定 |
|---|---|---|---|---|
| `travel_years_at_0_03c` | 141.6667 | 141.6667 | 2.35e-10 | OK |
| `kinetic_energy_1mt_at_0_03c_j` | 4.044398e22 | 4.044398e22 | 1.07e-09 | OK |
| `kinetic_energy_5mt_at_0_03c_j` | 2.022199e23 | 2.022199e23 | 2.58e-08 | OK |
| `gravity_1km_2rpm_m_s2` | 43.86491 | 43.8649 | 1.93e-07 | OK |
| `dust_1mg_0_03c_j` | 4.044398e7 | 40443983 | 1.07e-09 | OK |

> 上表为本节点独立交叉复核实测（`physics_v6_this_node_independent_check.py`，动态读入权威 `reference_checks`）。该结果亦与冻结源独立复算、修正复算一致。

---

## 4. 如实说明的边界 / 未执行项

- **未执行**：G1–G11 十一项硬门槛的独立复跑与 `overall: PASS` 判定（属 verification-integrator）；根级 `outputs/review/REVIEW_INDEX.md` 创建（属 verification-integrator）；数字应用修复（属 digital-engineering-builder）。
- **本节点不**作任何整体验收判定，**不**声明 `overall: PASS`。
- **观察项（非缺陷，未修改冻结件）**：冻结复验报告 §5「交付文件」表以 `outputs/<file>` 根相对形式列出本域文件，而本域按协调约定实际位于 `outputs/review/physics/<file>`；二者路径前缀不同，但指向同一内容。本 README §1 已给出**权威真实相对路径**，供 REVIEW_INDEX 使用。报告正文（公式/单位/三情景/锚点/数量级/未知项/缺陷处置）全部正确，故未改动冻结件，仅在此登记供核。
- **副作用**：none（只写本成员 `workdir/outputs/`；未触只读输入，未外部发布）。
