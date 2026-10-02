# 日冕计划 · Pre-Phase A · v6 · 第五次正式交付修订（r5）— 最终验收结论（FINAL ACCEPTANCE）

**修订序号**：第五次正式交付修订（r5，整次修订）
**本件由**：`corona-prephase-a-recovery-v6-verification-integrator`（独立验证与总装员）本轮独立复跑后生成
**日期**：2026-09-10
**基线**：`corona-prephase-a-v1`（速度轴 `0.01c / 0.03c / 0.05c`）
**权威验收源**：`<MAINTAINER_LOCAL_PATH>`
**　SHA-256** = `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`（本轮实测一致）
**被测根目录**：本工作流唯一最终目录 `outputs/`（禁用任何其他目录充当交付通道）。

> 结论仅以本轮真实工具调用为据。本轮未采信任何其他成员的自述最终验收；门级判定、检查计数、被测清单、链接检查
> 均由本轮在唯一 `outputs/` 上独立复跑产生，且以独立证据工件路径充当证据，不以本报告自身充当证据。

## 结论

本轮在唯一最终目录 `outputs/` 上，实际复跑权威 `acceptance.json` 全部十一项硬门槛（G1…G11），
**十一项全部通过，整体判定：`overall: PASS`**。

- 检查计数 = **11**，由本轮实际执行的 11 个门函数字典动态产生（`check_count_from_execution`，非硬编码、非沿用）。
- 被测清单（`audited_file_inventory`）= **72**，由对 `outputs/` 的真实文件扫描冻结，**排除 `__pycache__` 与 `*.pyc`**，
  并排除报告自引用文件（`FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/vi_run_gates_output.txt`）。
- 具名文件数 = **75**；实测总字节 = **545321** 字节（最终树，含本报告，测于最终化时刻）；`__pycache__` / `*.pyc` 计数 = **0**；
  两份 `traceability_matrix` 均位于规范相对路径且字节/内容与冻结源逐字一致。
- 逐门判定、计数、被测清单逐件 SHA-256、`required_final_paths_checked`、`review_index_paths` 见本轮真实生成的
  `verification/acceptance.json`（`verifier_node = corona-prephase-a-recovery-v6-verification-integrator`）；
  人类可读执行日志见 `verification/vi_run_gates_output.txt`；真实无头 Chrome(CDP) 交互证据见
  `verification/browser-evidence.json`（三者均为报告自引用工件，不列入被测清单、不以自身充当证据）。

## 本轮内容变更范围（整次修订 r5，非成员级纠偏）

**本轮只闭合第四次平台候选的两个失效页脚链接，并如实验证终态修订成本。**

第四次运行 `run-8eda7ade-66e4-504e-9f99-63d0aa2b9342` 已由平台完整保存 **75 个具名文件、544117 字节**，
`artifact_collection.complete = true`，两份 `traceability_matrix` 存在，G1–G11、真实 Chrome 三速切换与 JSON 导出均经独立复跑通过。
但独立本地链接检查（cache 排除）发现：

- `outputs/app/index.html` 第 **132–133 行** 的两个页脚链接指向**不存在的** `verification/machine_verification_checklist.md`
  与 `verification/verification_report.md`，实际点击会失效。

因此第四次运行**不得视为可采用**。已完成运行不支持成员级纠偏，只能启动**本整次修订（r5）**，该过程成本在本件如实记录。

本修订经有界修改后，唯一内容变更（r4→r5）为 `outputs/app/index.html` 第 132–133 行两处页脚链接：

- `verification/machine_verification_checklist.md` → `../verification/acceptance.json`，链接文字改为 `acceptance.json`
- `verification/verification_report.md` → `../verification/browser-evidence.json`，链接文字改为 `browser-evidence.json`

两目标均为最终目录内真实文件（本轮实测存在）：`outputs/verification/acceptance.json`、`outputs/verification/browser-evidence.json`。
**其余 74 个文件**与冻结源（`2026-09-10-corona-v6-formal-r3-linkfix-source/outputs`）逐字节一致；所述 r5 变更在冻结源中已完成并冻结。

**本节点对最终树与冻结源的字节比对**：仅 `FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/browser-evidence.json`
三个文件内容不同（均为本轮重新生成的验证/报告工件）；`verification/vi_run_gates_output.txt` 与其字节一致（确定性输出）。全部内容件
（`model/`、`drawings/`、`app/`、`review/`）与冻结源逐字节一致；`app/index.html` 第 132–133 行保持修复态。

## 本地链接解析检查（独立于 G4，cache 排除）

对 `outputs/` 下全部 HTML/SVG 的本地 `href` / `src` / `url(...)` 引用做真实文件解析：

- 扫描文件数 = **6**（`app/index.html` + 5 张 SVG）
- 本地引用检查数 = **11**（动态），**缺失 = 0**，解析命中 = 11
- 全部命中真实文件：`app/app.js`、`app/model.js`、`app/params.js`、5 张图纸（`drawings/FA-01…SVG` `FB-01…` `FC-01…` `FD-01…` `RH-01…scad`）、
  `model/README.md`、`verification/acceptance.json`、`verification/browser-evidence.json`。
- **结果：`missing = 0`（cache 排除）。** 本轮两个修复后的页脚链接 `../verification/acceptance.json`、
  `../verification/browser-evidence.json` 均命中最终目录内真实文件。

## 十一项硬门槛逐项判定（逐行）

| # | 门（acceptance.json 键） | 判定 | 本轮真实证据 |
|---|---|---|---|
| G1 | `model_tests_pass` | **PASS** | `python3 -m unittest tests.test_model`（`outputs/model`）退出码 **0**，`Ran 26 tests … OK`。 |
| G2 | `three_speed_scenarios_present` | **PASS** | `corona_model` `CRUISE_SPEEDS_C=[0.01,0.03,0.05]`、`SCENARIO_IDS=[S-0.01c,S-0.03c,S-0.05c]`、`n=3`；`app/params.js` 含三速度；`drawings/FD-01` 三速度贯穿。 |
| G3 | `reference_calculations_within_tolerance` | **PASS** | 首性复算 5 项参考锚点全部落在各自相对容差内（`all_ok=true`，`worst_rel_err=1.926e-07` = 重力锚点，`n=5`）。 |
| G4 | `app_starts_without_cloud_dependency` | **PASS** | 静态 `index.html` `src/href` 无任何 `http(s)://` 外部 URL；真实无头 Chrome(CDP) `app_loads_local` ok、`no_cloud_dependency` ok（`requests=4 external=0`）。 |
| G5 | `app_exports_scenario_json` | **PASS** | 真实无头 Chrome(CDP) 真实下载 `corona_S-0.05c_scenario.json`，`baseline_id=corona-prephase-a-v1`、`scenario_id=S-0.05c`、`consistent_with_page=true`；三情景切换 `S-0.01c/S-0.03c/S-0.05c` 全部核验通过。 |
| G6 | `drawings_are_parseable_and_editable` | **PASS** | 4 张 SVG（FA/FB/FC/FD-01）全部 `xmllint --noout` 解析 `OK`（`>=3`）；1 份 OpenSCAD `RH-01` 可编辑源存在。（`>=3` SVG + 可编辑源满足门限。） |
| G7 | `all_drawings_marked_conceptual` | **PASS** | 扫描 4 SVG + 1 SCAD 全部含「概念级」注记，缺失数 = **0**。 |
| G8 | `requirements_have_verification_methods` | **PASS** | 需求表提取 `SA-REQ-*`（`req_count=14`）全部含验证方法（`missing_method=[]`）；`traceability_matrix.csv` 逐行 `verification_method` 非空（`traceability_total=31`，`tm_empty=0`）。 |
| G9 | `claims_use_truth_labels` | **PASS** | `params.json` 真值标签 `['verified_fact','derived_result','assumption','unknown']`；`app.js` 含真值纪律；禁语命中 0、非否定语境命中 `bad_context=[]`。 |
| G10 | `cross_artifact_parameter_consistency_passes` | **PASS** | `params.json`↔`baseline.yaml` 一致；`params.js`↔`params.json` 一致；py↔js 跨语言 `worst_rel_err=0.0`；`FD-01` 展示文本正确；`config_items.yaml` 存在。 |
| G11 | `no_open_severity_one_issue` | **PASS** | 内容层 S1（RK-02..06）已避免；G1/G10 真跑通过；`browser-evidence.all_ok=true`；检查计数一致（11）；`review_index_paths.ok=true`；无报告自引用（`selfref_bad=[]`）。 |

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

**总体文案**：本轮针对第四次平台候选两个失效页脚链接的问题作整次修订（r5）。唯一最终目录 `outputs/` 实测为
**75 个具名文件**；`__pycache__` / `*.pyc` 计数 = **0**；本地链接检查 `missing=0`（cache 排除）；G1–G11 全部按权威
`acceptance.json` 真实复跑判定为 PASS；`review_index_paths.ok=true`（43 条引用全部命中，`missing=[]`）；
`FINAL_ACCEPTANCE.md`、`verification/acceptance.json`、`verification/browser-evidence.json` 均为本轮真实运行重新生成
（其余 71 件逐字节复用冻结源；`verification/vi_run_gates_output.txt` 为确定性输出，与本轮真实运行字节一致）。

## 判定口径与边界（如实申报）

- **本节点已验证**（本轮真实复跑，`vi_run_gates.py` 退出码 **0**，`check_count_from_execution=11`）：G1–G11 全通过；
  `outputs/` 具名文件数 = **75**、无 `__pycache__`/`*.pyc`；两份 `traceability_matrix`（`.md`/`.csv`）均位于规范相对路径且
  字节/内容与冻结源逐字一致；`review/REVIEW_INDEX.md` 引用的相对路径全部命中；`app/index.html` 第 132–133 行两个页脚链接
  均指向最终目录内真实文件（`missing=0`）。其余内容件逐字节复用冻结源。
- **整次修订（r5）成本如实记录**：已完成运行不支持成员级纠偏；第四次平台候选因两个失效页脚链接不得视为可采用，故启动本整次修订。
  整个过程成本 = 本轮重新生成 3 件验证/报告工件（`acceptance.json`、`browser-evidence.json`、`FINAL_ACCEPTANCE.md`）+
  复用冻结源 + 清理运行副产品（`__pycache__`/`*.pyc`）；不对外发布、无网络或跨目录副作用（`外部副作用 = none`）。
- **平台侧确认项**：`artifact_collection.complete` 与 Workbench 最终候选是否实际包含全部 75 个具名文件，由平台在收集本节点
  `outputs/` 时最终决定。本节点无直接读取该内部收集标志的工具；已证实 = 75 个具名文件 + 无缓存（0）+ 本地链接 `missing=0`
  的满足性证据成立；最终以平台保存的候选集为准。
- **OpenSCAD 本机未安装**（S3 受限项）：G6/G7 对 `RH-01_rotating_habitat.scad` 仅核验**可编辑源存在**与**「概念级」注记**，
  **未做几何渲染/编译**——如实声明；不构成对门槛的跳过（门限为 `>=3` SVG 可解析 + 可编辑源存在，均满足）。
- **外部副作用范围 = `none`**：仅向本工作流唯一 `outputs/` 写入；未写入只读冻结源；未跨节点修改其他专业件；未触发任何对外服务。

## 交付物索引（本轮生成/重写件，相对 `outputs/`）

- `outputs/FINAL_ACCEPTANCE.md`（本件，本轮重新生成，基于本轮真实复跑结果）
- `outputs/verification/acceptance.json`（本轮真实复跑报告，重新生成；`overall=PASS`，`check_count_from_execution=11`）
- `outputs/verification/browser-evidence.json`（本轮真实无头 Chrome 交互证据，重新生成；`all_ok=true`，三速切换 + JSON 导出 + 无云依赖）
- `outputs/verification/vi_run_gates_output.txt`（本轮人类可读执行日志，重新生成；确定性输出，与冻结源字节一致）
- 其余 **71 件** 逐字节复用冻结源 `formal-r3-linkfix-source/outputs`（含已修复的 `app/index.html`，第 132–133 行两处页脚链接指向
  `../verification/acceptance.json`、`../verification/browser-evidence.json`）。
