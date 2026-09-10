# 日冕计划 Pre-Phase A · v6 · systems-architect 复用声明与 REVIEW_INDEX 入口

**节点**：`corona-prephase-a-recovery-v6-systems-architect`（任务架构与需求 / 系统工程师）
**运行**：2026-09-10 · v6 正式交付修订轮（隔离恢复验证，复用冻结 v6 成果）
**基线**：`corona-prephase-a-v1`（v1.0.0，`frozen_for_validation`，速度轴 `0.01c / 0.03c / 0.05c`）
**本轮性质**：本节点**只交付** 需求分解 / 接口登记 / 路线终态 / 追踪矩阵 / 配置基线（即 `review/systems/` 控制件包）——**原样复用**冻结 v6，不在可复现缺陷之外做任何改动。
**外部副作用**：`none`；全部文件写入本节点自身 `outputs/`。

---

## 0. 诚实边界（必须先读）

- 本节点是 **systems-architect**，**不是** digital-engineering、**不是** mission-physics-analyst、**不是** verification-integrator。
- 因此本节点**未**生成统一模型、应用、图纸、整套交付或 `FINAL_ACCEPTANCE.md`；**未**创建根级 `outputs/review/REVIEW_INDEX.md`（属独立验证与总装员）；**未**复跑十一项硬门槛（G1–G11）、**未**做浏览器三速切换/导出（属 verification-integrator）。
- 本节点**不**作 `overall: PASS/FAIL` 判定；是否整体通过以 verification-integrator 真实复跑后的 `FINAL_ACCEPTANCE.md` 为准。

---

## 1. 复用声明（reuse，非修改）

下表 10 个 `review/systems/` 控制件本轮**原样复用**。复用判定依据：对只读冻结源与本节点 `outputs/` 两条路径分别测 `shasum -a 256`，两者**逐字节一致**，且与冻结 `manifest.json` 中对应条目 SHA-256 完全一致。

| 控制件 | 相对路径（本节点 `outputs/`） | SHA-256（= 冻结 manifest） | 服务硬门槛 |
|---|---|---|---|
| 域 README | `review/systems/README.md` | `42ebc1d3c81422afcf3134fd5bdac20cacc366ac2f719e97ef356c5f22a2a4f1` | — |
| 需求分解 | `review/systems/requirements/requirements_decomposition.md` | `8798b3840b354429dcae31e3799af99df505250c909955dd509bfae0177ced9f` | G8 / G9 / G10 |
| 接口登记 | `review/systems/interfaces/interface_register.md` | `a1a67fe4ccceee921935199dead665e59f15521361f5424c6bbaec922f79119b` | G8 / G10 / G11 |
| 路线终态 | `review/systems/routes/route_terminal_states.md` | `63bf7aec818c87d25561535d7013ddc9d0c3623d0d3b69ed9fb699f0bce60cd0` | G2 / G9 |
| 追踪矩阵（MD） | `review/systems/verification/traceability_matrix.md` | `a6b05142ae1657382d08a7bf895214d8dd004f2d5cf861dab670ba0bfc558a96` | G8 / G9 |
| 追踪矩阵（CSV） | `review/systems/verification/traceability_matrix.csv` | `a8dce39bba2f4dbc0bc4f501c18e9fd8a564e491fed00f0707451d05d92b4645` | G8（机器侧） |
| 参数一致性核验脚本 | `review/systems/verification/systems_param_consistency_check.py` | `f31a2fd65bc2c7e1b488d653fd70ab3eba9ee1733be8d3b7d11b8b685a98c70c` | G10（systems 侧） |
| 参数一致性证据 | `review/systems/verification/systems_param_consistency_check.txt` | `1f485958bf96e131675b2ea1fecd828231c380d981acbace05784fc6088824e6` | G10（systems 侧） |
| 配置基线注册 | `review/systems/config/config_baseline_register.md` | `25e3b81901bb4c01400cf8a701c392754b682b90fe637eb7f56384ec3e2a47a9` | G10 |
| 配置项注册（机器可读） | `review/systems/config/config_items.yaml` | `2271da864bb3679e7a2f4d68160dd3e006106e9466ea584745ecabe0b3cfdef9` | G10 |

> 本声明文件（`v6_reuse_declaration.md`）为**本轮新增**，用于自述复用与提供 REVIEW_INDEX 入口，**不属于**上表冻结复用件。

---

## 2. 本轮复核证据（真实工具调用）

| # | 事项 | 结果 |
|---|---|---|
| R1 | 10 个 `review/systems/` 文件 frozen ↔ 本节点 `outputs/` 逐字节 SHA-256 比对 | **10/10 一致**（与冻结 `manifest.json` 同值） |
| R2 | 冻结源 `outputs/review/systems/verification/systems_param_consistency_check.py` 在本节点 `outputs/` 重跑 | `exit 0`（修复 cp 继承只读权限后）；重生成 `systems_param_consistency_check.txt`，SHA-256 与冻结版**一致** |
| R3 | 重跑结果对照权威 `acceptance.json` 五项参考锚点 | **5/5 在容差内**，`all_ok=True`，max rel_err ≈ 2.6e-08 |
| R4 | 追踪矩阵 CSV（31 行 × 9 列）逐条 `evidence_path` 解析并做**路径存在性核验**（相对冻结源 `outputs/`） | 31 行 / 13 个唯一 `outputs/*` 路径**全部存在**，0 脱链 |
| R5 | FD-01 修正脚注 | 冻结源第 63 行存在：「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」，未改、保留 |
| R6 | 冻结 `manifest.json` 检查 `__pycache__` | 67 个产物，**0** 个 `__pycache__` 条目 |

---

## 3. v4 → v6 已修正的可复现缺陷（冻结 v6 已含，本轮确认保留）

冻结 v6 `review/systems/` 相对 v4 已修正两点（本节点复核确认，**非本轮新增**）：

1. **追踪矩阵证据路径脱链**：v4 `evidence_path` 写作 `outputs/requirements/…`、`outputs/config/…` 等（`required_final_paths` 并无这些顶层路径）；冻结 v6 已全部校正为实际可达路径 `outputs/review/systems/…`。本节点 R4 复核：31 行全部可达。
2. **虚构运行时 UUID**：v4 硬编码 `41233bee-…` / `84da6261-…`；冻结 v6 不代为填写，仅在 IFM-13 登记「生产侧与独立总装侧必须分属不同运行时」的过程约束。本节点沿用。

---

## 4. 本节点 `review/systems/` 入口与相对路径（供 verification-integrator 汇总进根级 `REVIEW_INDEX.md`）

> 以下路径均相对**最终交付根 `outputs/`**，且在本节点 `outputs/` 内已存在。

**主入口**：`review/systems/README.md`

| 子族 | 相对路径 | 内容 |
|---|---|---|
| 需求分解 | `review/systems/requirements/requirements_decomposition.md` | `SA-REQ-G/A/B/C/I/T-*` 架构级需求，每条含验证方法 |
| 接口登记 | `review/systems/interfaces/interface_register.md` | `IFM-01..IFM-14`（provider/consumer/contract/verification/gate） |
| 路线终态 | `review/systems/routes/route_terminal_states.md` | A/B/C × 0.01c/0.03c/0.05c 终态/成功定义/退出条件 + 冻结派生量表 |
| 追踪矩阵 | `review/systems/verification/traceability_matrix.md` / `.csv` | 需求↔情景↔方法↔证据↔门槛双向可追溯（31 行） |
| 配置基线 | `review/systems/config/config_baseline_register.md` / `config_items.yaml` | 引用冻结 `corona-prephase-a-v1`；CI-01..CI-08 |
| 一致性核验 | `review/systems/verification/systems_param_consistency_check.py` / `.txt` | 5 锚点重跑脚本 + 证据 |

---

## 5. 与整体交付（不含本节点责任的部分）的接口约定

- 本节点所用全部数值同源于冻结基线 `corona-prephase-a-v1`（`v4-outputs/model/baseline.yaml` 自包含副本）与权威 `acceptance.json`；本节点**未**自造任何常量/速度/航时/功率。
- `required_final_paths` 以权威 `acceptance.json` 数组为准（`outputs/model/`、`outputs/drawings/`、`outputs/app/`、`outputs/review/`、`outputs/verification/`、`outputs/FINAL_ACCEPTANCE.md` = **6 项**）。上游 `frozen-input-inventory.md` §6 表格记 `required_final_paths×7`，与本节点按权威文件的**实测 6 项**存在数字差；本节点以权威文件为准，将该数字差留待任务负责人/总装对账（不影响本节点任何判定）。

---

## 6. 结论

- 本节点已完成其职责：需求分解 / 接口登记 / 路线终态 / 追踪矩阵 / 配置基线，全部**原样复用**冻结 v6（10/10 文件逐字节一致，SHA-256 与冻结 `manifest.json` 相符）。
- 本节点范围内**未发现新增可复现缺陷**；唯一已知正式交付缺陷（缺根级 `outputs/review/REVIEW_INDEX.md`）在本节点职责之外，由 verification-integrator 闭环。
- 整体 `overall: PASS/FAIL` **未决**——由 verification-integrator 在真实复跑 G1–G11 后判定。

*由 systems-architect 节点产出；与需求分解、接口登记、路线终态、追踪矩阵、配置基线注册共同构成节点控制包。*
