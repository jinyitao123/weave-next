# 日冕计划 v6 复用 v4 · archive-reliability-designer 节点交付包

**节点**：corona-prephase-a-recovery-v6-archive-reliability-designer（档案载荷与可靠性）
**运行时**：v6 平台承载当前运行；本包为 v6 **复核并复用**冻结 v4 成果（原文为 v4 复验，专业内容未改）。
**基线**：`corona-prephase-a-v1`（冻结，v1.0.0，速度轴 0.01c / 0.03c / 0.05c）
**节点职责**：冗余维修 · 数据保存 · 概念布置 · 载荷架构
**交付承诺**：文明档案载荷架构、长期可靠性、概念图纸要求。
**生成**：2026-09-09（v4 作者）；v6 复核复用于 2026-09-10

---

## 1. 本节点交付文件（均 UTF-8，位于本节点 `outputs/review/archive/` 下）

> **v6 复核修订（2026-09-10）**：本表初版把文件路径写为冻结时的 `06_corona_v4_*` / `07_corona_v4_*` / `08_corona_v4_*` / `09_corona_v4_*`，但冻结 v6 包内实际文件名为 `*_v6_archive_*`（v6 复用 v4 后已更名）。按 v4 名查找将得到「No such file or directory」——属**可复现缺陷**。本次已把路径**更正为冻结 v6 包内的真实文件名**，与目录内实际文件逐项一致。

| 相对路径（真实存在，相对本 `outputs/review/archive/`） | 内容 | 类型 |
|---|---|---|
| `06_corona_v6_archive_payload_architecture_v1.0.md` | 文明档案载荷**架构**：五层双介质（传世层+完整层）· 速度轴影响 · 两构型(搭载/独立) · 参数字典 | 架构 |
| `07_corona_v6_archive_long_term_reliability_v1.0.md` | 长期**可靠性**：辐射/介质寿命/K-of-N/ECC/冗余维修/数据保存 · 未知尘埃通量 | 可靠性 |
| `08_corona_v6_archive_concept_drawing_requirements_v1.0.md` | 概念**图纸要求**：Fig B-01…B-05 · 标注规范 · 验证方法 | 图纸要求 |
| `09_corona_v6_archive_concept_architecture_diagram.svg` | 概念架构示意 SVG（自包含，XML 解析通过，无外部资源/脚本） | 概念示意 |
| `support/reliability_model.py` | 长期可靠性**可复算模型**（本节点真实运行，已移除死代码 `ecc_match()`） | 复现脚本 |
| `support/reliability_model_output.txt` | 模型真实运行**输出证据**（77 行，节选已写入 `07`；v6 重跑与 v4 逐字节一致） | 证据 |

## 2. 本节点真实工具调用证据（本轮）

- **v6 复核**：在 v6 复跑 `support/reliability_model.py`（移除未调用、引用未定义变量 `P_none` 的死代码 `ecc_match()`）→ 输出 77 行，与 v4 `support/reliability_model_output.txt` **逐字节一致**（`diff` 无差异）。结论/派生量/假设值未变。
- **v6 对照图纸**（v4 成果）：FA-01/FB-01/FC-01/FD-01 `.svg` 均 XML 解析通过、均含三速度情景与「概念级」标注、forbidden_claims 仅在否定/免责语境；RH-01 `.scad` 为可编辑源码。未发现可复现图纸缺陷。
- **v6 概念示意**：复用 `09_corona_v6_archive_concept_architecture_diagram.svg`，`xml.etree` 解析通过，无 `<script>`、无外部资源。
- （以下是 v4 本轮原始证据，v6 复用为此前的成果基线）运行 `python3 outputs/support/reliability_model.py` → 输出写入 `support/reliability_model_output.txt`。
- 派生量（`TIME_ALPHA`：425 / 141.667 / 85 yr；1mg 尘埃撞击能：4.49e6 / 4.04e7 / 1.12e8 J）与冻结基线 `baseline.yaml` / `01` 一致（derived_result·frozen）。
- 辐射剂量（0.20 Gy/yr 无屏蔽假设）→ 85 / 28.33 / 17 Gy；30 g/cm² Al 屏蔽下 61 / 20.3 / 12.2 Gy（assumption，需核验）。
- 介质单副本存活 p(T)：模拟蚀刻 ≈0.9995–0.9999；NAND/磁带 0.000–0.18；光存 0.215–0.75（assumption R）。
- K-of-N 达 P≥0.99：单副本 p=0.90 → 5-of-3（0.9914）；p=0.85 → 10-of-6（0.9901）（derived_result，副本独立假设）。
- SVG 用 `xml.etree` 解析通过，无 `<script>`、无外链（verified）。

## 3. 关键结论（一句话 ×3）

1. **架构**：路线 B 档案载荷应采用**双介质**——「传世层（模拟蚀刻，辐射近免疫、零耗电、被动 R≈1e6 yr）」保证文明核心跨 85–425 年；「完整层（光存/加固固态 + K-of-N + ECC）」保证全语料尽最大可能完整；两者互为备份。
2. **可靠性**：`TIME_ALPHA` 跨度 85–425 yr 使任何需主动刷新的数字介质**在物理上无法作为主存活层**；因此「冗余维修」在无人无维护下**退化为内建自愈**（K-of-N + ECC + 多样化抑制共模），而非在线修复。
3. **图纸**：概念级图纸须满足 `08` 的 Fig B-01…B-05 内容与统一标注规范（baseline_id / 版本 / 图号 / 三情景 / **概念级免责**），且**禁用** forbidden claims；数字工程成员按此绘制，总装员按解析/扫描复核。

## 4. 诚实边界（必须说明）

- 本节点**未**修复数字应用（属 digital-engineering-builder，CR-002），**未**独立复跑十一项硬门槛、**未**生成唯一最终目录与 `FINAL_ACCEPTANCE.md`（属 verification-integrator，CR-003）。
- 本节点**不**冒充独立总装员；本包是**专业成果 + 图纸要求**，**不是**验收结论。
- 本节点**未**独立复算 0.03c 五项锚点（属 mission-lead 已做，`baseline/anchors_reverified_2026-09-09.txt` 为上游输入）；本节点只引用 frozen 派生值。
- 可靠性数值中 `assumption`（剂量率、介质保留期、屏蔽系数）与 `unknown`（尘埃通量）**均需独立核验**，**不**作为 A 级事实或采购依据。
- 路线 B 整机按 frozen §3 仍为 **TRL 4–6（概念级）**；组件 TRL ≠ 系统 TRL，本节点已声明。

## 5. 交接给下游

| 下游 | 交接内容 | 形式 |
|---|---|---|
| digital-engineering-builder | `08` 图纸要求 + `06` 架构 + `07` 可靠性 → 绘制 `drawings/`（Fig B-01…B-05 ≥3 SVG + 1 SCAD） | 要求文档 |
| systems-architect | 构型 A/B 选择、L1 内容策展与多代语义的治理/伦理校核信号 | 架构约束 |
| verification-integrator | 本节点专业成果 + `support/reliability_model.py`（可复算）+ 概念级图纸注记 | 文件证据 |

> 本节点按派发契约只完成「档案载荷架构 / 长期可靠性 / 概念图纸要求」；**不**越域生成最终验收或冒充总装员。
