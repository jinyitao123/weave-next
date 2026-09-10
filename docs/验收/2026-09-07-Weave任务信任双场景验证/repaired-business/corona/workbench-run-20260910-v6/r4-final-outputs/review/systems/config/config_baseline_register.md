# 日冕计划 Pre-Phase A · v6 · 配置基线注册（systems-architect）

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 **隔离恢复验证**（复用 v4，仅改可复现缺陷）
**职责**：配置基线（configuration baseline）——登记并受控**引用**已冻结的共同基线，不重新生成基线文件。

> 相对 v4 修正：**移除对失效 v4 上游输入的引用**（`inputs/lead/baseline/baseline.yaml`、`inputs/lead/05`）。v6 输入集内冻结配置基线 `corona-prephase-a-v1` 的自包含副本实际位于 `v4-outputs/model/baseline.yaml`（其头部自述与 `complex-validation/baseline.yaml` 逐字一致），本注册改以该副本与 `acceptance.json` 为依据。

---

## 0. 诚实边界（重要）

- 本文件是**配置基线注册（register）**，引用冻结基线 `corona-prephase-a-v1`，**不是**冻结基线本体，**不是**新基线。
- 冻结基线**本体**由 `v4-outputs/model/baseline.yaml`（自包含副本；等价于 `complex-validation/baseline.yaml`）承载；本节点**未**重新创建或改写它。
- 按平台约束「Do not recreate a frozen baseline」，本注册**只做标识、版本、控制与追溯**，不自造参数。

---

## 1. 冻结基线标识

| 项 | 值 |
|---|---|
| baseline_id | `corona-prephase-a-v1` |
| baseline_version | `1.0.0` |
| status | `frozen_for_validation` |
| scope | Level-0 / Pre-Phase A conceptual study |
| schema_version | 1 |
| 来源（primary） | 《日冕计划_任务背景与全程纪要_修订稿.md》——外部源材料路径标注 |
| 载体（v6 输入集内自包含副本） | `v4-outputs/model/baseline.yaml` |

---

## 2. 配置项清单（Configuration Items）

| CI | 名称 | 受控属性 | 来源载体 |
|---|---|---|---|
| CI-01 | 速度轴（标准情景） | `cruise_speed_c = [0.01, 0.03, 0.05]`；`default_scenario_c = 0.03`；`scenario_ids = [S-0.01c, S-0.03c, S-0.05c]` | `v4-outputs/model/baseline.yaml` |
| CI-02 | 物理常数 | `speed_of_light_m_s=299792458`、`standard_gravity_m_s2=9.80665`、`proxima_distance_ly=4.25`、`ly_m=9.4607304725808e15`、`sec_per_year=31557600`、`tnt_equivalent_J_per_kg=4184000`；派生 `au_m=1.495978707e11`、`solar_lens_distance_au=550`、`precursor_distance_au=1000`（见 `params.json`） | `v4-outputs/model/baseline.yaml` / `params.json` |
| CI-03 | 参考案例 | `crewed_1mt`、`orbital_material_5mt`、`dust_1mg`、`rotating_habitat` | `v4-outputs/model/baseline.yaml` |
| CI-04 | 参考检查（0.03c 锚点） | `travel_years_at_0_03c`（tol 0.001）、`kinetic_energy_1mt_at_0_03c_j`（tol 0.01）、`kinetic_energy_5mt_at_0_03c_j`（tol 0.01）、`gravity_1km_2rpm_m_s2`（tol 0.01）、`dust_1mg_0_03c_j`（tol 0.01） | `v4-outputs/model/baseline.yaml` `acceptance_reference_checks` = `acceptance.json` `reference_checks` |
| CI-05 | 路线 | `laser_sail_precursor`（A）、`uncrewed_civilization_archive`（B）、`crewed_interstellar_vehicle`（C） | `v4-outputs/model/baseline.yaml` `routes` |
| CI-06 | 真值标签 | `[verified_fact, derived_result, assumption, unknown]` | `v4-outputs/model/baseline.yaml` `truth_labels` |
| CI-07 | 禁止断言 | `[construction_ready, manufacturing_ready, flight_certified, whole_program_cost_committed]` | `v4-outputs/model/baseline.yaml` `forbidden_claims` |
| CI-08 | 参数模型机器/app 副本 | `params.json` = `app/params.js` = `v4-outputs/model/baseline.yaml`（一致） | 数字工程交叉核验（见 `run_checks.py` M3/M4） |

---

## 3. 配置控制（变更控制链接）

| 变更 | 对象 | 结论 |
|---|---|---|
| 基线冻结确认 | `corona-prephase-a-v1` | 沿用，速度轴保持 `0.01c/0.03c/0.05c`，现有日冕材料不改写（用户指令） |
| 变更控制 | `outputs/app/`（页面计算/情景切换/导出） | 执行属 digital-engineering；仅在发现**可复现缺陷**时改 |
| 变更控制 | v6 复验流程（独立总装） | 执行属平台编排 + verification-integrator |

> 任何参数/公式/单位/情景改动一律走变更控制，不得就地覆盖冻结基线。参数基线本体不在数字工程/总装变更范围内。

---

## 4. 一致性核验（本轮真实工具调用）

- 用冻结常量独立重算 `CI-04` 参考检查与 `CI-01/CI-02` 派生值，结果与 `v4-outputs/model/baseline.yaml` / `acceptance.json` 逐项一致（5/5 在容差内，`all_ok=True`，max rel_err ≈ 2.6e-08）。证据：`../verification/systems_param_consistency_check.txt`（本次运行生成）。
- 该核验为转发一致性核对，**不是**对 `acceptance.json` 十一项硬门槛的独立复跑（属 verification-integrator）。

---

## 5. 边界

1. 本注册**不生成**统一模型/应用/`FINAL_ACCEPTANCE.md`；十一项硬门槛判定以独立总装员为准。
2. 本注册**不重新创建**冻结基线；仅在 `v4-outputs/model/baseline.yaml` 基础上做标识/受控/追溯。
3. 若权威 `acceptance.json`/团队定义与实际不一致，以真实文件为准。

---

*由 systems-architect 节点产出；配合 `../config/config_items.yaml`（机器可读）供自动化比对。*
