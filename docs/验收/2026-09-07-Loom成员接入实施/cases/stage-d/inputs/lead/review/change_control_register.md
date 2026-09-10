# 变更控制登记册与基线冻结记录

- **团队**: 日冕先期论证并行工程团队 v3 · luna
- **节点/职责**: luna-mission-lead（变更控制 · 接口管理 · 最终决策 · 需求基线）
- **日期**: 2026-09-06
- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0`

> 事实标签约定：每条陈述标注 [verified_fact] / [derived_result] / [assumption] / [unknown]。

---

## 1. 基线冻结记录（FREEZE-001）

| 项 | 内容 | 事实标签 |
|---|---|---|
| 冻结对象 | `outputs/model/baseline_frozen.yaml` | [verified_fact] |
| baseline_id | `corona-baseline-1.0.0` | [verified_fact] |
| content_digest (SHA-256) | `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2` | [verified_fact]（由本轮真实命令 `shasum -a 256` 返回） |
| 冻结日期 | 2026-09-06 | [verified_fact] |
| 批准范围 | 单一 0.03c 巡航速度情景、baseline 1.0.0；三路线可分离比较 | [verified_fact]（来源：本轮 run_input） |
| 冻结状态 | frozen —— 未经变更控制程序不得修改 | [verified_fact] |

### 1.1 冻结依据的四个真实输入文件（本轮已实际读取）

1. `/Users/jinyitao/Documents/日冕/日冕计划_任务背景与全程纪要_修订稿.md` [verified_fact]
2. `/Users/jinyitao/Documents/日冕/complex-validation/baseline.yaml`（父基线 `corona-prephase-a-v1`，状态 `frozen_for_validation`）[verified_fact]
3. `/Users/jinyitao/Documents/日冕/complex-validation/日冕复杂团队验收任务书.md` [verified_fact]
4. `/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json` [verified_fact]

读取方式：本轮 Read 工具真实调用返回，未扫描同机其他成员工作目录，未使用任何旧运行文件。

### 1.2 冻结前参考值独立复算（支撑冻结决策的有界命令证据）

命令：`python3` 内联脚本（常量 c=299792458 m/s，距离 4.25 ly，匀速 0.03c，非相对论动能 KE=½mv²，向心加速度 a=ω²r，ω=2π·rpm/60）。

| 校核项 | acceptance.json 期望值 | 本轮复算值 | 相对偏差 | 容差 | 结论 |
|---|---|---|---|---|---|
| 航行时间 @0.03c (年) | 141.6666667 | 141.66666666666669 | ~0 | 0.001 | 一致 |
| 动能下限 1Mt @0.03c (J) | 4.0443983e22 | 4.04439830431568e+22 | ~0 | 0.01 | 一致 |
| 动能下限 5Mt @0.03c (J) | 2.0221991e23 | 2.02219915215784e+23 | ~0 | 0.01 | 一致 |
| 人工重力 1km/2rpm (m/s²) | 43.8649 | 43.864908449286034 | ~0 | 0.01 | 一致 |
| 尘埃 1mg @0.03c (J) | 40443983 | 40443983.043156795 | ~0 | 0.01 | 一致 |

[derived_result] 另算得相对论动能对照（1Mt @0.03c，(γ−1)mc²）≈ 4.0471e22 J，与非相对论值偏差约 0.07%，供模型成员做非相对论/相对论对照时参考。

说明：本复算仅为冻结决策支撑，不替代验证阶段总装员的正式自动测试；正式 PASS/FAIL 以验证包实测记录为准。

### 1.3 运行独立性声明

- 本轮为新的、独立的完整验收运行；旧运行及其失败记录由平台侧保留，本轮不重试、不覆盖、不冒充旧运行。[verified_fact]（来源：run_input）
- 本轮未读取、复制或引用任何旧运行产物。[verified_fact]（本轮工具调用记录可证）
- 服务重启恢复记录缺口据 run_input 称已由平台修复；本节点自身未见证平台侧修复过程，标记为 [assumption]（来源：run_input 陈述），本轮团队侧保持完整可追溯记录（本文件及后续各节点产物）。

---

## 2. 决策日志

| 编号 | 决策 | 理由 | 事实标签 |
|---|---|---|---|
| DEC-001 | 冻结 `corona-baseline-1.0.0`，速度情景锁定为单一 0.03c | run_input 明确批准范围为单一 0.03c、baseline 1.0.0；与任务书冲突时以本轮批准范围为初始基线 | [verified_fact] |
| DEC-002 | 三速度要求（0.01c/0.05c）不预先纳入基线，登记为 CCR-001 待平台确认 | run_input：历史对话及任务书中的三速度要求仅作为待确认变更；只有平台正式确认的纠偏才可改变速度范围 | [verified_fact] |
| DEC-003 | 将 acceptance.json 硬门 `three_speed_scenarios_present` 与批准范围的冲突登记为 DISC-001 并上报平台裁定 | 团队无权改写验收准则，亦无权擅自扩展基线；掩盖冲突违反事实纪律 | [verified_fact] |
| DEC-004 | 唯一机器可读追溯键 = (baseline_id, baseline_version, content_digest)，强制嵌入全部下游产物 | 确保所有最终成果来自同一参数模型 | [verified_fact] |

---

## 3. 变更申请登记

### CCR-001 速度情景扩展：0.01c / 0.03c / 0.05c 三情景

| 项 | 内容 |
|---|---|
| 状态 | **pending_platform_confirmation（待平台正式确认）** |
| 来源 | 任务书 §5 受控变更条款；acceptance.json 硬门 `three_speed_scenarios_present`；任务书 §4.3 应用三情景比较要求 |
| 当前是否在基线内 | **否**（baseline 1.0.0 仅含 0.03c）[verified_fact] |
| 变更内容 | 巡航速度情景由 [0.03] 扩展为 [0.01, 0.03, 0.05]，并同步所有产物 |

**安全点（若获批）**: 冻结基线 1.0.0 全套产物（baseline_frozen.yaml 及其全部派生物）原样保留，基线升级为 1.1.0（或平台指定版本号），新摘要重新计算并公示。

**影响范围（预评估）[derived_result]**:

| 产物域 | 影响 | 重算/保留 |
|---|---|---|
| outputs/model/ | 情景参数、航行时间、动能等全部速度相关计算 | 速度相关函数重跑三情景；与速度无关的（人工重力等）保留 |
| outputs/drawings/ | 图纸注记中的速度/时间/能量数值 | 含速度数值的注记重算重注；纯结构图保留 |
| outputs/app/ | 情景比较 UI、可调参数、导出 JSON | 增加三情景比较能力；公式与模型同源不变 |
| outputs/review/ | 三路线比较、敏感性、需求基线数值 | 速度相关结论重算；定性成立条件大部保留 |
| outputs/verification/ | 验证矩阵、容差比对 | 矩阵新增三情景行并重跑 |
| acceptance.json | 硬门 `three_speed_scenarios_present` 由不通过转为可验证 | 准则本身由平台管理，团队不改写 |

**旧单情景残留清理要求（若获批）**: 全库检索 0.03c 唯一情景表述，逐一替换或标注；验证阶段须含“旧单情景结论无残留”专项检查（对应任务书 §5 "旧的单情景结论不得残留"）。

**确认路径**: 仅平台正式纠偏确认可批准本变更；团队成员（含本节点）无权自行批准。[verified_fact]

---

## 4. 已知差异与上报项

| 编号 | 内容 | 处置 | 事实标签 |
|---|---|---|---|
| DISC-001 | acceptance.json 硬门 `three_speed_scenarios_present` 与批准的单一 0.03c 锁定冲突；CCR-001 确认前该硬门按字面无法通过 | 已上报平台裁定（escalated_to_platform）；团队不擅自改写验收准则，也不擅自扩展基线 | [verified_fact] |
| DISC-002 | 任务书 §4.3 三情景应用要求受 CCR-001 约束；当前基线下应用仅实现 0.03c，接口按变更影响预留 | 待 CCR-001 | [verified_fact] |
| DISC-003 | OpenSCAD 安装就绪状态本节点未检查（属图纸/总装职责） | 已列入下游工作简报的待检查项；若未就绪须如实记录"待检查"，不得伪造通过 | [unknown] |

---

## 5. 变更控制程序（本轮生效）

1. 任何对 `baseline_frozen.yaml` 的修改必须以 CCR 形式提出，登记于本文件。
2. 速度范围的变更仅接受平台正式确认的纠偏作为批准来源。
3. 变更获批后：公示安全点与影响范围 → 平台确认 → 基线版本号递增并重新计算 content_digest → 全产物同步 → 验证阶段专项检查无旧结论残留。
4. 基线文件的任何版本均不得删除或覆盖；新版本以新文件/新版本号并存。
