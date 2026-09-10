# 日冕先期论证恢复验证 · v6 · 正式交付修订轮 · archive-reliability-designer 节点工作简报

**节点**：`corona-prephase-a-recovery-v6-archive-reliability-designer`（档案载荷与可靠性）
**职责**：冗余维修 · 数据保存 · 概念布置 · 载荷架构
**范围**：复核并复用冻结 v4 的**文明档案载荷架构**、**长期可靠性**、**概念图纸要求**。
**日期**：2026-09-10（v6 正式交付修订轮）
**输入基线**：只读冻结源 `recovery-materials/2026-09-10-corona-v6-corrected-source/`（manifest SHA-256 `59c911db…47aa03f` 已实测一致）；
权威验收 `/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json`（SHA-256 `f4777aaf…60ec4` 已实测一致）。
**本轮动作**：① 复核冻结 `outputs/review/archive/`；② **原样复用**为本次交付（本节点 `outputs/review/archive/`）；③ 修复一处**可复现**的索引路径缺陷；④ 提供准确入口路径供 verification-integrator 汇总 `REVIEW_INDEX.md`。

---

## 1. 本节点做了什么（复用优先）

1. **核对冻结输入**：冻结源 manifest.json 与权威 acceptance.json 的 SHA-256 均与任务书一致；复用素材为 `outputs/review/archive/`（v6 已承接冻结 v4 专业内容）。
2. **复核 v4/v6 档案成果**：`06` 载荷架构、`07` 长期可靠性、`08` 概念图纸要求、`09` 概念架构示意 SVG、`index`、`support/reliability_model.py` + 输出 —— 逐项核对：速度轴三情景贯穿、不重定义冻结常量/派生量、真值标签（verified_fact / derived_result / assumption / unknown）、forbidden_claims 仅在否定/免责语境、边界声明诚实（不冒充总装员）。
3. **对照实际图纸**（digital-engineering 交付）：`drawings/FA-01` `FB-01` `FC-01` `FD-01`（SVG）＋ `RH-01`（SCAD）是否满足本节点 `08` 图纸要求。
4. **可复算验证**：本节点在 v6 复核复跑 `support/reliability_model.py` → 输出 **77 行**，与冻结 `support/reliability_model_output.txt` **逐字节一致**（`diff` 无差异）——结论/派生量/假设值未变。
5. **交付**：上述成果原样纳入本节点 `outputs/review/archive/`，并修复索引路径缺陷。

---

## 2. 复核结论（各检查项本轮实测）

### 2.1 图纸概念要求 —— 未发现可复现缺陷
| 检查 | 结果 | 证据（本轮实测） |
|---|---|---|
| SVG 全部 XML 可解析 | **通过** | FA/FB/FC/FD 四张 `.svg` 及本节点 `09_….svg` 均 `xml.etree` 解析通过 |
| 可编辑源存在 | **通过** | `.svg` 为文本可编辑；`RH-01_rotating_habitat.scad` 为 OpenSCAD 源码 |
| 全部「概念级」免责 | **通过** | 各图均含「概念级 · 不可用于施工/制造/飞行认证」 |
| 速度轴三情景贯穿 | **通过** | 各图均出现 0.01c / 0.03c / 0.05c；FD-01 第 63 行保留纠正脚注「1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」（应纳入 G10） |
| forbidden_claims | **通过** | 「飞行认证/施工/制造」仅出现在否定/免责语境 |
| baseline 标注 | **通过** | 各图含 `corona-prephase-a-v1`、图号、版本、日期、who |

### 2.2 长期可靠性模型 —— 可复算通过（已复核，无现存缺陷）
- 复跑 `support/reliability_model.py`，退出码 0，输出 77 行，与冻结 `reliability_model_output.txt` **逐字节一致**（`diff` 无差异）。
- 冻结源码中已不含死代码 `ecc_match()` / 未定义变量 `P_none`（仅存在于头部注释说明）。结论/派生量/假设值未变。

### 2.3 文档（06 / 07 / 08）—— 未发现可复现**内容**缺陷
速度轴三情景、不重定义冻结量、真值标签齐全、未知项显式 `unknown`、无 forbidden_claims、边界声明诚实 —— **通过**。故文档**不改写内容**，仅保留 v6 复用标识。

### 2.4 本节点修复的「可复现」缺陷（索引路径 v4 → v6）
- **缺陷**：`index_archive_reliability_designer_v1.0.md` §1 交付件表中 4 条路径为冻结时的 `06_corona_v4_*` / `07_corona_v4_*` / `08_corona_v4_*` / `09_corona_v4_*`，但冻结 v6 包内实际文件名均为 `06_corona_v6_*` / `07_corona_v6_*` / `08_corona_v6_*` / `09_corona_v6_*`。
- **复现**：按 v4 名 `ls` / 按索引找 `06_corona_v4_archive_payload_architecture_v1.0.md` → `No such file or directory`（本轮实测 MISSING）。若 verification-integrator 依索引列 `REVIEW_INDEX.md` 路径，将触发「路径不存在」失败。
- **修复**：把该表路径更正为冻结 v6 包内**真实存在**的 v6 文件名，与目录内实际文件逐项一致；其余内容一字未改。修复依据：冻结包内实际文件清单（见 §4）。
- **影响面**：仅影响本节点索引的路径展示，不改变 06/07/08/09 专业内容；不涉及共享模型、应用、图纸等其它域成果。

---

## 3. 诚实边界（本节点未做 / 未判定）

- 本节点**只做专业成果复核与复用**，**未**独立复跑原 `acceptance.json` 的十一项硬门槛 G1–G11（属 **verification-integrator**），**未**生成 `outputs/verification/acceptance.json`、`browser-evidence.json`、`outputs/FINAL_ACCEPTANCE.md`，故**不**对 v6 十一项门槛做任何 PASS/FAIL 判定，**不**声明 `overall: PASS`。
- 本节点**未**修改数字应用（`app/`）、未出 `drawings/` 交付要件（属 digital-engineering-builder）；`09_corona_v6_archive_concept_architecture_diagram.svg` 仅为本节点**概念示意**，非交付要件。
- 本节点**未**生成根级 `outputs/review/REVIEW_INDEX.md`（属 verification-integrator，主交付人）；本节点只提供本域入口与真实相对路径。
- 可靠性数值中 `assumption`（剂量率、介质保留期、屏蔽系数）与 `unknown`（尘埃通量）**均需独立核验**，**不**当作 A 级事实或采购依据。
- 路线 B 整机按冻结口径仍为 **TRL 4–6（概念级）**；组件 TRL ≠ 系统 TRL。
- 所有输入均为只读；本节点未写入只读冻结源；外部副作用范围 **none**。

---

## 4. 交付文件（真实相对路径，本节点 `outputs/`）

> 以下路径均为本节点 `outputs/` 下**实际存在**的交付件（供 verification-integrator 写入根级 `REVIEW_INDEX.md` 的 archive 条目）。

| 相对路径 | 内容 |
|---|---|
| `outputs/review/archive/index_archive_reliability_designer_v1.0.md` | 本节点**交付包索引**（入口，已修复 v4→v6 路径） |
| `outputs/review/archive/06_corona_v6_archive_payload_architecture_v1.0.md` | 文明档案载荷**架构**（复用 v4，无内容缺陷） |
| `outputs/review/archive/07_corona_v6_archive_long_term_reliability_v1.0.md` | 长期**可靠性**（复用 v4，模型可复算一致） |
| `outputs/review/archive/08_corona_v6_archive_concept_drawing_requirements_v1.0.md` | 概念**图纸要求**（复用 v4，无内容缺陷） |
| `outputs/review/archive/09_corona_v6_archive_concept_architecture_diagram.svg` | 概念架构**示意 SVG**（XML 解析通过，无脚本/外链） |
| `outputs/review/archive/support/reliability_model.py` | 长期可靠性**可复算模型**（v6 重跑 77 行一致） |
| `outputs/review/archive/support/reliability_model_output.txt` | 模型真实运行**输出证据**（77 行） |
| `outputs/review/archive/archive-reliability-node-workbrief-v6.md` | 本工作简报（本文件） |

**archive 入口（供 REVIEW_INDEX 引用）**：`outputs/review/archive/index_archive_reliability_designer_v1.0.md`（包内列全部交付件与真实路径）。

---

## 5. 交接给下游

| 下游 | 交接内容 |
|---|---|
| digital-engineering | `08` 图纸要求（Fig B-01…B-05 建议 + 标注规范）；`06` 架构；`07` 可靠性 |
| systems-architect | 构型 A/B 选择、L1 内容策展与多代语义的治理/伦理校核信号 |
| verification-integrator | 本节点专业成果（`review/archive/` 全包）+ `support/reliability_model.py`（可复算）+ 概念级图纸注记；入口 `index_archive_reliability_designer_v1.0.md`；由其在本人 `outputs/review/REVIEW_INDEX.md` 汇总本域，并独立复跑形成唯一最终目录与 `FINAL_ACCEPTANCE.md` |

---

## 6. 复用 vs 修改声明

- **复用**：`06` / `07` / `08` / `09` / `support/*` 五件与冻结源**逐字节一致**（`diff -r` 无差异），原样复用；专业内容未改。
- **修改（1 处，可复现缺陷）**：`index_archive_reliability_designer_v1.0.md` §1 路径表 v4 名 → v6 真实文件名（复现步骤见 §2.4）。
- **未改动**：共享参数模型、`app/`、`drawings/`、`model/`、其它域评审包；本节点**未**修改团队、运行时绑定或交付路径。
