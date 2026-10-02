# 日冕计划 · Pre-Phase A · v6 · 正式交付修订轮 — cost_risk 域复用声明

**节点**：`cost-risk-analyst`（成本计划与风险分析员）
**团队**：`corona-prephase-a-recovery-v6`
**轮次**：正式交付修订（2026-09-10）；上游冻结源 `recovery-materials/2026-09-10-corona-v6-corrected-source/`（manifest SHA-256 `59c911db…`）
**本文件性质**：本轮**追加的节点级复用/修改自述**，供 verification-integrator 汇总根级 `outputs/review/REVIEW_INDEX.md` 时引用。
**域内权威索引**（不变）：`REVIEW_INDEX_cost_risk.md`（冻结 v6 既有原件，本轮**未改动**，且位于本节点 `outputs/review/cost_risk/`）。

---

## 1. 本轮决策：`cost_risk` 域 = 原样复用（未修改任何专业内容）

经本轮真实工具调用复核，冻结 v6 的 `cost_risk` 域在**数值、三情景口径、禁语、真值标签、跨路线边界、S1 处置、证据路径**上均无领域内缺陷，**整体复用**。

| 项 | 本轮真实检查（工具调用） | 结果 |
|---|---|---|
| 9 个冻结件 SHA-256 与冻结源 manifest | `shasum`-等价 hash 比对（`outputs/review/cost_risk/*` + `verification/*`） | 9/9 与 manifest 一致 ✅ |
| 5 个参考锚点独立复算 | python3 重算 `travel_years_at_0_03c / ke_1mt / ke_5mt / gravity_1km_2rpm / dust_1mg` | **5/5 在容差内**；worst rel_err = **1.926e-07**（gravity 1km 2rpm）✅ |
| 三速度派生量独立复算 | python3 重算三档（航时/动能/尘埃/人工重力/转向上限/相对论校正/成本敏感度比值） | 与冻结基线逐项一致；0.05c/0.01c 动能 = **25.0000**、0.03c/0.01c = **9.0000** ✅ |
| 权威 `acceptance.json` | 读取 `<MAINTAINER_LOCAL_PATH>` | 存在；SHA-256 = `f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4`（与任务书/上游一致）✅ |
| 域内是否引入开放 S1 | 逐条核 `risk_register.md` §0/§1/§5 | RFC 02/03/04/05/06 及本域自查项**已避免**；跨域 S1（RK-15/16/17、RK-CR1/2/3、RK-V6-1…5）已登记但**不**本节点裁量关闭 ✅ |

> 专业内容（六份 `.md` 交付件 + 两份 `verification/*.txt` 证据）**逐字未改**；仅本文件为本轮**追加**的复用自述。

---

## 2. 本域交付件清单与相对路径（供根级 REVIEW_INDEX.md 引用；均已在本节点 `outputs/` 中实际存在）

| 相对路径（相对本节点 `outputs/`） | 标题 | 说明 |
|---|---|---|
| `review/cost_risk/REVIEW_INDEX_cost_risk.md` | 域索引（职责/复核结论/编号口径/合规/证据/边界） | **域内权威索引**；本轮复用未改 |
| `review/cost_risk/v6-revision-reuse-declaration.md` | 本轮复用自述 | 本文件 |
| `review/cost_risk/techmaturity_assessment.md` | 三路线技术成熟度（TRL/IRL）×三速度 | 复用 |
| `review/cost_risk/wbs.md` | 工作分解结构 | 复用 |
| `review/cost_risk/cost_research_method.md` | 成本研究方法 | 复用 |
| `review/cost_risk/risk_register.md` | 风险登记册 | 复用 |
| `review/cost_risk/termination_conditions.md` | 终止条件与阶段决策门 | 复用 |
| `review/cost_risk/topic_package_next_phase.md` | 下一阶段研究课题包 | 复用 |
| `review/cost_risk/verification/cost_risk_recomputed_2026-09-10.txt` | v6 独立复算证据 | 复用 |
| `review/cost_risk/verification/cost_risk_recomputed_2026-09-09.txt` | v4 复算证据（保留，作来源比对） | 复用 |

> 上述所有路径在**本节点 `outputs/`** 中均实测存在，且与冻结源逐字节一致。

---

## 3. 冻结件复用清单（未改，SHA-256 与 manifest 一致）

`REVIEW_INDEX_cost_risk.md` a9656384… · `techmaturity_assessment.md` 4a516f20… · `wbs.md` ea9b9148… · `cost_research_method.md` 32883973… · `risk_register.md` 48349303… · `termination_conditions.md` 6ef1d9bb… · `topic_package_next_phase.md` 1b8a590b… · `verification/cost_risk_recomputed_2026-09-10.txt` c8dd8510… · `verification/cost_risk_recomputed_2026-09-09.txt` a641490b…

---

## 4. 观察项（不修改，无验收门依赖；供总装员知悉）

1. `REVIEW_INDEX_cost_risk.md` 头部所述「只读基线 `recovery-materials/2026-09-10-corona-v5-source/`」是**上一轮内容派生来源**的溯源注记；**本轮**权威冻结基线为 `2026-09-10-corona-v6-corrected-source/`（manifest `59c911db…`）。该注记不指向本节点交付路径，亦不影响根级 `REVIEW_INDEX.md` 的路径存在性校验；按「复用优先、仅在可复现缺陷时改」，**未改动**。
2. 相对论校正显示占比：`cost_risk_recomputed_2026-09-10.txt` 给出 `+0.0075% / +0.0676% / +0.1879%`，`cost_research_method.md` 给出 `+0.008% / +0.068% / +0.188%`；两者**因子一致**（`1.00008 / 1.00068 / 1.00188`），仅显示四舍五入位数不同。该量**非** `reference_checks` 五项锚点之一，无门依赖；按复用优先未改动。

---

## 5. 诚实边界（本节点）

- 本节点**未**运行 G1–G11、**未**做浏览器 `0.01c/0.03c/0.05c` 切换与导出 JSON、**未**创建根级 `outputs/review/REVIEW_INDEX.md`、**未**生成 `outputs/verification/acceptance.json`、`outputs/verification/browser-evidence.json`、`outputs/FINAL_ACCEPTANCE.md` —— 以上均属独立验证与总装员职责。
- 本节点**不**作任何整体验收 `overall: PASS/FAIL` 判定（属 verification-integrator）。
- 本节点**不**代为关闭跨域 S1（RK-15/16/17、RK-CR1/2/3、RK-V6-1…5）；仅在本域登记并评估影响，由对应责任节点以真实执行痕迹裁定。
- 外部副作用 `none`；本轮全部写入本节点 `workdir/outputs/`，未触碰只读冻结源与权威 `acceptance.json`。

**签署**：cost-risk-analyst（成本计划与风险分析员）　**日期**：2026-09-10
