# FINAL_ACCEPTANCE · 日冕计划 Level-0 / Pre-Phase A 先期论证 · 最终验收判定

节点：luna-verification-integrator（对抗审查 / 最终总装 / 独立复算 / 跨产物一致性）
日期：2026-09-06 · 基线：`baseline_v1.1.0`（corona-prephase-a-v1.1.0，CR-001 三速度情景已冻结）

---

## 0. 最终判定

**专业成果验收：通过（ACCEPTED）。**
acceptance.json 全部 11 项硬门实际执行并通过；49 条需求验证矩阵全覆盖；
五参考算例独立复算在容差内；无未关闭 severity-1 不符合项；无 NOT RUN 项。

过程验收（Weave 平台层，任务书 §6.1）属平台职责，本文件不代判；
本节点过程纪律自查（REQ-X-005/006）见 §6。

事实标签：本文件判定与执行为 [verified_fact | 本轮真实工具调用]；
引用的技术数值为 [derived_result | 来源 baseline_v1.1.0]；边界声明见 §7。

---

## 1. 来源核验（本轮真实执行）

- 平台传入 `inputs/` 清单 **78 个文件逐一 sha256 比对 `inputs/manifest.json`：78/78 一致**
  （python3 hashlib 实算，bad count=0）。
- 四个真实输入文件实际读取：任务纪要修订稿、complex-validation/baseline.yaml、
  验收任务书、acceptance.json（Read 工具返回全文）。
- 未扫描同机其他成员工作目录；旧失败运行未重试、未覆盖、未引用其文件。
- 下游接力：选定当前版本已复制整合进本节点 `outputs/`（基线 10 件来自 parallel-06；
  model/drawings/app/验证工具链来自 parallel-03；physics 来自 parallel-04；
  review 来自 parallel-02+parallel-05；archive_payload 来自 parallel-01），
  关键基线文件复制后复验哈希不变（baseline_v1.1.0.yaml = `592569b7…`，
  v1.0.0 历史快照 = `b908c4a0…`）。
- 唯一当前基线：`outputs/baseline/baseline_v1.1.0.yaml`；v1.0.0 仅存为
  `baseline_v1.0.0_superseded.yaml` 历史证据，当前产物零引用（机器扫描确认）。

## 2. 必交类型齐全性（acceptance.json required_final_paths）

| 必需路径 | 状态 | 内容要点 |
|---|---|---|
| `outputs/model/` | ✅ | corona_model.py（ICD §3 七式）、baseline 副本（哈希一致）、params.json、30 项测试 |
| `outputs/drawings/` | ✅ | COR-DWG-001/002/003 SVG + 004 SCAD + DXF 备选 + OpenSCAD 实测渲染 PNG |
| `outputs/app/` | ✅ | 离线静态应用（index/app/calc/params/styles）+ 机器检查 19 项 + 页面交互检查 18 项 |
| `outputs/review/` | ✅ | TRL/风险/成本方法/WBS/终止条件/课题包/推进能源热控比较 + 总装索引 |
| `outputs/verification/` | ✅ | 49 条验证矩阵、不符合项登记、全部原始结果日志、可复跑总控脚本 |
| `outputs/FINAL_ACCEPTANCE.md` | ✅ | 本文件 |

## 3. 十一项硬门逐项判定（全部本节点实际执行，原始结果在 outputs/verification/results/）

| # | 硬门 | 执行证据（有界命令 → 原始结果） | 判定 |
|---|---|---|---|
| 1 | model_tests_pass | `python3 -m unittest discover -s tests -v`（120s）→ model_tests.log：`Ran 30 tests … OK` | **PASS** |
| 2 | three_speed_scenarios_present | 模型测试情景断言 + app_check 三情景 + 图纸注记扫描 + 文档抽查 → 三处均 [0.01, 0.03, 0.05] | **PASS** |
| 3 | reference_calculations_within_tolerance | `independent_acceptance_recalc.py`（不 import 模型代码）→ 5/5 PASS（worst rel_err 2.6e-8）；模型测试与 recheck_physics.py 双重复核 | **PASS** |
| 4 | app_starts_without_cloud_dependency | `node --check` 三文件 SYNTAX-OK；app_check §6 运行时资源零外部 URL；纯静态无云架构 | **PASS** |
| 5 | app_exports_scenario_json | app_check §5 + app_interaction §4：导出含 baseline_version "1.1.0"、三情景完整参数、open_unknowns(7)、conceptual_boundary | **PASS** |
| 6 | drawings_are_parseable_and_editable | ElementTree 解析 3 SVG 全 XML-OK；OpenSCAD 2026.09.03 实测渲染成功（42305 B PNG，目视几何正确）；DXF R12 结构断言 OK | **PASS** |
| 7 | all_drawings_marked_conceptual | consistency_check §3：3 SVG + SCAD 全项命中（CONCEPT ONLY / 非施工非制造非认证 / 图号版本 / baseline_version: 1.1.0 / 三情景 / 四标签） | **PASS** |
| 8 | requirements_have_verification_methods | 机器实计：requirements_baseline.md 49/49 条均含 A/T/I/D 验证方法 | **PASS** |
| 9 | claims_use_truth_labels | consistency_check §6 关键产物 4/4 标签 + 全文档抽查（E-SWEEP-F） | **PASS** |
| 10 | cross_artifact_parameter_consistency_passes | consistency_check.py 37 项全 PASS：sha256 溯源链 + ICD §2 键名三方一致（YAML↔params.json↔params.js）+ 无 v1.0.0 残留 | **PASS** |
| 11 | no_open_severity_one_issue | ICD §6 七条逐项对抗扫描 + NC-01~05 全部关闭复测（nonconformity_log.md） | **PASS** |

总控复跑：`bash outputs/verification/run_checks.sh`（10 步，全部经 bounded_run.py 硬超时，
连续两次 `RUN_CHECKS | ALL OK`，exit=0）。未使用任何前台开发服务器充当证据。

## 4. 独立复算摘要（reference_checks，本节点直算）

| 参考算例 | 复算值 | 期望值 | rel_err | 容差 | 判定 |
|---|---|---|---|---|---|
| travel_years_at_0_03c | 141.66666666666669 | 141.6666667 | 2.4e-10 | 1e-3 | PASS |
| kinetic_energy_1mt_at_0_03c_j | 4.04439830431568e22 | 4.0443983e22 | 1.1e-9 | 1e-2 | PASS |
| kinetic_energy_5mt_at_0_03c_j | 2.02219915215784e23 | 2.0221991e23 | 2.6e-8 | 1e-2 | PASS |
| gravity_1km_2rpm_m_s2 | 43.864908449286034 | 43.8649 | 1.9e-7 | 1e-2 | PASS |
| dust_1mg_0_03c_j | 40443983.043156795 | 40443983 | 1.1e-9 | 1e-2 | PASS |

三情景补充复算 [derived_result]：t = 425 / 141.667 / 85 yr；KE(1 Mt) = 4.494e21 / 4.044e22 / 1.123e23 J；
dust(1 mg) = 4.494e6 / 4.044e7 / 1.123e8 J。与基线派生表交叉比对 worst rel_err 4.9e-11（E-PHYS）。

## 5. 需求验证矩阵结论

49/49 条全部验证通过（含上游登记的"34 条"计数笔误勘误 ERR-01，实计 49 条），
双向可追踪，无 NOT RUN、无替代检查。详见
`outputs/verification/requirements_verification_matrix.md`。

## 6. 不符合项与对抗审查结论

- NC-01/02：两处禁用词表措辞命中（语义为禁用声明），已按检查器口径修复并复测通过；
- NC-03/04/05：总装环境三类问题（只读副本、render 目录缺失、脚本内步骤顺序），已修复并两次完整复跑确认；
- ERR-01：REQ prose 计数 34→49 勘误，移交下一 CR 更正基线文件；
- ICD §6 七条禁止条款逐项扫描无违规；已撤回口径（2.4/3.4 万亿投资、1 mg≈450 MJ 尘埃值）零命中；
- **独立否定复核终判：无开放的 severity-1 不符合项**（nonconformity_log.md §4）。

## 7. 边界与开放项声明（随判定保留）

- 概念级边界：本轮全部图纸与应用标注 CONCEPT ONLY / 非施工、非制造、非认证；
  ICD §6.4 四类禁用声称全程零违规使用。
- 基线七项未知项（尘埃通量、推进效率、盾体参数、封闭生态放大、冬眠成熟度、经费证据、
  总成本）全部保持 unknown 开放，未以假设静默填补；档案质量、任务终态选定等同款开放。
- 动能下限（10²²–10²³ J 量级）仅为下限，不等于完整推进能源预算（REQ-X-003）。
- 三路线可分离：克级光帆结论未外推至大型载荷/载人路线（REQ-M-003）。
- OpenSCAD 本机实测可用（2026.09.03），REQ-D-004 为实测通过而非"待检查"；DXF 备选同附。
- 页面交互检查以最小 DOM 桩驱动真实 app.js 事件接线（未模拟浏览器排版渲染），边界已在
  `outputs/app/tests/app_interaction_check.js` 头部声明。

## 8. 阻塞申报

本节点无阻塞。全部硬门实际执行完毕并留痕；无依赖外部权限或资源的未决项。
