# 日冕计划 Pre-Phase A · v6 · 接口登记（systems-architect）

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 **隔离恢复验证**（复用 v4，仅改可复现缺陷）
**基线**：`corona-prephase-a-v1`（frozen，v1.0.0，`0.01c / 0.03c / 0.05c`）
**职责**：接口登记（interface registration）——在验收硬门槛与冻结参数基线之下，建立**可登记、可追踪的接口目录**（IFM-*），每条含 provider / consumer / contract / verification / gate。

> 本登记沿用 v4 `interface_register.md`（复用优先）。相对 v4 修正两点可复现缺陷：
> ① **移除对失效 v4 上游输入的引用**（`inputs/lead/03`、`inputs/lead/04`）——v6 输入集不含这些文件，改以 `acceptance.json`、`v4-outputs/model/baseline.yaml`、`inputs/lead/*` 为依据；
> ② **不虚构 v6 运行时 UUID**——v4 硬编码了 `41233bee-…`/`84da6261-…`，但 v6 的运行时归属由平台编排，本节点**不代为填写**，仅在 IFM-13 登记「生产侧与独立总装侧必须分属不同运行时」这一过程约束。

---

## 0. 诚实边界

- 本节点是 **systems-architect**，产出**接口登记**（目录与契约），**不**定义新参数、**不**生产应用、**不**执行十一项复跑。
- 接口的**实际执行与产物归集**由平台编排；本登记作为仲裁依据，不改写验证框架，而是将其落地为逐条可追踪目录。

---

## 1. 接口目录（IFM-*）

| 接口 ID | 名称 | 类型 | Provider | Consumer | 契约（contract） | 验证方式 | 对应硬门槛 |
|---|---|---|---|---|---|---|---|
| IFM-01 | 冻结参数源（单一事实源） | 参数数据 | `v4-outputs/model/baseline.yaml`（= 冻结配置基线 `corona-prephase-a-v1`） | 各专业/模型/应用 | `baseline_id=corona-prephase-a-v1`、`baseline_version=1.0.0`、`cruise_speed_c=[0.01,0.03,0.05]`、`default_scenario_c=0.03`、常量、参考案例**逐字一致** | Inspection（机器比对） | G10 |
| IFM-02 | 参数模型机器副本 | 参数数据 | `model/params.json` | 模型/应用/总装 | 与 `baseline.yaml` 一致；与 `app/params.js` 同 ID 同值 | Inspection（机器比对） | G10 |
| IFM-03 | 应用侧参数副本 | 参数数据 | `app/params.js` | 数字应用 | 与 `params.json` 同 ID 同值 | Inspection（机器比对） | G10 |
| IFM-04 | 派生量表共享 | 派生数据 | `route_terminal_states.md` §2.1 / 参数字典 | 各专业 | `V_KMPS(CS)` / `TIME_ALPHA(CS)` / `TIME_GLENS(CS)` / `TIME_PRECURSOR(CS)` / `P_REL(CS)` / `E_SAIL_1MT(CS)`；引用写全参数 ID，不得只写「0.01c」 | Review of design | G2 / G9 |
| IFM-05 | 事实纪律（禁止项） | 约束 | `v4-outputs/model/baseline.yaml` `forbidden_claims` | 各专业 | 不使用 `construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed`；缺证据保留 `unknown` | Inspection（扫描） | G7 / G9 |
| IFM-06 | 真值标签 | 约束 | `v4-outputs/model/baseline.yaml` `truth_labels` | 各专业/总装 | 关键陈述带 `verified_fact / derived_result / assumption / unknown` | Inspection（内容扫描） | G9 |
| IFM-07 | 生产者→复验者交接（模型） | 数据交接 | digital-engineering-builder | verification-integrator | `model/corona_model.py`、`model/params.json`、`model/tests/test_model.py` | Review of design + 重跑 | G1 |
| IFM-08 | 生产者→复验者交接（应用） | 数据交接 | digital-engineering-builder | verification-integrator | `app/index.html`、`app/model.js`、`app/app.js`、`app/params.js` + 机器验证清单 | 真实 GUI + 校验 | G4 / G5 / G10 |
| IFM-09 | 生产者→复验者交接（图纸） | 数据交接 | digital-engineering-builder / archive | verification-integrator | `drawings/*.svg`、`drawings/*.scad`；可解析、可编辑、标注概念级 | XML 解析 + 结构校验 | G6 / G7 |
| IFM-10 | 论证材料交接 | 数据交接 | 各专业 | verification-integrator | `review/*.md`（TRL/成本/风险/WBS/课题包/独立否定意见） | Review of design | G9 |
| IFM-11 | 需求/接口/追踪矩阵交接 | 数据交接 | systems-architect（本节点） | verification-integrator | `review/systems/requirements/`、`interfaces/`、`verification/traceability_matrix.md` | Review of design | G8 |
| IFM-12 | 验收硬门槛定义 | 数据 | `acceptance.json`（只读） | verification-integrator | 11 项硬门槛 + 5 项参考检查 + 6 条 required_final_paths | Review of design | G1–G11 |
| IFM-13 | 运行时归属（独立总装） | 过程约束 | 平台编排 | verification-integrator | **生产侧与独立总装侧必须分属不同外部运行时**，均产生真实执行痕迹；v6 具体 UUID 由平台编排，本节点不代为填写 | Inspection（运行时核对） | G11（过程层 OP-1/OP-2） |
| IFM-14 | 唯一最终结果目录 | 过程约束 | verification-integrator | 平台 / 用户 | `outputs/model|drawings|app|review|verification|FINAL_ACCEPTANCE.md` 六路径全部存在，且为唯一当前版本 | Inspection（文件存在性核对） | G4–G11（过程层 OP-2） |

---

## 2. 接口契约规则（登记为硬性）

1. **参数同源**：任何专业的速度、航时、功率、能量必须写全参数 ID，禁止内部再定义；唯一参数源为 `corona-prephase-a-v1`（`v4-outputs/model/baseline.yaml`）与 `acceptance.json`。
2. **冲突处理优先级**：专业判断与冻结基线结论冲突 → 以专业证据为准，写明冲突点，交任务负责人/总装裁决；不覆盖基线。
3. **最近端目标口径**：若系统/制度专业对「引力透镜/前驱/日球层」作为近端目标有分歧，标 `不确定性` 交 Gate 1 前评审，不得各自采用不同口径。
4. **独立复跑前置**：数字工程应用修复须先完成且经真实 GUI 交互验证，verification-integrator 方可复跑。
5. **只读边界**：`acceptance.json`、`original-materials/`、`v4-outputs/` 均为只读；本节点只写自身 `outputs/`。

---

## 3. 接口 ↔ 需求 ↔ 硬门槛 映射

- 接口目录与 `../requirements/requirements_decomposition.md` 的 `SA-REQ-I-*`、`SA-REQ-T-*` 互映。
- 完整双向往返见 `../verification/traceability_matrix.md`。

---

## 4. 边界（如实）

1. 本登记**未执行**任何接口的实际调用（应用修复、11 项复跑），接口执行与归集由平台编排。
2. 本登记**不新增**参数/公式；一切数值以冻结基线为准。
3. v6 运行时 UUID、团队 UUID 由平台编排，本节点**不虚构、不代为给出**；过程层以实际执行痕迹为准。

---

*由 systems-architect 节点产出；与本节点其余控制件（需求分解、路线终态、追踪矩阵、配置基线注册）构成完整接口与架构包。*
