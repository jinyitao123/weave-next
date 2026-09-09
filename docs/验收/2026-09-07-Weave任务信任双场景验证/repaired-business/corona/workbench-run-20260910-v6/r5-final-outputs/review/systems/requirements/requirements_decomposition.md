# 日冕计划 Pre-Phase A · v6 · 需求分解（systems-architect）

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 **隔离恢复验证**（复用 v4，仅改可复现缺陷；非从零重做）
**基线**：`corona-prephase-a-v1`（frozen，v1.0.0，速度轴 `0.01c / 0.03c / 0.05c`；`default_scenario_c = 0.03`）
**上游（v6 实际可读源）**：
- 权威验收：`/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json`（11 项硬门槛 + 5 项参考锚点 + 6 条 required_final_paths，只读）
- 冻结参数基线：`v4-outputs/model/baseline.yaml`（= 冻结配置基线 `corona-prephase-a-v1` 的自包含副本；与 `complex-validation/baseline.yaml` 逐字一致）
- 程序性来源：`original-materials/materials/systems_engineering_coronal_program.md`、`original-materials/materials/coronal_program_final_decision_report.md`
- 任务负责人简报：`inputs/lead/frozen-input-inventory.md`、`coordination-brief.md`、`aggregated-decisions.md`
**职责**：需求分解（requirements decomposition）——在冻结基线之上产出**系统级 / 架构级需求**，把三路线终态（`../routes/route_terminal_states.md`）落成可核查条目，并为每一条给出**允许的验证方法**（满足硬门槛 `requirements_have_verification_methods`）。

---

## 0. 诚实边界（必须先读）

- 本节点是 **systems-architect**，产出**需求分解**，**不**生成统一模型（属 digital-engineering）、**不**独立复跑十一项硬门槛（属 verification-integrator）、**不**产出 `FINAL_ACCEPTANCE.md`。
- 本文件沿用 v4 `requirements_decomposition.md`（复用优先）；相对 v4 的变化是：**纠正对 v4 上游输入（`inputs/lead/01/03/04/05`、`inputs/lead/baseline/baseline.yaml`、`inputs/lead/requirements/requirements_baseline.md`）的失效引用**——这些 v4 上游并不在 v6 输入集内，故改以 v6 实际可读的冻结源（`v4-outputs/model/baseline.yaml`、`acceptance.json`、`original-materials/materials/*.md`、`inputs/lead/*`）为依据。
- 所有需求编号 `SA-REQ-*` 为本节点架构级分解；与验收硬门槛、接口登记、路线终态、追踪矩阵互映（见 `../verification/traceability_matrix.md`）。

---

## 1. 需求来源与层级

| 层级 | 来源 | 说明 |
|---|---|---|
| 验收硬门槛 | `acceptance.json` `hard_gates`（11 项） | 无论证门槛，P0 硬性 |
| 参考锚点 | `acceptance.json` `reference_checks`（5 项） | 0.03c 容差锚点（CR-001），非主案 |
| 本节点架构级需求 | 本文 `SA-REQ-*` | 把「三路线 × 三速度 × 十一门槛」分解为系统/架构需求 |
| 路线终态 | `../routes/route_terminal_states.md` | A/B/C 的终态/成功定义/退出条件 |
| 接口定义 | `../interfaces/interface_register.md` | IFM-01..IFM-14（provider/consumer/contract） |

---

## 2. 架构级需求（SA-REQ-*）

> 每条需求给**验证方法**（允许值：Analysis / Demonstration / Inspection / Test / Review of design），以满足硬门槛 `requirements_have_verification_methods`（G8）。

### 2.1 通用架构需求（跨路线）

| ID | 需求 | 验证方法 | 对应硬门槛 | 备注 |
|---|---|---|---|---|
| SA-REQ-G-01 | 三速度情景 `0.01c / 0.03c / 0.05c` 作为**并列标准情景**贯穿模型、应用、图纸、论证材料；无单 0.03c 残留 | Inspection（跨产物扫描） | G2 `three_speed_scenarios_present` | 0.03c 仅为参考容差锚点（CR-001），非主案 |
| SA-REQ-G-02 | 所有数值**同源于** `corona-prephase-a-v1`（`baseline.yaml`）；任何成员不得自造常量/速度/航时/功率 | Inspection（参数一致性比对） | G10 `cross_artifact_parameter_consistency_passes` | 引用须写全参数 ID；依据 `v4-outputs/model/baseline.yaml` |
| SA-REQ-G-03 | 每条需求**具备允许的验证方法**，可双向追溯到证据/假设/未知项/验证结果 | Review of design | G8 `requirements_have_verification_methods` | 对齐 `../verification/traceability_matrix.md` |
| SA-REQ-G-04 | 每条关键陈述带真值标签（`verified_fact / derived_result / assumption / unknown`）；缺证据项显式保留 `unknown` | Inspection（内容扫描） | G9 `claims_use_truth_labels` | 真值标签见 `baseline.yaml` `truth_labels` |
| SA-REQ-G-05 | 无未关闭的一级严重问题（S1）；内容层 S1 与过程层 S2/S3 分列记录，不得伪装 | Review of design（不符合项登记册） | G11 `no_open_severity_one_issue` | — |

### 2.2 路线终态需求（对映 `../routes/route_terminal_states.md`）

| ID | 需求 | 验证方法 | 对应硬门槛 | 备注 |
|---|---|---|---|---|
| SA-REQ-A-01 | 路线 A 终态为「近端可交付科学的前驱原型」，速度轴三档均可达近端目标（日球层边界/引力透镜/前驱） | Analysis | G2 | P1 依据（`baseline.yaml` + 决策报告 §5.1） |
| SA-REQ-A-02 | 路线 A 不得以「抵达恒星」为成功定义；不得使用 `construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed` | Inspection | G7 + 事实纪律 | `baseline.yaml` `forbidden_claims` |
| SA-REQ-B-01 | 路线 B 终态可描述为「被动载荷」或「独立近端档案任务」，且不预设唯一口径 | Review of design | G9 | 多代语义需治理/伦理校核（未知项） |
| SA-REQ-B-02 | 路线 B 的「多代语义 / 治理 / 伦理」未决项以 `unknown`+`unresolved` 显式保留，不得假设掩盖 | Inspection | G9 | 录入不符合项/未决项（`route_terminal_states.md` §2.3） |
| SA-REQ-C-01 | 路线 C 终态为「观察 / 边界定位」；百年尺度（85–425 年）下不得承诺载人近端任务时间表 | Analysis | G9 | P2 依据；不可授权载人研发 |
| SA-REQ-C-02 | 路线 C 不因速度轴变化而改变「载人不可承诺」结论；不授权载人研发 | Review of design | G9 | 速度轴强化既有结论 |

### 2.3 接口与可追踪性需求

| ID | 需求 | 验证方法 | 对应硬门槛 |
|---|---|---|---|
| SA-REQ-I-01 | 各专业间接口（参数/运行时/交接）**登记在册**，契约明确（provider/consumer/contract/verification） | Review of design | G10 |
| SA-REQ-I-02 | 生产者（digital-engineering 等）与独立总装员（verification-integrator）**分属不同运行时** | Inspection（运行时归属 + 执行痕迹） | G11（过程层 OP-1） |
| SA-REQ-T-01 | 需求 ↔ 三速度情景 ↔ 验证方法 ↔ 证据 ↔ 硬门槛**双向可追踪** | Review of design | G8 / G9 |

---

## 3. 保留口径（本次只确认，不重定义）

按 `original-materials` 与 v4（复用），以下沿用 v1.0 不再重定义：三路线（A/B/C）、八维评价、TRL 口径（A 系统级约 TRL 2、B 约 TRL 4–6、C 约 TRL 1–2）、WBS（A1–A8/B1–B8/C1–C8）、风险登记 R1–R10（SE 稿）/ RK-01…（cost_risk）、首批 10–14 人、Gate 1（第 6 个月末）、六门 M1–M6、来源编号 E01–E09 / R01–R23。

> 若某专业判断与此类保留口径冲突，按「以专业证据为准，明写冲突点交任务负责人/总装裁决」，**不默默覆盖基线**。

---

## 4. 边界（如实）

1. 本文件**不是**验收结论；十一项硬门槛的独立判定以 verification-integrator 的 `FINAL_ACCEPTANCE.md` 为准。
2. 本文件**不修复应用、不复跑硬门槛**（分别属 digital-engineering / verification-integrator 节点）。
3. 需求变更须走变更控制，不得就地覆盖冻结基线。

---

*由 systems-architect 节点产出；与路线终态、接口登记、追踪矩阵、配置基线注册组成本节点控制包。*
