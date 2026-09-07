# 任务需求基线（Requirements Baseline 1.0.0）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **状态**: 已冻结（frozen），变更须经 CCR 程序（见 change_control_register.md）
- **范围**: Level-0 / Pre-Phase A 概念级先期论证；不声称施工级、制造级或飞行认证级设计 [verified_fact]
- **批准速度情景**: 单一 0.03c（三速度为 CCR-001 待确认变更）

> 每条需求标注事实标签与验证方法。验证方法代码：TST=自动测试，ANL=分析/复算，INSP=检查，DEMO=应用/图纸操作演示。

---

## 1. 范围与边界需求

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-SCP-001 | 本轮基线巡航速度情景为单一 0.03c，任何产物不得预先包含 0.01c/0.05c 情景 | run_input | INSP（全产物检索） | [verified_fact] |
| RQ-SCP-002 | 比较三条可分离路线：激光光帆先锋探测器、无人文明档案载荷、载人星际飞行器 | 任务书 §2 / baseline.yaml routes | INSP | [verified_fact] |
| RQ-SCP-003 | 三路线可分离：不得把克级光帆探测器可行性外推为大型无人载荷或载人飞行器可行性 | 纪要 §一 / 任务书 §3 | INSP | [verified_fact] |
| RQ-SCP-004 | 明确区分"飞离太阳系"与"抵达目标恒星系统"两种任务终态表述 | 纪要 §二.1 | INSP | [verified_fact] |
| RQ-SCP-005 | 成果仅概念级：禁止出现 construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed 四类声明 | baseline.yaml forbidden_claims | INSP（关键词检索） | [verified_fact] |

## 2. 统一参数模型需求（outputs/model/）

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-MDL-001 | 模型全部参数派生自 `baseline_frozen.yaml`，并嵌入追溯键 | 本节点 DEC-004 | TST + INSP | [verified_fact] |
| RQ-MDL-002 | 计算航行时间与光行时：0.03c 下 4.25 ly 单程 ≈ 141.6667 年（容差 0.1%） | acceptance.json | TST | [verified_fact] |
| RQ-MDL-003 | 动能下限（非相对论，并给出相对论对照）：1Mt@0.03c ≈ 4.0443983e22 J；5Mt@0.03c ≈ 2.0221991e23 J（容差 1%） | acceptance.json / 纪要 §二.2 | TST | [verified_fact] |
| RQ-MDL-004 | 动能下限不得表述为完整推进能源预算（未计效率、排气动能、推进剂、减速、备用、建造损耗） | 纪要 §二.2 / 任务书 §3 | INSP | [verified_fact] |
| RQ-MDL-005 | 人工重力：r=1km、2rpm → a ≈ 43.8649 m/s²（容差 1%）；1g 同半径转速 ≈ 0.95 rpm | acceptance.json / 纪要 §二.3 | TST | [verified_fact] |
| RQ-MDL-006 | 尘埃撞击：1mg@0.03c ≈ 4.0443983e7 J（容差 1%，约 9.7 kg TNT 当量）；旧稿"1mg≈450MJ"已撤回不得复现 | acceptance.json / 纪要 §二.4 | TST + INSP | [verified_fact] |
| RQ-MDL-007 | 目标几何与转向上限：比邻星–巴纳德星 ≈78°、比邻星–鲸鱼座τ ≈101°、巴纳德星–鲸鱼座τ ≈117°；10%–15% Δv 仅支持约 5.7°–8.6° 转向；中途切换三目标不可行 | 纪要 §二.5 | ANL | [verified_fact]（角度值源自纪要，标记为其所载公开坐标计算结果） |
| RQ-MDL-008 | 先锋探测器回传：0.2c 飞比邻星最低约 21.2 年 + 回传约 4.25 年，最早回传约 25.5 年；先锋数据不得作为第 20 年以前母舰决策条件 | 纪要 §二.6 | ANL | [verified_fact]（0.2c 为探测器路线自身参数，非本轮巡航情景扩展） |
| RQ-MDL-009 | 加速/减速/效率/质量情景与三路线敏感性比较 | 任务书 §4.1 | TST | [verified_fact] |
| RQ-MDL-010 | 关键数值必须带单位、公式、输入、输出和来源标识 | 任务书 §3 | INSP | [verified_fact] |
| RQ-MDL-011 | 模型含可运行源码与自动测试 | 任务书 §4.1 | TST | [verified_fact] |

## 3. 概念图纸需求（outputs/drawings/）

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-DRW-001 | 三路线任务架构总图 SVG | 任务书 §4.2 | INSP + XML 解析 | [verified_fact] |
| RQ-DRW-002 | 无人文明档案载荷功能分区图 SVG | 任务书 §4.2 | INSP + XML 解析 | [verified_fact] |
| RQ-DRW-003 | 推进、能源、热控、通信与档案接口图 SVG（接口定义见 interface_control.md） | 任务书 §4.2 | INSP + XML 解析 | [verified_fact] |
| RQ-DRW-004 | 一份可编辑 OpenSCAD 或 DXF 概念模型；OpenSCAD 未就绪时如实记录待检查，不得伪造通过 | 任务书 §4.2 / run_input | DEMO 或记录待检查 | [verified_fact] |
| RQ-DRW-005 | 全部图纸标注图号、版本、单位、假设、参数追踪表，并显式标注"概念级" | 任务书 §4.2 / §6.2 | INSP | [verified_fact] |
| RQ-DRW-006 | 图纸中全部数值注记与基线一致并嵌入追溯键 | DEC-004 | TST（跨文件一致性） | [verified_fact] |

## 4. 数字决策应用需求（outputs/app/）

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-APP-001 | 本地离线运行，无云服务依赖 | 任务书 §4.3 / acceptance.json | DEMO | [verified_fact] |
| RQ-APP-002 | 当前基线下实现 0.03c 情景；允许调整质量、推进效率、人工重力半径和转速 | run_input / 任务书 §4.3 | DEMO | [verified_fact] |
| RQ-APP-003 | 实时显示航行时间、动能下限、等效能量、人工重力、尘埃撞击能量 | 任务书 §4.3 | DEMO | [verified_fact] |
| RQ-APP-004 | 显示三路线证据状态、成立条件、终止条件和主要未知项 | 任务书 §4.3 | DEMO | [verified_fact] |
| RQ-APP-005 | 能打开概念图并导出当前情景 JSON | 任务书 §4.3 / acceptance.json | DEMO | [verified_fact] |
| RQ-APP-006 | 全部计算与统一模型使用相同公式和参数（同一基线） | 任务书 §4.3 | TST（与应用导出值比对模型） | [verified_fact] |
| RQ-APP-007 | 三情景（0.01/0.03/0.05c）比较能力仅作 CCR-001 预留接口，当前不实现、不展示 | DEC-002 / DISC-002 | INSP | [verified_fact] |

## 5. 论证包需求（outputs/review/）

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-REV-001 | 每条关键陈述使用 verified_fact / derived_result / assumption / unknown 标签 | 任务书 §3 / baseline.yaml | INSP | [verified_fact] |
| RQ-REV-002 | 三路线比较：任务终态、成立条件、关键缺口、时间与能源数量级、成本研究方法、终止条件、下一阶段课题包 | 任务书 §2 | INSP | [verified_fact] |
| RQ-REV-003 | 假设、证据和未知项登记册；缺少直接证据项（材料性能、尘埃通量、推进效率、退款时间等）显式保留未知项 | 任务书 §3 / §4.4 | INSP | [verified_fact] |
| RQ-REV-004 | 技术成熟度、风险、成本研究与 WBS；成本仅为研究方法，不形成整项工程承诺成本 | 任务书 §4.4 / 纪要 §五 | INSP | [verified_fact] |
| RQ-REV-005 | 收录独立否定意见与未关闭问题；严重问题未清零不得判通过 | 任务书 §4.4 / §6.2 / acceptance.json | INSP | [verified_fact] |
| RQ-REV-006 | 不以 5000 人冬眠为质量/补给估算基线；冬眠为机会技术单列 | 纪要 §三.3 | INSP | [verified_fact] |
| RQ-REV-007 | 原 2.4–3.4 万亿元总投资与已撤回经费结构不得作为报批依据复现 | 纪要 §五 | INSP | [verified_fact] |
| RQ-REV-008 | 原第 8/20/35/40 年决策门仅作远期假设，不作当前承诺；阶段决策门采用 6/12/18–24 月 | 纪要 §六 | INSP | [verified_fact] |

## 6. 验证包与最终验收需求（outputs/verification/、outputs/FINAL_ACCEPTANCE.md）

| 需求ID | 需求陈述 | 来源 | 验证方法 | 事实标签 |
|---|---|---|---|---|
| RQ-VER-001 | 需求验证矩阵覆盖本基线全部需求，双向可追踪 | 任务书 §4.5 / §6.2 | INSP | [verified_fact] |
| RQ-VER-002 | 最终总装员实际运行：模型测试、应用启动与导出、图纸解析与渲染、跨文件一致性、参考值容差、事实标签、严重问题清零；每项保存有界命令及原始结果，未执行项不得宣称 PASS | run_input | TST/DEMO 记录审查 | [verified_fact] |
| RQ-VER-003 | 参考值容差比对以 acceptance.json reference_checks 为准（当前全部为 0.03c 工况，与基线锁定一致） | acceptance.json | TST | [verified_fact] |
| RQ-VER-004 | 硬门 `three_speed_scenarios_present` 在 CCR-001 确认前按 DISC-001 如实记录为不适用/受阻，不得伪造通过 | DEC-003 | INSP | [verified_fact] |
| RQ-VER-005 | 验收准则差异（DISC-001/002）须在 FINAL_ACCEPTANCE.md 中显式披露，不得用综合表述掩盖 | 任务书 §7 | INSP | [verified_fact] |

---

## 7. 基线统计

- 需求总数：42（范围5 / 模型11 / 图纸6 / 应用7 / 论证8 / 验证5）
- 全部需求事实标签：[verified_fact]（需求存在性源自已读真实输入或本节点已记录决策）；需求数值结论的实测验证状态在验证阶段前为 [unknown]，由验证包回填。
