# 日冕计划 Pre-Phase A · v6 · 追踪矩阵（systems-architect）

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 **隔离恢复验证**（复用 v4，仅改可复现缺陷）
**基线**：`corona-prephase-a-v1`（frozen，v1.0.0，`0.01c / 0.03c / 0.05c`）
**职责**：追踪矩阵（traceability matrix）——实现硬门槛 `requirements_have_verification_methods`（G8）/ `claims_use_truth_labels`（G9）所需的**双向可追溯**（需求 ↔ 三情景 ↔ 验证方法 ↔ 证据 ↔ 硬门槛 ↔ 责任节点）。

> 相对 v4 修正可复现缺陷：**证据路径脱链**。v4 的 `evidence_path` 列写作 `outputs/requirements/…`、`outputs/config/…`、`outputs/routes/…`、`outputs/interfaces/…`，而在交付布局里这些控制件实际位于 `outputs/review/systems/…`（`required_final_paths` 只含 `outputs/review/`，无 `outputs/requirements/` 等顶层路径）。本 v6 版将 `evidence_path` 全部校正为**实际交付可达路径** `outputs/review/systems/…`，并移除对失效 v4 上游输入（`inputs/lead/baseline/…`、`inputs/lead/requirements/…`）的引用。

---

## 0. 诚实边界

- 本矩阵由 systems-architect 编制，**不是**验收结论；十一项硬门槛的独立判定以 verification-integrator 的 `FINAL_ACCEPTANCE.md` 为准。
- 本矩阵**不**生产应用/模型/图纸；各条目的「验证方法」多为 `Review of design` / `Inspection`（架构层），内容/过程层的重跑属对应执行节点。
- 对**未执行**条目（尤其数字工程应用修复、verification-integrator 独立复跑），本矩阵如实标 `未执行`，**不标为通过**。

---

## 1. 矩阵（机器可读版）

- **CSV**：`traceability_matrix.csv`（列：`req_id, requirement, category, verification_method, hard_gate, scenarios_covered, evidence_path, verification_status, responsible_node`）。
- 下表为 CSV 的可读摘要（关键列；`evidence_path` 已校正为 `outputs/review/systems/…`）。为版面紧凑，下表「证据路径」列统一省略交付根前缀 `outputs/`（如 `review/systems/requirements/…` ≡ `outputs/review/systems/requirements/…`）；**完整、无歧义的路径见 CSV `traceability_matrix.csv` 的 `evidence_path` 列**。

| req_id | 需求 | 验证方法 | 硬门槛 | 覆盖情景 | 证据路径 | 状态 | 责任节点 |
|---|---|---|---|---|---|---|---|
| SA-REQ-G-01 | 三速度贯穿、无单 0.03c 残留 | Inspection | gate 2 | 0.01/0.03/0.05 | `review/systems/requirements/requirements_decomposition.md` §2.1 | 待独立复跑 | verification-integrator |
| SA-REQ-G-02 | 数值同源于 corona-prephase-a-v1 | Inspection | gate 10 | 0.01/0.03/0.05 | `review/systems/config/config_items.yaml` CI-01..CI-08 | 本节点一致性核对通过（转发） | systems-architect |
| SA-REQ-G-03 | 需求含验证方法、双向追踪 | Review of design | gate 8 | 0.01/0.03/0.05 | `review/systems/verification/traceability_matrix.csv` | 本节点已编制 | systems-architect |
| SA-REQ-G-04 | 关键陈述带真值标签 | Inspection | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §1 | 本节点已标注，待独立复跑 | verification-integrator |
| SA-REQ-G-05 | 无未关闭 S1；S2/S3 分列 | Review of design | gate 11 | 0.01/0.03/0.05 | `review/systems/interfaces/interface_register.md` IFM-12/13/14 | 待独立复跑 | verification-integrator |
| SA-REQ-A-01 | A 终态=近端前驱原型 | Analysis | gate 2 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.2 | 本节点定义，待独立复跑 | verification-integrator |
| SA-REQ-A-02 | A 不以抵达恒星为成功；禁用施工/制造/认证/总预算断言 | Inspection | gate 7 + 事实纪律 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.2 + `baseline.yaml` `forbidden_claims` | 本节点定义，待独立复跑 | verification-integrator |
| SA-REQ-B-01 | B 终态可描述（被动载荷/独立档案任务） | Review of design | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.3 | 本节点定义，多代语义保留 unknown | systems-architect |
| SA-REQ-B-02 | B 多代/治理/伦理未决项显式保留 | Inspection | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.3 | 本节点保留（未决） | systems-architect |
| SA-REQ-C-01 | C 终态=观察/边界；百年不可承诺 | Analysis | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.4 | 本节点定义，待独立复跑 | verification-integrator |
| SA-REQ-C-02 | C 不改「载人不可承诺」；不授权载人研发 | Review of design | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` §2.4 | 本节点定义 | systems-architect |
| SA-REQ-I-01 | 接口登记在册、契约明确 | Review of design | gate 10 | 0.01/0.03/0.05 | `review/systems/interfaces/interface_register.md` | 本节点已登记 | systems-architect |
| SA-REQ-I-02 | 生产者/总装分属不同运行时 | Inspection | gate 11（过程层 OP-1） | — | `review/systems/interfaces/interface_register.md` IFM-13 | 依赖平台编排真实参与 | Weave |
| SA-REQ-T-01 | 需求↔情景↔方法↔证据↔门槛双向 | Review of design | gate 8/9 | 0.01/0.03/0.05 | `review/systems/verification/traceability_matrix.csv` | 本节点已编制 | systems-architect |
| REQ-V4-C-01 | 统一模型+参数+测试 | Test | gate 1 | 0.01/0.03/0.05 | `model/`（`corona_model.py`,`params.json`,`tests/test_model.py`） | 待独立复跑 | digital-engineering / verification-integrator |
| REQ-V4-C-03 | 五项 0.03c 锚点在容差内 | Test | gate 3 | 0.03 | `model/anchors_reverified_2026-09-09.txt` + `acceptance.json` reference_checks | 本节点一致性核对 5/5 OK；独立复跑待办 | verification-integrator |
| REQ-V4-C-04 | 应用本地可运行、无云依赖 | Demonstration | gate 4 | 0.01/0.03/0.05 | `app/`（待数字工程修复） | **未执行** | digital-engineering |
| REQ-V4-C-05 | 三速度切换实时更新 | Demonstration | gate 5 | 0.01/0.03/0.05 | `app/`（待数字工程修复） | **未执行** | digital-engineering |
| REQ-V4-C-06 | 导出当前情景 JSON | Demonstration | gate 5 | 0.01/0.03/0.05 | `app/`（待数字工程修复） | **未执行** | digital-engineering |
| REQ-V4-C-07 | ≥3 SVG + 1 OpenSCAD 可解析可编辑 | Inspection | gate 6 | 0.01/0.03/0.05 | `drawings/`（待数字工程产物） | **未生成**（本节点不产出图纸） | digital-engineering |
| REQ-V4-C-08 | 全部图纸标注概念级 | Inspection | gate 7 | 0.01/0.03/0.05 | `drawings/`（待数字工程产物） | **未生成** | digital-engineering |
| REQ-V4-C-09 | 需求含验证方法、双向追踪 | Review of design | gate 8 | 0.01/0.03/0.05 | `review/systems/verification/traceability_matrix.csv` | 本节点已编制 | systems-architect |
| REQ-V4-C-10 | 关键陈述带真值标签 | Inspection | gate 9 | 0.01/0.03/0.05 | `review/systems/routes/route_terminal_states.md` | 本节点已标注，待独立复跑 | verification-integrator |
| REQ-V4-C-11 | 无未关闭一级严重问题 | Review of design | gate 11 | 0.01/0.03/0.05 | `review/systems/interfaces/interface_register.md` IFM-12..14 | 待独立复跑 | verification-integrator |
| REQ-V4-P-01 | 至少两个外部运行时真实参与 | Inspection | gate 11（过程层） | — | `review/systems/interfaces/interface_register.md` IFM-13 | 依赖平台编排真实参与 | Weave |
| REQ-V4-P-02 | 真独立总装员：不复用成员自称结论 | Demonstration | gate 1–11 | — | `outputs/FINAL_ACCEPTANCE.md`（待复跑） | **未执行** | verification-integrator |
| REQ-V4-P-03 | 11 项逐项复跑、未过即 FAIL | Demonstration | gate 1–11 | — | `outputs/FINAL_ACCEPTANCE.md` + `outputs/verification/acceptance.json` | **未执行** | verification-integrator |
| REQ-V4-P-04 | 唯一最终目录、六路径存在 | Inspection | gate 4–11（过程层 OP-2） | — | `outputs/model|drawings|app|review|verification|FINAL_ACCEPTANCE.md` | **未生成** | verification-integrator |
| REQ-V4-P-05 | 全部成果来自同一参数模型 | Inspection | gate 10 | 0.01/0.03/0.05 | `review/systems/config/config_items.yaml` CI-01..CI-08 | 本节点一致性核对（转发） | systems-architect |
| REQ-V4-P-06 | 先修好三处应用功能再复跑 | Demonstration | gate 4/5/10 | 0.01/0.03/0.05 | `app/`（待数字工程） | **未执行** | digital-engineering |
| REQ-V4-P-07 | 保留既有材料与三速度，不推新基线 | Inspection | gate 10 | 0.01/0.03/0.05 | `review/systems/config/config_baseline_register.md` | 本节点确认沿用 | systems-architect |

---

## 2. 反向映射（按硬门槛检索）

| 硬门槛 | 关键需求/追踪项 |
|---|---|
| gate 1 model_tests_pass | REQ-V4-C-01 |
| gate 2 three_speed_scenarios_present | SA-REQ-G-01、SA-REQ-A-01、IFM-04 派生量表 |
| gate 3 reference_calculations_within_tolerance | REQ-V4-C-03 |
| gate 4 app_starts_without_cloud_dependency | REQ-V4-C-04、IFM-08 |
| gate 5 app_exports_scenario_json | REQ-V4-C-05、REQ-V4-C-06 |
| gate 6 drawings_are_parseable_and_editable | REQ-V4-C-07、IFM-09 |
| gate 7 all_drawings_marked_conceptual | REQ-V4-C-08、SA-REQ-A-02（事实纪律） |
| gate 8 requirements_have_verification_methods | SA-REQ-G-03、REQ-V4-C-09、SA-REQ-T-01 |
| gate 9 claims_use_truth_labels | SA-REQ-G-04、SA-REQ-B-01/B-02、SA-REQ-C-01/C-02、REQ-V4-C-10 |
| gate 10 cross_artifact_parameter_consistency_passes | SA-REQ-G-02、SA-REQ-I-01、REQ-V4-P-05、IFM-01/02/03/08 |
| gate 11 no_open_severity_one_issue | SA-REQ-G-05、SA-REQ-I-02、REQ-V4-P-01..P-04 |

---

## 3. 未决/待执行/数据缺口（如实）

本矩阵对以下为**「未执行」或「未决」**状态，**非结论**：

- **数据缺口（本节点不能重建）**：v4 追踪矩阵未含 `REQ-V4-C-02`。由于 v6 输入集不含 v4 上游 `inputs/lead/requirements/requirements_baseline.md`，本节点**无法重建该条内容**，故不虚构、不补全，列为数据缺口交 verification-integrator / 任务负责人对账。这**不影响 G8**，因为 `SA-REQ-*` 架构级需求已逐条给出验证方法并双向可追溯。
- **数字工程（CR-002 类）**：`REQ-V4-C-04..C-06`（应用功能）为 **未执行**；`C-07/C-08`（图纸）未生成。执行属 digital-engineering。
- **独立复跑（CR-003 类）**：十一项硬门槛逐项复跑 + 唯一最终目录 + `FINAL_ACCEPTANCE.md` 均为 **未执行**，属 verification-integrator。本节点**仅**提供架构层需求/接口/终态/追踪/配置核验，不承担复跑。
- **路线 B「多代语义 / 治理 / 伦理校核」**：未决，标 `unknown`，交 Gate 1 前评审（见 `../routes/route_terminal_states.md` §2.3）。

---

*由 systems-architect 节点产出；本矩阵与需求分解、接口登记、路线终态、配置基线注册共同构成节点控制包。*
