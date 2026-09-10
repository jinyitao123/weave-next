# 日冕计划 · Pre-Phase A · v6 · 第四次正式交付修订 — 最终验收结论（FINAL ACCEPTANCE）

**运行**：`task-d591e695a9480bf22b28a40624e33ba2`（verification-integrator 独立复跑节点，同一新运行，未制造运行时中断，未补充团队 UUID）
**日期**：2026-09-10
**基线**：`corona-prephase-a-v1`（`frozen_for_validation`，速度轴 `0.01c / 0.03c / 0.05c`）
**权威验收源**：`/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json`
**　SHA-256** = `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`（本轮实测，与任务书一致）
**被测根目录**：本工作流唯一最终目录 `outputs/`（禁用任何其他目录充当交付通道）。

> 本件为 verification-integrator（独立验证与总装员）按职责独立复跑权威 `acceptance.json` 十一项硬门槛后之结论。
> 结论仅以本轮真实工具调用为据；未采信其他成员自述的最终验收，未以报告自身充当证据。

## 结论

本轮在唯一最终目录 `outputs/` 上，实际复跑权威 `acceptance.json` 全部十一项硬门槛（G1…G11）。
**十一项全部通过，门级整体判定：`overall: PASS`。**

- 检查计数 = **11**，由本轮实际执行的 11 个门函数字典动态产生（`check_count_from_execution`，非硬编码、非沿用）。
- 被测清单（`audited_file_inventory`）= **72**，由对 `outputs/` 的真实文件扫描冻结，**排除 `__pycache__` 与 `*.pyc`**，并排除报告自引用文件（`FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/vi_run_gates_output.txt`）。
- 逐门判定与计数、被测清单逐件 SHA-256、`review_index_paths` 均见 `verification/acceptance.json`（本轮真实生成，`verifier_node` = `corona-prephase-a-recovery-v6-verification-integrator`）；人类可读执行日志见 `verification/vi_run_gates_output.txt`；真实无头 Chrome(CDP) 交互证据见 `verification/browser-evidence.json`。

## 本轮闭合的收集缺口（73 → 75）

第三次运行 `run-d5e18a5c-86b0-5624-9d82-74a2c2584804` 总装员本地 `outputs/` 为 75 文件 / 540523 字节且 G1–G11、Chrome 三速、JSON 导出均通过，但 Workbench 最终候选仅保存 73 个具名文件，缺：
- `review/systems/verification/traceability_matrix.csv`（7662 B）
- `review/systems/verification/traceability_matrix.md`（11112 B）

缺件根因为旧的 512 KiB 有界聚合收集上限导致超限截断（缺 2 件合计 18774 B）。因此第三次运行**不得视为可采用**。平台已在提交 `ec9ea5f822d339b1f3ed37ab30b8a5071a444d70` 将有界聚合上限调整为 **1 MiB（1048576 B）**.

本轮处置：
- 唯一最终目录 `outputs/` **完整保留并实测为 75 个具名文件**——两项缺件现均位于规范相对路径且纳入包中：
  - `review/systems/verification/traceability_matrix.csv`，SHA-256 = `a8dce39bba2f4dbc0bc4f501c18e9fd8a564e491fed00f0707451d05d92b4645`
  - `review/systems/verification/traceability_matrix.md`，SHA-256 = `a6b05142ae1657382d08a7bf895214d8dd004f2d5cf861dab670ba0bfc558a96`（与冻结源逐字一致）
- `review_index_paths` 实测 `found=43 ok=true missing=[]`；`required_final_paths_checked` 六项（model / drawings / app / review / verification / FINAL_ACCEPTANCE.md）全部 `exists=true`。

## 十一项硬门槛逐项判定（逐行）

| # | 门（acceptance.json 键） | 判定 | 本轮真实证据 |
|---|---|---|---|
| G1 | `model_tests_pass` | **PASS** | `python3 -m unittest tests.test_model`（`outputs/model`）退出码 **0**，`Ran 26 tests … OK`。 |
| G2 | `three_speed_scenarios_present` | **PASS** | `corona_model` `CRUISE_SPEEDS_C=[0.01,0.03,0.05]`、`SCENARIO_IDS=[S-0.01c,S-0.03c,S-0.05c]`、`n=3`；`app/params.js` 含三速度；`drawings/FD-01` 三速度贯穿。 |
| G3 | `reference_calculations_within_tolerance` | **PASS** | 首性复算 5 项参考锚点全部落在各自相对容差内，**worst rel_err = 1.9262e-07**（重力锚点，容差 1e-2；次大 2.579e-08）。 |
| G4 | `app_starts_without_cloud_dependency` | **PASS** | 静态 `index.html` `src/href` 无任何 `http(s)://` 外部 URL；真实无头 Chrome(CDP) `app_loads_local` ok、`no_cloud_dependency` ok（`requests=4 external=0`）。 |
| G5 | `app_exports_scenario_json` | **PASS** | 真实无头 Chrome(CDP) 真实下载 `corona_S-0.05c_scenario.json`，`baseline_id=corona-prephase-a-v1`、`scenario_id=S-0.05c`、`consistent_with_page=true`；三情景切换 `S-0.01c/S-0.03c/S-0.05c` 全部核验通过。 |
| G6 | `drawings_are_parseable_and_editable` | **PASS** | 4 张 SVG（FA/FB/FC/FD-01）全部 `xmllint --noout` 解析 `OK`（`>=3`）；1 份 OpenSCAD `RH-01` 可编辑源存在（`editable_source=true`）。 |
| G7 | `all_drawings_marked_conceptual` | **PASS** | 扫描 4 SVG + 1 SCAD 全部含「概念级」注记，缺失数 = **0**（`checked>=4`）。 |
| G8 | `requirements_have_verification_methods` | **PASS** | 需求表提取 **14** 个 `SA-REQ-*`，`missing_method=[]`（>=10）；`traceability_matrix.csv` **31 行** `verification_method` 空值数 = **0**（该 CSV 本轮已入包）。 |
| G9 | `claims_use_truth_labels` | **PASS** | `params.json` 真值标签 = `[verified_fact, derived_result, assumption, unknown]`；`app.js` 含真值纪律（truth_discipline / forbidden_claims / open_unknowns 等）；禁语命中 = 0、非否定语境命中 = 0（`bad_context=[]`）。 |
| G10 | `cross_artifact_parameter_consistency_passes` | **PASS** | `params.json`↔`baseline.yaml` 一致；`params.js`↔`params.json` 一致；py↔js 跨语言 **worst rel_err = 0.0**（无失配）；`FD-01` 展示文本正确（`fd_correct=true`，1 mt / 1e9 kg / 4.49e21–1.12e23 J / ½mv²）；`config_items.yaml` 存在。 |
| G11 | `no_open_severity_one_issue` | **PASS** | 内容层 S1（RK-02..06）已避免；G1/G10 真跑通过；`browser-evidence.all_ok=true`；检查计数一致（11）；`review_index_paths.ok=true`；无报告自引用。 |

（上表每行「本轮真实证据」均取自本轮真实生成的 `verification/acceptance.json` 逐门 `detail` 字段，指向独立证据工件路径；本文件自身不作为证据。）

## 逐行门判定（机器可读）

```
G1: PASS
G2: PASS
G3: PASS
G4: PASS
G5: PASS
G6: PASS
G7: PASS
G8: PASS
G9: PASS
G10: PASS
G11: PASS
```

## 整体判定

```
OVERALL: PASS
```

**总体文案**：本轮针对第三次正式交付收集不完整的问题作单一闭合。唯一最终目录 `outputs/` 实测为 **75 个具名文件 / 544117 字节**（含此前缺失的两份 `traceability_matrix` 件），约为 **0.5189 MiB < 1 MiB 新上限**（余量 504459 B）；G1–G11 全部按权威 `acceptance.json` 真实复跑判定为 PASS；`review_index_paths.ok=true`；`__pycache__/*.pyc` 已排除；`FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/browser-evidence.json`、`verification/vi_run_gates_output.txt` 均为本轮真实运行重新生成。

## 判定口径与边界（如实申报）

- **本节点已验证**：G1–G11 全通过（本轮真实复跑，`vi_run_gates.py` 退出码 **0** 有界收敛，`check_count_from_execution=11`）；`__pycache__`/`*.pyc` 计数 = 0；`outputs/` 具名文件数 = **75**、实际总字节 = **544117 B**（实测；本轮重新生成 4 件验证工件所致，较冻结基线 540523 B 之差 = `FINAL_ACCEPTANCE.md` 本件重写 + `browser-evidence.json`/`acceptance.json`/`vi_run_gates_output.txt` 重跑，均 < 1 MiB，余量 504459 B）；两份缺件均位于规范相对路径且 SHA-256 与冻结源逐字一致；`review/REVIEW_INDEX.md` 引用的 43 条相对路径全部命中（`find` 实测）。**其余 71 件**逐字节复用冻结源，按冻结 `manifest.json` 逐文件 SHA-256 核对全部匹配（71/71）。
- **平台侧确认项**：`artifact_collection.complete` 与 Workbench 最终候选是否实际包含全部 75 个具名文件，由平台在收集本节点 `outputs/` 时最终决定。本节点无直接读取该内部收集标志的工具；已证实 = 75 个具名文件 + 无缓存（0）+ 总字节 544117 B < 1 MiB 上限（504459 B 余量），满足性证据成立，最终以平台保存的候选集为准。第三次的超限截断为纯体积上限所致，本轮上限已足容。
- **OpenSCAD 本机未安装**（S3 受限项）：G6/G7 对 `RH-01_rotating_habitat.scad` 仅核验**可编辑源存在**与**「概念级」注记**，**未做几何渲染/编译**——如实声明；G6/G7 按 `acceptance.json` 口径（可编辑源 + 概念级注记）判定通过，不构成对门槛的跳过。
- **既有只读观察（非本节点引入、不改动）**：冻结的 `outputs/app/index.html` 页脚链接 `../verification/machine_verification_checklist.md` 与 `../verification/verification_report.md`，此二者不在 75 件冻结清单内，属页脚普通导航链接（非脚本/资源依赖），对 G4/G5 及应用加载、三速切换、JSON 导出均无影响（本轮真实 Chrome 实测 `all_ok=true`）。按「逐字节复用、不改动」纪律未对冻结 app 内容作任何修改；此为如实申报的悬挂链接提示，不构成验收失败。
- **未做任何功能改动**：除真实重跑并重新生成四项验证工件（`FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/browser-evidence.json`、`verification/vi_run_gates_output.txt`）及纳入两项 `traceability_matrix` 补件外，其余内容件均逐字节复用冻结源。
- **外部副作用范围 = `none`**：仅向本工作流唯一 `outputs/` 写入；未写入只读冻结源；未跨节点修改其他专业件；未触发任何对外服务。

## 交付物索引（本轮生成/重写件，相对 `outputs/`）

- `outputs/FINAL_ACCEPTANCE.md`（本件，本轮重新生成，基于本轮真实复跑结果）
- `outputs/verification/acceptance.json`（本轮真实复跑报告，重新生成；`overall=PASS`）
- `outputs/verification/browser-evidence.json`（本轮真实无头 Chrome 交互证据，重新生成；`all_ok=true`）
- `outputs/verification/vi_run_gates_output.txt`（本轮人类可读执行日志，重新生成）
- `outputs/review/systems/verification/traceability_matrix.csv`（本轮纳入的补件，7662 B，SHA-256 `a8dce39b…`）
- `outputs/review/systems/verification/traceability_matrix.md`（本轮纳入的补件，11112 B，SHA-256 `a6b05142…`）
