# 日冕计划 Pre-Phase A · v6 · systems-architect（系统工程师）控制件包

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 **隔离恢复验证**（复用 v4，仅改可复现缺陷；非从零重做）
**基线**：`corona-prephase-a-v1`（v1.0.0，`frozen_for_validation`，速度轴 `0.01c / 0.03c / 0.05c`）
**职责**：任务终态 / 接口登记 / 追踪矩阵 / 需求分解 / 配置基线。

---

## 1. 本包交付物（deliverables）

| 控制件 | 文件（相对本包 `outputs/review/systems/`） | 内容 | 服务硬门槛 |
|---|---|---|---|
| 需求分解 | `requirements/requirements_decomposition.md` | `SA-REQ-*` 架构级需求，每条含验证方法 | G8、G9、G10 |
| 接口登记 | `interfaces/interface_register.md` | `IFM-01..IFM-14`（provider/consumer/contract/verification/gate） | G8、G10、G11 |
| 路线终态 | `routes/route_terminal_states.md` | A/B/C × 三速度终态/成功定义/退出条件 + 冻结派生量 | G2、G9 |
| 追踪矩阵（MD） | `verification/traceability_matrix.md` | 需求↔情景↔方法↔证据↔门槛双向可追溯 | G8、G9 |
| 追踪矩阵（CSV） | `verification/traceability_matrix.csv` | 31 行 × 9 列机器可读 | G8（机器侧） |
| 参数一致性核验 | `verification/systems_param_consistency_check.py` | 转发一致性重算脚本（可重跑） | G10（systems 侧） |
| 参数一致性证据 | `verification/systems_param_consistency_check.txt` | 5/5 锚点在容差内（`all_ok=True`） | G10（systems 侧） |
| 配置基线注册 | `config/config_baseline_register.md` | 引用冻结 `corona-prephase-a-v1`；CI-01..CI-08 | G10 |
| 配置项注册（机器可读） | `config/config_items.yaml` | register 类型，不重定义参数 | G10 |

---

## 2. 本节点真实工具调用证据（v6 本轮）

| # | 事项 | 结果 |
|---|---|---|
| C1 | 用冻结常量独立重算派生量表（V_KMPS / TIME_ALPHA / TIME_GLENS / TIME_PRECURSOR / KE / P_REL / 人工重力） | `systems_param_consistency_check.py` 运行，`exit 0` |
| C2 | 与 `acceptance.json` 五项参考锚点比对 | **5/5 在容差内，`all_ok=True`**（max rel_err ≈ 2.6e-08） |
| C3 | 校验追踪矩阵 CSV（9 列、31 行、ID 唯一、evidence_path 无脱链） | 通过（python csv 校验） |

---

## 3. 相对 v4：修复的可复现缺陷（本节点范围内）

1. **追踪矩阵证据路径脱链**：v4 的 `evidence_path` 写作 `outputs/requirements/…`、`outputs/config/…`、`outputs/routes/…`、`outputs/interfaces/…`，而实际交付布局为 `outputs/review/systems/…`（`required_final_paths` 并无 `outputs/requirements/` 等顶层路径）。v6 已全部校正为实际可达路径。
2. **失效上游引用**：v4 大量引用 `inputs/lead/01/03/04/05`、`inputs/lead/baseline/baseline.yaml`、`inputs/lead/requirements/requirements_baseline.md`——这些不在 v6 输入集内。v6 改以 `v4-outputs/model/baseline.yaml`（冻结配置基线自包含副本）、`acceptance.json`、`original-materials/materials/*.md`、`inputs/lead/*` 为依据。
3. **虚构运行时 UUID**：v4 硬编码 `41233bee-…`/`84da6261-…`。v6 不代为填写运行时/UUID（由平台编排），仅在 IFM-13 登记「生产侧与独立总装侧必须分属不同运行时」约束。

---

## 4. 不在本节点职责内（如实，未执行/未触及）

- **数字工程（检查计数漂移 + 最终报告自引用）**：`machine_verification_checklist.md` 声明 M1–M12（12 项）vs `run_checks.py` 实际注册（18 项），以及 `FINAL_ACCEPTANCE.md` 以「本文件」/§三/§四作为自身证据——属 digital-engineering 修复范畴，**本节点不改写**。
- **应用真实切换 / 导出 JSON**：`REQ-V4-C-04..C-06` 状态 `未执行`，属 digital-engineering。
- **十一项硬门槛独立复跑 + 唯一最终目录 + `FINAL_ACCEPTANCE.md`**：`REQ-V4-P-02..P-04` 状态 `未执行`，属 verification-integrator。
- **数据缺口**：v4 追踪矩阵未含 `REQ-V4-C-02`；v6 输入集不含 `requirements_baseline.md`，本节点无法重建，已列为缺口交总装/任务负责人对账，不虚构补全。

---

## 5. 诚实边界（结论）

- 本节点是 **systems-architect**，**不得**声明 v6 十一项硬门槛通过；整体 `overall: PASS` 仅当 verification-integrator 真实复跑十一项全部通过时成立——本节点未复跑、未见结果，故保持未决。
- 本节点**未生成**统一模型、应用、图纸、整套交付或 `FINAL_ACCEPTANCE.md`。
- 本包全部写于本节点自身 `outputs/`；`acceptance.json`、`original-materials/`、`v4-outputs/` 均只读，未改写。

---

*由 systems-architect 节点产出；与需求分解、接口登记、路线终态、追踪矩阵、配置基线注册共同构成节点控制包。*
