# FINAL_ACCEPTANCE.md — 日冕计划 Level-0 / Pre-Phase A 三路线先期论证 · 最终验收

- 文件：CI-COR-FINAL-001 v1.0.0
- 日期：2026-09-06
- 编制节点：verification-integrator（对抗审查 / 独立复算 / 跨产物一致性 / 最终总装）
- 基线：baseline_id `corona-prephase-a-v1` / CI-COR-BASE-001 v1.0.0（status: frozen_for_validation）
- 边界声明：本验收针对概念级（Level-0 / Pre-Phase A）论证成果；全部产物不构成施工级、制造级或飞行认证级设计 [verified_fact]

---

## 1. 最终判定：**通过（PASS）**

判定前提（本节点职责声明的验收条件）全部满足：
- 必交产物类型齐全：outputs/model/、outputs/drawings/、outputs/app/、outputs/review/、outputs/verification/ 五类均有实质内容并经本节点实际打开/执行核实 [verified_fact]；
- 模型测试全过（24/24，本节点重跑，exit=0）；
- 应用为纯静态离线正式产物（无构建服务器依赖；baseline.js 由冻结基线同源生成），页面交互链路（情景切换→实时计算→JSON 导出）经 node 复演与平台独立浏览器导出样例双向验证通过；
- 图纸可解析（4×SVG xmllint PASS + index.html PASS）、可编辑（OpenSCAD 参数化自包含，结构检查 PASS）、全部标注概念级；
- 跨产物参数一致性检查通过（模型↔应用 9 组数值 rel-tol 1e-12 一致，版本四方一致）；
- 无未关闭 severity-1 问题（open_severity_1_count = 0）。

本判定不采信任何上游简报文字结论；全部依据为本节点 2026-09-06 对真实阶段文件的独立读取、独立复算与重跑，证据见 `outputs/verification-integrator/INTEGRATOR_EVIDENCE.md` 与 `independent_recalc_results.json` [verified_fact]。

## 2. acceptance.json 十一条硬门逐项终判

| # | 硬门 | 判定 | 本节点证据（均为本轮真实执行） |
|---|---|---|---|
| HG-01 | model_tests_pass | **PASS** | 重跑 `test_model.py`：24/24 OK，exit=0 |
| HG-02 | three_speed_scenarios_present | **PASS** | 模型 TestThreeScenarios 4 项；DWG-001 图面三情景注记；TRL §5 三情景动能表；physics/analysis/architecture 三情景表；应用 scenario_count=3；无"仅 0.03c 基线"残留 |
| HG-03 | reference_calculations_within_tolerance | **PASS** | 本节点一手复算 5/5 在容差内（最大相对误差 2.6e-8，容差 1e-2/1e-3） |
| HG-04 | app_starts_without_cloud_dependency | **PASS** | 纯静态 `file://` 应用；外部资源扫描零命中；index.html 解析 PASS |
| HG-05 | app_exports_scenario_json | **PASS** | `test_app_calc.js` 导出结构检查全过；复演导出与 `verification/results/app_export_sample.json`（0.03c）及平台独立浏览器导出（0.05c）逐值一致，且与一手真值一致 |
| HG-06 | drawings_are_parseable_and_editable | **PASS** | 4×SVG xmllint PASS；COR-SCAD-001 结构检查 PASS（5 模块+总装、自包含；本机无 openscad，未编译渲染，如实记录） |
| HG-07 | all_drawings_marked_conceptual | **PASS** | 5 个图纸文件逐一核实：均含概念级标注（×7–12）+ 图号 + baseline_version 1.0.0 |
| HG-08 | requirements_have_verification_methods | **PASS** | REQ_DECOMPOSITION.md 36 条需求行均带 I/A/T/D 验证方法；TRACEABILITY_MATRIX 双向追踪到硬门 |
| HG-09 | claims_use_truth_labels | **PASS** | 四类标签在全部七个产物区实质分布；关键数值带单位/公式/输入/来源/baseline_version |
| HG-10 | cross_artifact_parameter_consistency_passes | **PASS** | 重跑 `run_verification.sh`：OVERALL PASS，exit=0；模型↔应用 9 组数值 rel-tol 1e-12 一致；撤回值全库扫描通过 |
| HG-11 | no_open_severity_one_issue | **PASS** | risk_register.yaml `open_severity_1_count: 0`；验证清单 NC-1/NC-2（验证脚本自身缺陷）已修复复验，无遗留 |

## 3. 独立复算摘要（本节点一手计算，非引用）

| 算例 | 期望值 | 本节点复算值 | 判定 |
|---|---|---|---|
| 0.03c 航行时间 | 141.6666667 yr | 141.6666667 yr | PASS |
| 1 Mt@0.03c 动能下限 | 4.0443983e22 J | 4.044398304e22 J | PASS |
| 5 Mt@0.03c 动能下限 | 2.0221991e23 J | 2.022199152e23 J | PASS |
| 1 km/2 rpm 人工重力 | 43.8649 m/s² | 43.86490845 m/s² | PASS |
| 1 mg@0.03c 尘埃动能 | 40443983 J | 40443983.04 J | PASS |

三情景复核：425.0 / 141.667 / 85.0 yr；1 Mt 动能下限 4.4938e21 / 4.0444e22 / 1.1234e23 J；1 mg 尘埃 4.49 / 40.44 / 112.34 MJ；先锋 0.2c 最早回传 25.5 yr。全部与冻结基线、纪要修订稿及各产物一致 [derived_result]。

## 4. 交付物清单（唯一当前版本）

| 类型 | 路径 | 内容 |
|---|---|---|
| 统一模型 | outputs/model/ | corona_model.py（八域覆盖任务书 §4.1）、baseline_frozen.yaml、yaml_lite.py、test_model.py（24 测试）、TEST_RESULTS.txt |
| 概念图纸 | outputs/drawings/ | COR-DWG-001/002/003（SVG）、COR-DRW-ARCH-001（SVG）、COR-SCAD-001（OpenSCAD）、DRAWINGS_INDEX.md、COR-DRW-REQ-001 |
| 数字应用 | outputs/app/ | index.html/app.js/calc.js/style.css/baseline.js（同源生成）、test_app_calc.js、README.md |
| 论证包 | outputs/review/ | TRL_ASSESSMENT、WBS(+yaml)、COST_RESEARCH_METHOD、RISK_REGISTER(+yaml)、TERMINATION_CONDITIONS、RESEARCH_TOPIC_PACKAGE |
| 验证包 | outputs/verification/ | VERIFICATION_CHECKLIST、run_verification.sh、cross_consistency_check.py、scad_structural_check.py、results/（8 证据文件+日志+导出样例） |
| 支撑产物 | outputs/architecture/、analysis/、physics/、payload/、reliability/、baseline/ | 需求分解/接口登记/任务终态/追踪矩阵/配置基线；推进能源热控比较；物理复核；档案架构；长期可靠性；基线冻结记录 |
| 本文件 | outputs/FINAL_ACCEPTANCE.md | 最终验收判定（本节点本轮新写） |

## 5. 事实纪律终检

- 四类事实标签贯穿全部产物；九项 mandatory_unknowns（尘埃通量、粒径分布、材料性能、推进效率、减速方案、长期可靠性、封闭生态、档案复苏、设备再制造）全部显式保留并映射课题 [verified_fact]。
- 动能一律标注"下限非完整推进能源预算"；克级先锋可行性未外推；冬眠条款单列且不构成成立条件；已撤回值（450 MJ、2.4–3.4 万亿元、0.03c 唯一基线、旧 8/20/35/40 年门）全库扫描无非撤回语境残留 [verified_fact]。

## 6. 残余观察项与限制（不构成不通过）

- **OBS-1** [severity-3 级]：任务书 §4.4 六项内容中，任务需求基线实体位于 outputs/architecture/、三路线比较实体位于 outputs/analysis/（经追踪矩阵挂接至 review 体系）；"独立否定意见"未独立成文，由 RISK_REGISTER §3、TERMINATION_CONDITIONS 与 severity-1=0 记录承载。建议下一轮专家评审前补一份独立否定意见短文或将上述实体归拢入 review/ 目录镜像。
- **LIM-1**：本机无 openscad 可执行文件，SCAD 未编译渲染，仅结构级验证；评审环境可用 `openscad -o /dev/null COR-SCAD-001_archive_payload_concept.scad` 补跑。
- **LIM-2**：页面交互判据为 node 调用链复演 + DOM 绑定静态核查 + 平台同日独立浏览器导出样例三方互证；本节点未自行启动图形浏览器。
- 过程验收（任务书 §6.1，平台层：运行时可见性、纠偏安全点、恢复能力等）超出本节点可独立核实范围，本文件不作判定，仅交付专业成果层（§6.2）证据。

## 7. 专业成果层结论

acceptance.json 十一条硬门全部通过；三速度情景贯穿模型、图纸、应用与论证材料；关键基线计算与本节点独立复算在容差内一致；应用离线启动、情景调整与 JSON 导出验证通过；图纸可解析、可编辑、概念级标注齐全；需求—证据—假设—未知项—验证结果双向追踪在位；无未关闭 severity-1 问题。

**专业成果验收（§6.2）：通过。** 成果结构可直接交下一轮专家评审，OBS-1 为会前建议整理项。

---
*证据锚点：outputs/verification-integrator/INTEGRATOR_EVIDENCE.md、independent_recalc.py、independent_recalc_results.json（本节点本轮新写）；模型/应用/图纸/验证重跑以各平台工作区真实文件为对象，命令与退出码见证据记录。*
