# 日冕计划 · Pre-Phase A · v6 · 复核材料索引（REVIEW INDEX）

**节点**：`corona-prephase-a-recovery-v6-verification-integrator`（独立验证与总装员）
**日期**：2026-09-10
**基线**：`corona-prephase-a-v1`（v1.0.0，`frozen_for_validation`，速度轴 `0.01c / 0.03c / 0.05c`）
**用途**：为本轮正式交付修订把各专业域（systems / physics / propulsion / archive / cost_risk）的**索引与入口**及**实际相对路径**汇总为一份根级索引，供 `artifact:review-index` 等路径存在性校验，并作为十一项硬门槛复核的可检索地图。

> 本索引中的所有相对路径均以**唯一最终目录 `outputs/`** 为根（即 `outputs/review/…`）。下方各路径已在总装员本轮真实执行中经 `os.path.exists` 逐项核验存在（见 `verification/acceptance.json` 的 `review_index_paths` 字段）；域内文件亦已对照冻结源 `manifest.json` 与各成员本轮交付哈希核验一致，不含 `__pycache__` / `*.pyc`。

## 一、域入口总览

| 域 | 入口（相对 outputs/） | 角色 | 服务硬门槛 |
|---|---|---|---|
| 系统/需求（systems） | `review/systems/README.md` | 任务终态/接口/追踪矩阵/需求分解/配置基线 | G2 G3 G8 G9 G10 G11 |
| 物理（physics） | `review/physics/README.md` | 公式/单位/轨迹数量级/三速度情景/未知项 | G2 G3 G9 G10 |
| 推进能源热控（propulsion） | `review/propulsion/README.md` | 成立/终止条件/验证清单/比较矩阵 | G2 G3 G10 |
| 档案载荷与可靠性（archive） | `review/archive/index_archive_reliability_designer_v1.0.md` | 载荷架构/长期可靠性/概念图纸要求 | G6 G7 G8 G9 G10 |
| 成本与风险（cost_risk） | `review/cost_risk/REVIEW_INDEX_cost_risk.md` | 技术成熟度/WBS/成本方法/风险/终止条件/课题包 | G3 G8 G9 G11 |

## 二、逐域清点（相对 outputs/，均实测存在）

### 2.1 systems（系统工程师 · 控制件包）

| 文件 | 说明 |
|---|---|
| `review/systems/README.md` | **域索引/入口** |
| `review/systems/requirements/requirements_decomposition.md` | 需求分解 `SA-REQ-*`（每条含验证方法） |
| `review/systems/interfaces/interface_register.md` | 接口登记 `IFM-01..14` |
| `review/systems/routes/route_terminal_states.md` | 路线终态 A/B/C × 三速度 |
| `review/systems/verification/traceability_matrix.md` | 追踪矩阵（需求的 Markdown 版，31 行） |
| `review/systems/verification/traceability_matrix.csv` | 追踪矩阵（机器可读，31 行 × 9 列） |
| `review/systems/config/config_baseline_register.md` | 配置基线注册 `CI-01..08` |
| `review/systems/config/config_items.yaml` | 配置项（机器可读） |
| `review/systems/verification/systems_param_consistency_check.py` | 参数一致性重算脚本（可重跑） |
| `review/systems/verification/systems_param_consistency_check.txt` | 参数一致性证据（5/5 锚点 `all_ok=True`） |
| `review/systems/v6_reuse_declaration.md` | 本轮复用声明/REVIEW_INDEX 入口 |

### 2.2 physics（物理 · 复核包）

| 文件 | 说明 |
|---|---|
| `review/physics/README.md` | **域索引/入口** |
| `review/physics/physics_v6_review_verification.md` | 主复验报告（公式/单位/三情景/锚点/数量级/未知项/缺陷处置） |
| `review/physics/physics_v6_independent_recompute.py` | 自包含第一性原理独立复算（不 import v4 模块） |
| `review/physics/physics_v6_recompute_evidence.txt` | 独立复算真实运行输出 |
| `review/physics/physics_recompute_v6_fixed.py` | v4 修正版复算（仅改 [C] 标签 + 补 1g 行） |
| `review/physics/physics_v6_fixed_recompute_evidence.txt` | 修正版真实运行输出（与冻结源逐字一致） |
| `review/physics/physics_v6_this_node_independent_check.py` | 本节点追加独立交叉复核（动态读权威 acceptance.json） |
| `review/physics/physics_v6_this_node_independent_check_evidence.txt` | 上脚本真实运行输出（含 FD-01 数量级闭环） |

### 2.3 propulsion（推进/能源/热控 · 复核包）

| 文件 | 说明 |
|---|---|
| `review/propulsion/README.md` | **域索引/入口** |
| `review/propulsion/06_corona_v6_propulsion_energy_thermal_analyst_v1.0.md` | 主复核报告（比较 + 成立 + 终止 + 验证清单 + DEF-01 修正） |
| `review/propulsion/07_corona_v6_pet_comparison_matrix.csv` | 三速度比较矩阵（机器可读） |
| `review/propulsion/scripts/pet_check.py` | 有界自动检查脚本 |
| `review/propulsion/checks/pet_check_output.txt` | 本轮真实重跑输出 |
| `review/propulsion/checks/pet_results.json` | 机器可读结果 |

### 2.4 archive（档案载荷与可靠性 · 复核包）

| 文件 | 说明 |
|---|---|
| `review/archive/index_archive_reliability_designer_v1.0.md` | **域索引/入口**（本轮已修正交付表路径为 v6 实文件名） |
| `review/archive/06_corona_v6_archive_payload_architecture_v1.0.md` | 文明档案载荷架构（五层双介质） |
| `review/archive/07_corona_v6_archive_long_term_reliability_v1.0.md` | 长期可靠性（辐射/介质寿命/K-of-N/ECC） |
| `review/archive/08_corona_v6_archive_concept_drawing_requirements_v1.0.md` | 概念图纸要求（Fig B-01…B-05 · 标注规范 · 验证方法） |
| `review/archive/09_corona_v6_archive_concept_architecture_diagram.svg` | 概念架构示意 SVG（自包含，无外部资源/脚本） |
| `review/archive/support/reliability_model.py` | 长期可靠性可复算模型 |
| `review/archive/support/reliability_model_output.txt` | 模型真实运行输出证据（77 行） |
| `review/archive/archive-reliability-node-workbrief-v6.md` | 本节点工作简报 |

### 2.5 cost_risk（成本与风险 · 复核包）

| 文件 | 说明 |
|---|---|
| `review/cost_risk/REVIEW_INDEX_cost_risk.md` | **域索引/入口** |
| `review/cost_risk/techmaturity_assessment.md` | 技术成熟度评估 |
| `review/cost_risk/wbs.md` | 工作分解结构 |
| `review/cost_risk/cost_research_method.md` | 成本研究方法 |
| `review/cost_risk/risk_register.md` | 风险登记册（RK-01…21、RK-CR1/2/3、RK-V6-1…5） |
| `review/cost_risk/termination_conditions.md` | 终止条件 |
| `review/cost_risk/topic_package_next_phase.md` | 下一阶段课题包 |
| `review/cost_risk/verification/cost_risk_recomputed_2026-09-10.txt` | v6 派生量复算证据 |
| `review/cost_risk/verification/cost_risk_recomputed_2026-09-09.txt` | v4 证据（保留比对） |
| `review/cost_risk/v6-revision-reuse-declaration.md` | 本轮复用自述 |

## 三、关键保留项（不得回退）

- **FD-01 修正脚注**：`drawings/FD-01_speed_axis_scenario_comparison.svg`（概念图纸域，非复核域，但为 G10 关键口径）第 63 行文本
  `柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J` —— 已在总装员本轮核验存在，并把该**展示文本**纳入 G10 跨产物一致性复核（与 ½mv² 首性复算的 1 mt 在 0.01c–0.05c 区间 `[4.4938e21, 1.1234e23] J` 一致，相对差 ~8e-4 / 3e-3，纯显示位数舍入）。

## 四、计数与清单口径

- 本索引**不含**任何运行缓存（`__pycache__` / `*.pyc`）；核验时对冻结被测清单排除该类缓存。
- 各域入口与文件均在总装员本轮真实执行中经路径存在性校验（见 `verification/acceptance.json`）。
- 本索引是复核材料的路由表，**不是**验收结论；十一项硬门槛的最终逐项判定见 `outputs/FINAL_ACCEPTANCE.md`。
