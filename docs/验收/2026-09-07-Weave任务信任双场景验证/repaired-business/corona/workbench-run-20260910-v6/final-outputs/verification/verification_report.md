# 日冕计划 v6 · 数字工程交付 · 机器验证清单与报告

**产出**：digital-engineering-builder（corona-prephase-a-recovery-v6-digital-engineering-builder）
**日期**：2026-09-10
**结果**：`PASS`（18 项检查，计数由本次实际执行动态产生）
**命令**：`python3 run_checks.py`（有界退出，exit 0=通过 / 1=失败）；全步骤带超时，不启动前台开发服务器。

| # | 检查项 | 方法 | 结果 | 说明 |
|---|---|---|---|---|
| 1 | `model_tests_pass` | Test/Run python3 -m unittest tests.test_model | PASS | returncode=0 last=['Ran 26 tests in 0.001s', '', 'OK'] |
| 2 | `model_reference_anchors_within_tolerance` | Test/Analysis corona_model.reference_check() | PASS | 5/5 anchors, worst rel_err=1.926e-07, all_ok=True |
| 3 | `params_json_matches_baseline_yaml` | Inspection params.json vs baseline.yaml | PASS | baseline_id=corona-prephase-a-v1 speeds=[0.01, 0.03, 0.05] |
| 4 | `params_js_matches_params_json` | Test node js_model_runner.js vs params.json | PASS | baseline_id=corona-prephase-a-v1 speeds=[0.01, 0.03, 0.05] |
| 5 | `app_model_matches_python_model` | Test cross-consistency py↔js | PASS | max rel_err=0.000e+00, 24 fields/36 checked |
| 6 | `app_no_external_resource_reference` | Inspection index.html src/href 字段 | PASS | refs=11 external=[] |
| 7 | `forbidden_claims_not_asserted` | Inspection 关键字扫描（肯定式禁语） | PASS | clean |
| 8 | `drawings_parseable_and_labeled` | Test XML 解析 (xmllint) + 内容扫描 | PASS | svgs=4 scads=1 parse_ok=True labeled=True |
| 9 | `openscad_editable_source_present` | Inspection editable source | PASS | scads=['RH-01_rotating_habitat.scad'] labeled=True（本机未装 openscad，仅核验可编辑源与注记） |
| 10 | `truth_discipline_present` | Inspection app.js + export structure | PASS | app.js contains truth_discipline/forbidden_claims/open_unknowns/unknown |
| 11 | `app_gui_app_loads_local` | Demonstration headless Chrome (CDP) | PASS | AppAPI/CoronaModel 存在 |
| 12 | `app_gui_default_scenario_0_03c` | Demonstration headless Chrome (CDP) | PASS | initial=S-0.03c |
| 13 | `app_gui_switch_S-0.01c` | Demonstration headless Chrome (CDP) | PASS | {"id":"S-0.01c","c":"0.01","ta":425,"ke":4.493775893684088e+21,"v":2997.92458} |
| 14 | `app_gui_switch_S-0.03c` | Demonstration headless Chrome (CDP) | PASS | {"id":"S-0.03c","c":"0.03","ta":141.66666666666669,"ke":4.044398304315679e+22,"v":8993.77374} |
| 15 | `app_gui_switch_S-0.05c` | Demonstration headless Chrome (CDP) | PASS | {"id":"S-0.05c","c":"0.05","ta":85,"ke":1.1234439734210221e+23,"v":14989.6229} |
| 16 | `app_gui_export_scenario_json` | Demonstration headless Chrome (CDP) | PASS | {"filename":"corona_S-0.05c_scenario.json","scenario_id":"S-0.05c","baseline_id":"corona-prephase-a-v1","routes":3,"unknown_keys":["propulsion_efficiency","dust_flux_at_velocity","cost_reduction_evidence","sail_material_at_scale"],"truth_labels":["verified_fact","derived_result","assumption","unknown"],"consistent_with_page":true,"page_id":"S-0.05c"} |
| 17 | `app_gui_no_cloud_dependency` | Demonstration headless Chrome (CDP) | PASS | requests=4 external=0 |
| 18 | `app_gui_gui_harness` | Demonstration headless Chrome (CDP) | PASS | CDP 驱动真实 Chrome 完成 |

## 覆盖的产物与独立证据

本表所列每条检查均给出**独立可复跑命令/判据**（见 `machine_verification_checklist.md`），
不依赖本报告自身章节充当证据；gate 4/5/10 的判定依据指向下方独立证据工件路径，而非「见本报告 §X」。

- 统一模型 `outputs/model/`（G1 model_tests_pass / G3 reference_calculations_within_tolerance）
- 数字应用 `outputs/app/`（G4 app_starts_without_cloud_dependency / G5 app_exports_scenario_json / G10 cross_artifact_parameter_consistency_passes）
- 概念图纸 `outputs/drawings/`（G6 drawings_are_parseable_and_editable / G7 all_drawings_marked_conceptual）
- 事实纪律 / 真值标签 / 禁用语（G9 claims_use_truth_labels 的机器侧支撑）

**独立证据工件（引证时优先使用，勿以“本文件”自证）：**
- 模型单元测试：`python3 -m unittest tests.test_model`（在 outputs/model，exit 0）→ 结果文本 `model/tests/` 输出或下方报告行
- 参考锚点复算：`python3 -c "import corona_model as c; print(c.reference_check())"`（0.03c 五锚点，worst rel_err 见报告行 2）
- 应用 GUI 真实交互（gate 4/5）：`node gui_verify.js outputs/app <chrome>` → stdout JSON 记录默认情景/三情景切换/导出文件/外部请求=0
- 跨产物一致性（gate 10）：`node js_model_runner.js outputs/app` 与 `corona_model.py` 交叉比对，max rel_err ≤ 1e-9

## 计数一致性说明（v4 检查计数漂移修复）

v4 中 `machine_verification_checklist.md` 手写声明 M1–M12（12 项）与实际 `run_checks.py` 注册的 
18 项检查存在漂移。v6 起本清单与报告**均由 rep.checks 动态生成**，二者计数恒等于本次实际执行的检查数（== 清单行数），
清单逐行对应**一次真实执行**的每一项检查，不再有独立手写子集。

## 诚实边界

1. 本清单由**生产者**（digital-engineering-builder）自身运行并自证；按 CR-003 / REQ-V4-P-01/02，
   十一项硬门槛的**独立复跑**与最终判定属 verification-integrator（独立验证与总装员），本节点不代做。
2. OpenSCAD 本机未安装，`openscad_editable_source_present` 仅核验可编辑源文件与概念级注记，未做几何渲染。
3. GUI 验证以**真实无头 Chrome + CDP 交互**完成（点击情景按钮、触发导出下载、统计网络请求），不使用 Node DOM-shim。

## 关于 v4 最终报告自引用的处置

v4 `FINAL_ACCEPTANCE.md` 存在**自引用缺陷**：以「本文件」作为自身 `required_final_path` 的证据，
并以「见 §三/§四」「本总装员已复跑闭合」作为 gates 4/5/10 的判定依据。本节点（数字工程）提供
`machine_verification_checklist.md` 作为**逐条独立复跑指引**，并在下文给出**独立证据工件路径**，
使 verification-integrator 在 v6 重写 `FINAL_ACCEPTANCE.md` 时改为指向真实证据工件而非自引用。
（`FINAL_ACCEPTANCE.md` 的最终判定内容由独立验证与总装员节点重写，本节点不代为生成。）
