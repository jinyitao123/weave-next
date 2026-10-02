# 日冕计划 · Pre-Phase A · v6 最终验收（FINAL ACCEPTANCE）

**验证节点**：`corona-prephase-a-recovery-v6-verification-integrator`（独立验证与总装员）
**验证日期**：2026-09-10
**基线**：`corona-prephase-a-v1`（v1.0.0，`frozen_for_validation`，速度轴 `0.01c / 0.03c / 0.05c`）
**权威验收文件**：`<MAINTAINER_LOCAL_PATH>`（SHA-256 `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`，与本轮实测一致）
**判定方式**：本总装员在本人 `outputs/` **唯一最终目录**上**真实复跑**原 `acceptance.json` 十一项硬门槛；每一门的判定均指向**独立证据工件路径**（`model/`、`app/`、`drawings/`、`review/`、`verification/` 下的真实交付件与运行证据），不以本报告自身章节充当证据，亦不采信任何成员自述的最终验收。

**检查计数**：11（`check_count_from_execution = 11`，由本次实际执行记录动态生成）。
**被测文件冻结清单**：72 个文件（排除 `__pycache__` / `*.pyc` 运行缓存，排除报告自引用文件 `FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/vi_run_gates_output.txt`；逐文件 SHA-256 见 `verification/acceptance.json` 的 `audited_file_inventory`）。
**唯一最终目录**：`outputs/model/` `outputs/drawings/` `outputs/app/` `outputs/review/` `outputs/verification/` `outputs/FINAL_ACCEPTANCE.md`（`required_final_paths` 全 6 项均实测存在，见 `verification/acceptance.json`）。

## 逐门判定（G1 … G11）

| 门 | 判定 | 门槛 | 方法 | 独立证据工件 |
|---|---|---|---|---|
| **G1: PASS** | `model_tests_pass` | 26 个单元测试全部通过，exit 0 | Test python3 -m unittest tests.test_model（在 outputs/model） | `model/tests/test_model.py`；`verification/vi_run_gates_output.txt` |
| **G2: PASS** | `three_speed_scenarios_present` | 0.01c / 0.03c / 0.05c 三情景贯穿模型、应用、图纸（n=3） | Inspection 模型/应用/图纸三速度贯穿 | `model/params.json`；`model/corona_model.py`；`app/params.js`；`drawings/FD-01_speed_axis_scenario_comparison.svg` |
| **G3: PASS** | `reference_calculations_within_tolerance` | 5/5 参考锚点在容差内（worst rel_err = 1.926e-7，重力锚点） | Analysis 第一性原理复算 vs `acceptance.json` reference_checks | `model/baseline.yaml`；`review/physics/physics_v6_review_verification.md`；`review/physics/physics_v6_this_node_independent_check_evidence.txt`；`verification/vi_run_gates_output.txt` |
| **G4: PASS** | `app_starts_without_cloud_dependency` | 静态 index.html 无外部 URL；真实无头 Chrome 加载，网络 external=0 | Inspection static + Demonstration 真实无头 Chrome（CDP） | `app/index.html`；`app/params.js`；`verification/browser-evidence.json` |
| **G5: PASS** | `app_exports_scenario_json` | 真实浏览器导出 JSON，合法且与页面当前情景一致 | Demonstration 真实无头 Chrome（CDP）真实下载 + 一致性核对 | `app/app.js`；`verification/browser-evidence.json` |
| **G6: PASS** | `drawings_are_parseable_and_editable` | 4 张 SVG 全部 xmllint 解析通过；RH-01.scad 可编辑源存在 | Test xmllint 解析（≥3 SVG）+ Inspection OpenSCAD 源 | `drawings/FA-01_laser_sail_precursor.svg`；`drawings/RH-01_rotating_habitat.scad` |
| **G7: PASS** | `all_drawings_marked_conceptual` | 5 件图纸（4 SVG + 1 SCAD）全部含「概念级」标注 | Inspection 概念级标注扫描 | `drawings/FA-01_laser_sail_precursor.svg`；`drawings/FD-01_speed_axis_scenario_comparison.svg` |
| **G8: PASS** | `requirements_have_verification_methods` | SA-REQ-* 全部带验证方法；追踪矩阵 31 行 method 列 0 空 | Review of design + Inspection（需求表 + traceability CSV） | `review/systems/requirements/requirements_decomposition.md`；`review/systems/verification/traceability_matrix.csv` |
| **G9: PASS** | `claims_use_truth_labels` | 真值标签（verified_fact/derived_result/assumption/unknown）定义并贯穿；禁语仅现于否定/免责语境 | Inspection 真值标签 + 禁语（否定语境）扫描 | `review/systems/routes/route_terminal_states.md`；`review/cost_risk/risk_register.md`；`app/app.js`；`model/params.json` |
| **G10: PASS** | `cross_artifact_parameter_consistency_passes` | params.json==baseline.yaml、params.js==params.json、py↔js max rel_err=0.0（≤1e-9）、FD-01 展示文本三档均在 1% 内、config_items.yaml 存在 | Inspection + Test 跨产物一致性（含 FD-01 展示文本） | `model/params.json`；`model/baseline.yaml`；`drawings/FD-01_speed_axis_scenario_comparison.svg`；`review/systems/config/config_items.yaml`；`verification/browser-evidence.json` |
| **G11: PASS** | `no_open_severity_one_issue` | 内容层 S1（RK-02…06）已避免；跨域 S1（RK-15/16/17、RK-CR1/2/3、RK-V6-1…5）经本轮真实执行痕迹关闭（G1/G10 PASS、真实 GUI G4/G5、计数一致=11、无自引用、REVIEW_INDEX 就位） | Review of design（S1 关闭判据取真实执行痕迹 + 自引用审计） | `review/cost_risk/risk_register.md`；`verification/browser-evidence.json`；`review/REVIEW_INDEX.md`；`verification/vi_run_gates_output.txt` |

**整体：`overall: PASS`**（仅当 G1–G11 全部 `PASS`；本轮 11/11 通过）。

## 关键完成项（本轮最终目录内）

- **唯一最终目录就位**：`outputs/model/`、`outputs/drawings/`、`outputs/app/`、`outputs/review/`、`outputs/verification/`、`outputs/FINAL_ACCEPTANCE.md` 全部建立；`required_final_paths` 6 项实测存在。
- **`outputs/review/REVIEW_INDEX.md` 已创建（正式交付缺陷闭环）**：逐域列出 systems / physics / propulsion / archive / cost_risk 的索引/入口与实际相对路径，43 条路径经 `os.path.exists` 全数校验存在（见 `verification/acceptance.json` 的 `review_index_paths`）。
- **FD-01 修正脚注保留并纳入 G10**：`drawings/FD-01_speed_axis_scenario_comparison.svg` 第 63 行文本「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」实测存在；该展示文本与第一性原理复算的 1 mt 在 0.01c/0.03c/0.05c 下 `4.4938e21 / 4.0444e22 / 1.1234e23 J` 一致（相对差 ~8e-4 / 1.1e-3 / 3.1e-3，纯显示位数舍入），已在 G10 逐档核对。
- **真实浏览器三速切换 + JSON 导出**：真实无头 Chrome（CDP）点击 0.01c/0.03c/0.05c 三档，派生量随变；导出 `corona_S-0.05c_scenario.json`，`scenario_id`/字段/数值与页面当前情景一致；网络请求外部=0。证据 `verification/browser-evidence.json`。
- **检查计数无漂移**：清单声明项数 = `run_checks` 执行项数 = 报告输出项数 = **11**（由 `gates` 执行字典动态生成）。
- **最终报告无自引用**：本报告每门判定引用独立证据工件路径，不把报告自身当作证据，只作汇总。

## 审计与诚实边界

- 无 `unknown` 被当作 `verified_fact`；无 `construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed` 肯定式声称；真值标签与禁语均按基线纪律。
- 本报告逐门判定引用**独立证据工件路径**（`model/`、`app/`、`drawings/`、`review/`、`verification/` 下真实件），报告仅作汇总，不作为自身各门证据；检查计数与被测清单由本次实际执行记录动态生成。
- **OpenSCAD 本机未安装**（S3 受限项）：`RH-01_rotating_habitat.scad` 仅核验可编辑源与「概念级」注记，未做几何渲染；G6/G7 均如实声明此边界。
- 被测清单已排除 `__pycache__` / `*.pyc` 等运行缓存，避免把非交付缓存计入产物收集完整性；各域内亦实测无运行缓存。
- v6 整体以本次真实复跑判定为准。本节点（verification-integrator）不采信任何成员自述的最终验收；各专业复核材料仅作为待复核证据，最终逐门判定由本节点独立复跑得出，与 member 自述相互独立。

## 执行记录（独立证据）

- 门 G1–G11 判定与证据路径：`verification/acceptance.json`（机器可读）。
- G4/G5 真实浏览器交互证据：`verification/browser-evidence.json`。
- 本复跑人类可读执行记录：`verification/vi_run_gates_output.txt`。
- 复核材料索引：`review/REVIEW_INDEX.md`。
