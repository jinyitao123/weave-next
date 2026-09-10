# 日冕计划 · Pre-Phase A · v6 复验 · 成本·风险·技术成熟度域 — 交付索引

**节点**：`cost-risk-analyst`（成本计划与风险分析员）
**团队**：`corona-prephase-a-recovery-v6`
**基线**：`corona-prephase-a-v1`（冻结，v1.0.0，速度轴 0.01c/0.03c/0.05c，status=frozen_for_validation）
**节点契约**：**复核并复用冻结 v4 的技术成熟度、WBS、成本研究方法、风险和课题包**（复用优先，仅改可复现缺陷）
**日期**：2026-09-10
**上游**：用户复验指令（run_input，`corona-prephase-a-recovery-v6`）；mission-lead 冻结输入包 `inputs/lead/`（`frozen-input-inventory.md`、`coordination-brief.md`、`aggregated-decisions.md`）；只读基线 `recovery-materials/2026-09-10-corona-v5-source/`。

---

## 0. 本节点诚实定位

本节点**只**完成 `cost-risk-analyst` 分支域内分析：技术成熟度（TRL/IRL）、工作分解结构（WBS）、成本研究方法、风险登记册、终止条件、下一阶段课题包，并对以上 v4 成果做**复核后复用**。

本节点**不**：
- 生成 `FINAL_ACCEPTANCE.md`，**不**作任何「验收通过/不通过」判定（属 verification-integrator）。
- 代做物理模型（physics）、图纸/应用（digital-engineering）、需求接口（systems）、推进/能源/热控（propulsion）、档案载荷（archive）。
- 修复数字应用页面计算/情景切换/导出（CR-002，属 digital-engineering，本节点只做风险登记）。
- **独立复跑全部十一项硬门槛**（属 verification-integrator，见 `coordination-brief.md` §2/§3；本节点只是该硬门槛相关专业材料的提供方与支撑方）。

最终是否 `overall: PASS` 由独立验证与总装员在真实复跑原 `acceptance.json` 后判定；本节点**未运行、未见**该复跑结果，故**不**作任何整体验收声明。

---

## 1. 复核结论：v4 成本/风险/TRL/WBS/课题包域无领域内缺陷，可整体复用

| # | 复核项 | v6 本节点独立检查（真实工具调用） | 结果 |
|---|---|---|---|
| C-1 | 5 个参考锚点（acceptance.json `reference_checks`）独立复算 | python3 重算：travel_years_at_0_03c / ke_1mt / ke_5mt / gravity_1km_2rpm / dust_1mg，逐项与 expected 比对 | **5/5 在容差内**；worst rel_err = **1.926e-07**（gravity 1km 2rpm）；见 `verification/cost_risk_recomputed_2026-09-10.txt` |
| C-2 | 三速度派生量独立复算（航时/动能/尘埃/人工重力/转向上限/相对论修正/成本敏感度比值） | python3 重算三档（0.01c/0.03c/0.05c） | 与 v4 evidence 及冻结基线**逐项一致**；0.05c/0.01c 动能 = 25.0000、0.03c/0.01c = 9.0000（v² 标度）；相对论修正 <0.2%（0.0075%/0.0676%/0.1879%） |
| C-3 | `acceptance.json` 权威文件校验 | 直接读取该路径，SHA-256 = `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4` | 与任务书/mission-lead 给定值**一致** |
| C-4 | 禁止声称扫描（construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed） | 全量 grep：出现处均为**否定/定义/禁语表**语境，无肯定式声称 | **无违规** |
| C-5 | 单情景（0.03c 唯一算例）残留扫描 | 全量 grep「唯一算例/唯一情景/仅算 0.03」等：仅出现在「无……残留」「已避免」否定语境 | **无单情景残留** |
| C-6 | 跨路线不可外推（克级 ≠ 大型无人载荷 ≠ 载人飞船） | 全量 grep「外推为/等同于/等价于可行」：仅否定/边界语境 | **无不当外推** |
| C-7 | 三情景贯穿（G2）真值标签覆盖（G9） | 每域文件统计 0.01c/0.03c/0.05c 出现与 `verified_fact/derived_result/assumption/unknown` 出现 | **7 份文件 + 证据文件均三情景并列出、均含真值标签** |
| C-8 | 领域内 S1（RK-02/03/04/05/06）处置 | 逐条核对：全部标「已避免」，无开放 | **本域无开放 S1** |

> **结论**：v4 `cost_risk` 域成果在数值、三情景口径、禁语、真值标签、跨路线边界、S1 处置上**均无领域内缺陷**，**整体复用**。除下述两处**证据路径引用缺陷**外，专业内容未改动。

---

## 2. 本域发现并修复的可复现缺陷（v6 变更）

| # | 缺陷 | 证据 | v6 处置 |
|---|---|---|---|
| F-1 | **证据文件相对路径引用错误**：`techmaturity_assessment.md` 与 `cost_research_method.md` 引用 `../verification/cost_risk_recomputed_2026-09-09.txt`；该路径从 `review/cost_risk/` 解析为 `review/verification/`（**不存在**）。真实证据在 `review/cost_risk/verification/` | `ls -d ../verification` → **No such file or directory**；证据实际位于 `verification/cost_risk_recomputed_2026-09-09.txt` | 修正为 `verification/cost_risk_recomputed_2026-09-10.txt`（v6 证据，位于同目录 `verification/`），消除断链 |
| F-2 | **v4 证据文件名日期**与 v6 复验不符（属证据/版次） | v4 `cost_risk_recomputed_2026-09-09.txt` | 以 v6 复验另生成 `verification/cost_risk_recomputed_2026-09-10.txt`，v4 证据保留作来源比对 |

> 说明：F-1 属于可复现的断链缺陷（路径指向不存在的 `review/verification/`），在本域范围内予以修复；F-2 为证据版次补充。两处都只改**引用/证据**，不改任何专业结论与数值。任务负责人确认的两个 v4 全局缺陷（**检查计数漂移** 12 vs 18、**最终报告自引用**）分别属 digital-engineering 与 verification-integrator 职责，已在 `risk_register.md` §5 以 RK-V6-1/RK-V6-2 登记，**非**本节点修复范围。

---

## 3. 编号与口径说明（与 v4 一致，避免断链）

v4 任务负责人在 `01` §3 以宏标签记下既有框架：三路线与八维评价、TRL 口径、风险登记「R1–R10」、WBS「A1–A8/B1–B8/C1–C8」、Gate 1（第 6 个月末）、六门「M1–M6」、首批 12 人、既有来源编号「S01–S20 / R01–R23 / E01–E09」。

本节点在**可访问材料集**中未能定位与上述宏标签**逐字对应**的独立编号源文件。为不破坏可追溯性，本域沿用可核验编号（`RK-*`、WBS `1.*`、`G1/G2/G3`、`TP-*`），并在下表给出与宏标签**语义等价**的映射。关键判断：三路线 × 三速度 × 八维评价、TRL 口径、Gate/决策门、风险/WBS/课题包框架的**实质内容**均保持，差异仅在**编号命名**；这不构成对冻结模型的改动。

| 领域 | 本域采用编号 | 说明 |
|---|---|---|
| 风险 | `RK-01 … RK-21`（v4）+ `RK-CR1/CR2/CR3`（v4 过程）+ `RK-V6-1…RK-V6-5`（**v6 新增**，见 `risk_register.md` §5） | 逐条可追溯 |
| WBS | 顶层 `1.0`，工作包 `1.1.1 … 1.11.0` | 面向 18–24 月先期论证程序的定价单元；远期概念级另列 §4 |
| 阶段决策门 | `G1`(第6月) / `G2`(第12月) / `G3`(第18–24月) | 对应 `M1–M6` 里程碑组 |
| 课题包 | `TP-01 … TP-23`（+ v4 `TP-24/TP-25`） | 见 `topic_package_next_phase.md` |
| 组织规模 | 25–30 人核心组 + 课题制（18–24 月） | 上游背景纪要 §四 |

---

## 4. 本域验证与证据（本轮真实工具调用）

| # | 验证项 | 结果 | 证据 |
|---|---|---|---|
| V-r1 | 三速度派生量独立复算（航时/动能下限/尘埃/人工重力/转向上限/相对论修正/成本敏感度比值） | 与冻结基线逐项一致 | `verification/cost_risk_recomputed_2026-09-10.txt`（本机 real 复算） |
| V-r2 | 速度×5 → 动能×25；0.03c/0.01c 动能 = 9 | 25.0000 / 9.0000 | 同上（v² 标度，derived） |
| V-r3 | 0.03c 五项锚点与 acceptance.json 容差 | 5/5 在容差内，worst rel_err 1.926e-07 | 同上 + `acceptance.json`（SHA-256 一致） |
| V-r4 | 本域不引入开放 S1 | 已避免（RK-02/03/04/05/06，见 `risk_register.md` §0 自查 + §1 状态列） | `risk_register.md` §0/§1/§5 |
| V-r5 | 禁止声称 / 单情景 / 跨路线外推合规 | 未发现违规 | 上表 C-4/C-5/C-6 grep 结果 |
| V-r6 | v4 成果复用一致性 | 本域专业内容与 v4 逐字一致（仅索引/头部元数据 + 证据链接更新 + §5 新增） | `diff` 比对（见 §2） |

> 注：本节点**未**复跑物理模型单元测试（`model_tests_pass`，属 digital-engineering/生产侧），也**未**复跑 11 项硬门槛（属 verification-integrator）；以上为**本域可负责部分**的验证。

---

## 5. 文件清单与需求绑定（v6 · 本域交付）

| 相对路径 | 标题 | 需求绑定（任务书 / acceptance） |
|---|---|---|
| `REVIEW_INDEX_cost_risk.md` | 本索引（职责/复核结论/修复/编号口径/合规/证据） | 追踪载体；`claims_use_truth_labels` |
| `techmaturity_assessment.md` | 三路线技术成熟度（TRL/IRL）× 三速度 | `REQ-RVW-04`、`R-GEN-02`、任务书 §4.4 |
| `wbs.md` | 工作分解结构（研究论证程序 + 远期概念级） | `REQ-RVW-04`、`R-GEN-02`、任务书 §4.5 |
| `cost_research_method.md` | 成本研究方法（A/B/C 层 + 三速度敏感度） | `REQ-RVW-04`、`R-GEN-02`、任务书 §5；`whole_program_cost_committed` 红线 |
| `risk_register.md` | 风险登记册（RK-01..21 + 过程风险 + v6 新增 RK-V6-*） | `REQ-RVW-04`、`no_open_severity_one_issue` |
| `termination_conditions.md` | 终止条件与阶段决策门 | `R-GEN-02`、任务书 §六.3 |
| `topic_package_next_phase.md` | 下一阶段研究课题包 | `REQ-RVW-05`、任务书 §4.4/§四.5 |
| `verification/cost_risk_recomputed_2026-09-10.txt` | 本域派生量独立复算证据（v6） | `reference_calculations_within_tolerance`（间接）、`claims_use_truth_labels` |
| `verification/cost_risk_recomputed_2026-09-09.txt` | v4 复算证据（保留，作来源比对） | 同上 |

**真值标签分布**：本域每张表的关键数值/判断均标注真值标签；缺直接证据标 `unknown`，工程外推标 `assumption`，闭合公式推导标 `derived_result`，事实标 `verified_fact`。

---

## 6. 对 G11（no_open_severity_one_issue）的可追溯说明

- **本域已避免（非开放 S1）**：RK-02（克级外推载人）/ RK-03（动能下限当完整能源预算）/ RK-04（概念图称施工图）/ RK-05（整项工程成本承诺）/ RK-06（0.03c 唯一算例）+ §0 自查项。均以「不可外推」、「下限+未含项」、「概念级标注」、「方法+量级无承诺」、「三情景并列」处置。
- **须由对应责任节点以真实执行痕迹裁定关闭（本节点仅登记/评估，不代裁量）**：RK-15 / RK-16 / RK-17 / RK-CR1/CR2/CR3，以及 v6 新增 **RK-V6-1 / RK-V6-2 / RK-V6-3 / RK-V6-4 / RK-V6-5**（见 `risk_register.md` §5）。
- **研究缺口（`unknown`，不视为缺陷）**：RK-01 / RK-09 / RK-10 / RK-12 / RK-21，对应课题包。

**因此**：就本节点所负责的成本/风险/成熟度/WBS/课题包域而言，**未引入任何开放的 S1**；跨域过程性 S1 是否关闭，交由 verification-integrator 在真实复跑后裁定。

---

## 7. 诚实边界（v6）

1. 本节点**未**运行/未见十一项硬门槛的真实复跑结果；**不**声明任何整体验收通过与否。
2. 本节点**未**执行数字应用三处修复（CR-002），也**未**独立复跑 11 项硬门槛（CR-003）。
3. 本节点**不**代表 verification-integrator 裁量 RK-15/16/17、RK-CR1/2/3、RK-V6-* 是否实际「已关闭」；只将其登记并评估影响。
4. 本节点对外副作用为 **none**；全部写入本节点 `workdir/outputs/`，未触碰只读输入/权威文件。
5. 若平台在检查点中断后续跑，**只有**本次实际继续后产生的 `outputs/*` 文件与检查证据可报告对应过程事实；本索引基于本轮真实工具调用，如实记录。

**签署**：cost-risk-analyst（成本计划与风险分析员）　**日期**：2026-09-10
