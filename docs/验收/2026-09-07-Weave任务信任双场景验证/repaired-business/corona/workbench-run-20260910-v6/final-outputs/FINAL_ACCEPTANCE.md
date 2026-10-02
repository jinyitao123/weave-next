# 日冕计划 · Pre-Phase A · v6 最终验收（FINAL ACCEPTANCE）

**验证节点**：`corona-prephase-a-recovery-v6-verification-integrator`（独立验证与总装员）
**验证日期**：2026-09-10
**权威验收文件**：`<MAINTAINER_LOCAL_PATH>`（SHA-256 `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`，与任务书一致）
**判定方式**：本总装员在本次真实执行中**独立复跑**原 `acceptance.json` 十一项硬门槛；每门判定指向**独立证据工件路径**（该工件路径指向 `verification/`、`model/`、`app/`、`drawings/`、`review/` 等交付件，非本报告自身章节，非成员自述）。

**检查计数**：11（由本次实际执行记录动态生成）。
**被测文件冻结清单**：67 个文件（逐一记录 SHA-256，见 `verification/acceptance.json` 的 `audited_file_inventory`）。
**唯一最终目录**：`outputs/model/` `outputs/drawings/` `outputs/app/` `outputs/review/` `outputs/verification/` `outputs/FINAL_ACCEPTANCE.md`。

## 逐门判定（G1 … G11）

| 门 | 判定 | 门槛 | 方法 | 独立证据工件 |
|---|---|---|---|---|
| **G1: PASS** | `model_tests_pass` | `PASS` | Test python3 -m unittest tests.test_model | `model/tests/test_model.py`; `verification/vi_acceptance_output.txt` |
| **G2: PASS** | `three_speed_scenarios_present` | `PASS` | Inspection model/app/drawing 三速度贯穿 | `model/params.json`; `model/corona_model.py`; `app/params.js`; `drawings/FD-01_speed_axis_scenario_comparison.svg` |
| **G3: PASS** | `reference_calculations_within_tolerance` | `PASS` | Analysis first-principles + model cross | `model/baseline.yaml`; `model/anchors_reverified_2026-09-09.txt`; `verification/vi_acceptance_output.txt` |
| **G4: PASS** | `app_starts_without_cloud_dependency` | `PASS` | Inspection static + Demonstration headless Chrome (CDP) | `app/index.html`; `verification/browser-evidence.json` |
| **G5: PASS** | `app_exports_scenario_json` | `PASS` | Demonstration headless Chrome (CDP) real download | `app/app.js`; `verification/browser-evidence.json` |
| **G6: PASS** | `drawings_are_parseable_and_editable` | `PASS` | Test xmllint parse + Inspection editable source | `drawings/FA-01_laser_sail_precursor.svg`; `drawings/RH-01_rotating_habitat.scad` |
| **G7: PASS** | `all_drawings_marked_conceptual` | `PASS` | Inspection concept-level label scan | `drawings/FA-01_laser_sail_precursor.svg`; `drawings/FD-01_speed_axis_scenario_comparison.svg` |
| **G8: PASS** | `requirements_have_verification_methods` | `PASS` | Review of design + Inspection | `review/systems/requirements/requirements_decomposition.md`; `review/systems/verification/traceability_matrix.csv` |
| **G9: PASS** | `claims_use_truth_labels` | `PASS` | Inspection truth-label + forbidden-claim context | `review/systems/routes/route_terminal_states.md`; `app/app.js`; `model/params.json` |
| **G10: PASS** | `cross_artifact_parameter_consistency_passes` | `PASS` | Inspection + Test cross-consistency (incl FD-01 displayed text) | `model/params.json`; `model/baseline.yaml`; `drawings/FD-01_speed_axis_scenario_comparison.svg`; `review/systems/config/config_items.yaml`; `verification/browser-evidence.json` |
| **G11: PASS** | `no_open_severity_one_issue` | `PASS` | Review of design (severity-one closure) | `review/cost_risk/risk_register.md`; `verification/acceptance.json`; `verification/machine_verification_checklist.md` |

**整体：`overall: PASS`**（仅当 G1–G11 全部 `PASS` 时判 `PASS`）。

## 关键修复项（本次最终目录内）

- `drawings/FD-01_speed_axis_scenario_comparison.svg` 面板 B 脚注量级错误（原「5 万吨级 / 单帆 4.5 GJ 至 112 GJ」）：已修正为「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」，并把图中展示文本纳入 G10 跨产物一致性复核。
- 检查计数漂移（v4 缺陷①）：`machine_verification_checklist.md` 声明项数 = `run_checks.py` 实际运行项数 = 报告输出项数（= 18）。
- 最终报告自引用（v4 缺陷②）：本项目逐门判定引用独立证据工件路径（见上方逐门表），不以报告自身章节充当证据。
- 应用真实切换 0.01c/0.03c/0.05c 与导出 JSON 与页面一致（真实无头 Chrome + CDP 证据 `verification/browser-evidence.json`）。

## 审计与诚实边界

- 无 `unknown` 被当作 `verified_fact`；无 `construction_ready/manufacturing_ready/flight_certified/whole_program_cost_committed` 肯定式声称。
- 本项目逐门判定引用**独立证据工件路径**（`verification/`、`model/`、`app/`、`drawings/`、`review/` 等交付件），报告仅作汇总，不作为自身各门的证据来源。
- OpenSCAD 本机未安装：`RH-01_rotating_habitat.scad` 仅核验可编辑源与「概念级」注记，未做几何渲染（S3 受限项）。
- v6 整体以本次真实复跑判定为准；未运行/失败的必写 `FAIL`。
