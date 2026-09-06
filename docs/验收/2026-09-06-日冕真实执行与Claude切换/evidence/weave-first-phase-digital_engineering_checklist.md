# 机器验证清单 · 数字工程构建节点（luna-digital-engineering-builder）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **日期**: 2026-09-06 · **执行者**: luna-digital-engineering-builder（计算内核 / SVG图纸 / Web应用 / 可编辑概念模型）
- **范围**: 本节点交付物 `outputs/model/`、`outputs/drawings/`、`outputs/app/` 的机器可检查项；**不替代**最终总装员的正式验收（RQ-VER-002 要求的总装级实跑与 FINAL_ACCEPTANCE.md 由验证/总装成员完成）
- **纪律**: 全部命令有界退出（无前台开发服务器、无看护进程；最长单条命令为 38 项 unittest，实测 0.046s）；每项保存原始命令输出于 `outputs/verification/logs/`；未执行项如实标 NOT_RUN [verified_fact]

## 1. 结果总表

| 检查ID | 检查项 | 有界命令（工作目录） | 原始结果 | 判定 | 覆盖需求/硬门 |
|---|---|---|---|---|---|
| V-01 | 基线文件 SHA-256 与冻结登记一致 | `shasum -a 256 outputs/model/baseline_frozen.yaml`（workdir） | `logs/v01_baseline_digest.log` | **PASS**（cfb12b78…8b10c2 逐字符一致） | RQ-MDL-001 / cross_artifact_parameter_consistency |
| V-02 | 模型自动测试 | `python3 -m unittest discover -s tests -v`（outputs/model） | `logs/v02_model_unittests.log` | **PASS**（38 项全部 OK，0.046s；曾 1 项 γ 期望值精度 FAIL，修正后复跑通过，见 §3 纠正记录） | RQ-MDL-011 / model_tests_pass |
| V-03 | 模型 CLI 运行 + acceptance 容差比对 | `python3 run_model.py`（outputs/model，退出码 0/1 契约） | `logs/v03_model_run.log` | **PASS**（5/5 参考值在容差内，最大相对偏差 2.579e-8 ≪ 容差） | RQ-MDL-002/003/005/006, RQ-VER-003 / reference_calculations_within_tolerance |
| V-04 | SVG XML 可解析 | `xmllint --noout outputs/drawings/*.svg`（workdir） | `logs/v04_svg_xmllint.log` | **PASS**（3/3 PARSE_OK） | RQ-DRW-001/002/003 / drawings_are_parseable_and_editable（解析部分） |
| V-05 | OpenSCAD 就绪状态 | `which openscad; ls /Applications \| grep -i scad`（workdir） | `logs/v05_openscad_readiness.log` | **NOT_RUN（渲染检查）**：openscad 未安装，DISC-003 如实记录待检查，**不伪造通过** | RQ-DRW-004 / drawings_are_parseable_and_editable（渲染部分移交总装） |
| V-05b | .scad 文本级括号/模块配平（**非渲染**替代检查） | `python3` 配平脚本（workdir，有界） | `logs/v05b_scad_text_check.log` | **PASS（限文本级）**：括号配平 OK、4 个模块定义齐全；明确标注不等价于 OpenSCAD 解析/渲染 | RQ-DRW-004（部分） |
| V-06 | 应用↔模型公式一致性 + 导出 JSON 契约 | `node tests/calc_consistency_check.cjs`（outputs/app，退出码 0/1 契约） | `logs/v06_app_consistency.log` | **PASS**（23/23：5 项参考值复算、8 项与 Python 模型交叉比对 ≤1e-12、导出 JSON 含追溯键/输入/输出/事实标签） | RQ-APP-006 / app_exports_scenario_json（逻辑级） |
| V-07 | 追溯键三元组全产物嵌入 | `python3` 扫描脚本（workdir，有界） | `logs/v07_traceability_scan.log` | **PASS**（24 文件，0 缺失；1 项记录在案豁免 EX-1：`baseline_frozen.yaml` 为摘要计算对象自身，无法内嵌自身摘要，登记值见 inputs/manifest.json 与变更控制登记册 §1；首轮发现 3 个源码文件缺头部追溯键，已修复复跑，见 §3） | RQ-DRW-006, ICD-SOT-003 / cross_artifact_parameter_consistency |
| V-08 | 禁用声明扫描 | `grep -rn -E 'construction_ready\|manufacturing_ready\|flight_certified\|whole_program_cost_committed' outputs/{model,drawings,app}` | `logs/v08_forbidden_claims_scan.log` | **PASS**：全部命中均为纪律语境（基线 forbidden_claims 清单、测试断言、应用"本应用不出现…"否定句、参数 JSON 之清单转录）；无肯定性声明 | RQ-SCP-005 |
| V-09 | 情景泄漏与撤回值扫描 | `grep -rn -E '0\.01c\|0\.05c'` + `grep -rn '450'`（同域） | `logs/v09_scenario_leakage_scan.log` | **PASS**：0.01c/0.05c 全部处于 CCR-001 待确认变更/预留接口语境（RQ-APP-007 允许）；450 命中均为否定语境（"已撤回/不得复现"）或 SVG 坐标值，无撤回值复现 | RQ-SCP-001, RQ-MDL-006, RQ-APP-007 |
| V-10 | 应用静态引用完整性（离线性） | `python3` 引用解析脚本（workdir，有界） | `logs/v10_app_static_refs.log` | **PASS**（8/8 本地引用存在；0 个 http(s) 外部引用，无云服务依赖） | RQ-APP-001 / app_starts_without_cloud_dependency（静态部分） |
| V-11 | 跨产物参数一致性（图纸注记 vs 模型输出） | `python3` 一致性脚本（workdir，有界） | `logs/v11_cross_artifact_consistency.log` | **PASS**（15/15；含概念级标注、追溯键缩写、撤回值精确模式 `450\s*(MJ\|兆焦)`；首轮发现 1 项图纸数值笔误并修复，见 §3） | RQ-DRW-006 / cross_artifact_parameter_consistency |

## 2. NOT_RUN 项（诚实移交最终总装员）

| 项 | 状态 | 原因与移交说明 |
|---|---|---|
| OpenSCAD 解析与渲染 | **NOT_RUN** | 本机未安装 openscad（V-05 实测）；DISC-003 待检查，不得伪造通过。若总装环境具备 OpenSCAD，建议有界命令：`openscad -o /tmp/corona.stl outputs/drawings/corona_concept_model.scad`（加超时约束）。 |
| 应用浏览器端启动与交互 DEMO（调参、打开图纸、点击导出） | **NOT_RUN** | 本节点无浏览器驱动；已完成静态引用检查（V-10）与导出契约逻辑级检查（V-06）。总装员实测时以 `file://` 直接打开 `outputs/app/index.html` 即可（无需服务器；**禁止以前台开发服务器充当完成证据**）。 |
| SVG 视觉渲染检查 | **NOT_RUN** | 已完成 XML 可解析检查（V-04）；视觉渲染由总装员以浏览器/渲染器实测。 |
| 需求验证矩阵（42 条全覆盖）、事实标签全库检查、严重问题清零、FINAL_ACCEPTANCE.md | **NOT_RUN（本节点职责外）** | 属验证/总装成员职责（RQ-VER-001/002/005）；本清单仅覆盖数字工程三类交付物。 |
| 硬门 `three_speed_scenarios_present` | **受阻（不适用）** | DISC-001：与批准的单一 0.03c 锁定冲突，CCR-001 待平台裁定；本节点如实记录，不伪造通过（RQ-VER-004）。DISC-002 同因：应用仅实现 0.03c，三情景为预留接口。 |

## 3. 不符合项与纠正记录（本轮真实发生）

| 编号 | 发现（检查ID） | 纠正 | 复跑结果 |
|---|---|---|---|
| NC-01 | `test_lorentz_gamma_bounds` FAIL：硬编码 γ(0.03) 期望值精度不足（V-02 首轮） | 断言改为按定义式 `1/sqrt(1-β²)` 现算并放宽到 places=14 | V-02 复跑 38/38 OK |
| NC-02 | V-07 首轮：`baseline.py`/`physics.py`/`computations.py` 缺头部追溯键（3 文件） | 补齐追溯键注释；`baseline_frozen.yaml` 记录为豁免 EX-1（摘要对象自身） | V-07 复跑 24 文件 0 缺失 |
| NC-03 | V-11 首轮：COR-SVG-001 路线 A 动能注记笔误 4.04×10¹³ J（模型值 4.0444e10 J） | 修正为 4.04×10¹⁰ J 并扩展 V-11 覆盖三路线代表质量注记 | V-11 复跑 15/15 CONSISTENT |
| NC-04 | V-11 次轮：撤回值检查误报（子串 `450` 命中 SVG 坐标） | 检查模式精确化为 `450\s*(MJ\|兆焦)` | V-11 复跑通过，无真实撤回值复现 |

以上 4 项均为本轮真实工具调用发现并修复，严重程度均为低（sev-3 以下），当前无未关闭严重问题（本节点范围内）[verified_fact]。

## 4. 验收准则差异披露（移交 FINAL_ACCEPTANCE.md，不得掩盖）

- **DISC-001** [verified_fact]：acceptance.json 硬门 `three_speed_scenarios_present` 与本轮批准的单一 0.03c 锁定直接冲突；CCR-001 获平台确认前该硬门按字面无法通过。本节点不擅自改写验收准则、不擅自扩展基线。
- **DISC-002** [verified_fact]：任务书 §4.3 三情景应用要求受 CCR-001 约束；应用仅实现 0.03c 并预留接口（RQ-APP-007）。
- **DISC-003** [unknown→已查]：OpenSCAD 本机未安装（V-05 实测），渲染检查待具备环境时执行。

## 5. 给最终总装员的接力说明

1. 本节点交付物参数全部派生自 `outputs/model/baseline_frozen.yaml`（digest 已实测一致，V-01）。
2. 复跑入口：`cd outputs/model && python3 -m unittest discover -s tests -v && python3 run_model.py`；`cd outputs/app && node tests/calc_consistency_check.cjs`。两条链均为有界退出，失败时退出码非 0。
3. 应用离线打开方式：`file://` 直接打开 `outputs/app/index.html`；参数经 `data/baseline_params.js` 以 `<script>` 加载，不依赖 fetch/服务器/云。
4. 本节点未读取、未使用任何旧运行文件；上游输入仅来自 `inputs/`（平台接力）与四个指定真实源文件 [verified_fact]。
