# 接口控制文件（Interface Control Document, ICD-1.0.0）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **职责**: luna-mission-lead（接口管理）
- **状态**: 已冻结，随基线 1.0.0 生效 [verified_fact]
- **目的**: 确保模型、图纸、应用、论证包、验证包全部来自同一参数模型，接口一致、可追溯、可机械检查。

---

## 1. 单一参数源（Single Source of Truth）

| 接口ID | 规则 |
|---|---|
| ICD-SOT-001 | `outputs/model/baseline_frozen.yaml` 是唯一参数源。任何产物不得硬编码与之冲突的常数、工况或情景。 |
| ICD-SOT-002 | 模型实现源码必须从 `baseline_frozen.yaml`（或其机器可读副本）读取参数；应用必须与模型使用同一公式与参数（建议直接复用模型计算内核或由其生成参数 JSON）。 |
| ICD-SOT-003 | 每个产物文件（SVG 注释/元数据、应用导出 JSON、文档页眉）必须嵌入追溯键三元组：`(baseline_id, baseline_version, content_digest)`。缺少或值不符即不符合项。 |

## 2. 规范参数命名与单位（跨产物一致）

| 规范名 | 单位 | 含义 | 当前基线值/域 | 事实标签 |
|---|---|---|---|---|
| `cruise_speed_c` | 无量纲（×c） | 巡航速度 | 0.03（锁定单值） | [verified_fact] |
| `speed_of_light_m_s` | m/s | 光速 | 299792458 | [verified_fact] |
| `standard_gravity_m_s2` | m/s² | 标准重力 | 9.80665 | [verified_fact] |
| `proxima_distance_ly` | ly | 比邻星距离 | 4.25 | [verified_fact] |
| `mass_kg` | kg | 质量工况（如 crewed_1mt = 1e9，orbital_material_5mt = 5e9） | 见 reference_cases | [verified_fact] |
| `relative_speed_c` | 无量纲（×c） | 相对撞击速度（尘埃） | 0.03 | [verified_fact] |
| `radius_m` / `rotation_rpm` | m / rpm | 人工重力半径与转速 | 1000 / 2（参考工况） | [verified_fact] |
| `travel_time_yr` | yr | 航行时间（导出量） | 141.6667 @0.03c | [derived_result] |
| `kinetic_energy_j` | J | 动能下限（导出量，非相对论；相对论对照单列） | 见 reference_checks | [derived_result] |
| `artificial_gravity_m_s2` | m/s² | 向心加速度（导出量） | 43.8649 @1km/2rpm | [derived_result] |
| `dust_impact_energy_j` | J | 尘埃撞击动能（导出量） | 40443983 @1mg/0.03c | [derived_result] |

## 3. 规范公式接口（模型↔应用↔图纸注记必须一致）

| 接口ID | 公式 | 说明 |
|---|---|---|
| ICD-FML-001 | `travel_time_yr = proxima_distance_ly / cruise_speed_c`（匀速、不含加减速，须显式标注此边界） | 航行时间 |
| ICD-FML-002 | `kinetic_energy_j = 0.5 * mass_kg * (cruise_speed_c * c)^2`；相对论对照 `(γ−1)mc²` 单列且不得混用 | 动能下限；**不得**表述为完整推进能源预算 |
| ICD-FML-003 | `artificial_gravity_m_s2 = (2π * rotation_rpm / 60)^2 * radius_m` | 人工重力 |
| ICD-FML-004 | `dust_impact_energy_j = 0.5 * dust_mass_kg * (relative_speed_c * c)^2` | 尘埃撞击；TNT 当量换算（1 kg TNT = 4.184e6 J）如使用须标注来源为常用换算约定 [assumption] |
| ICD-FML-005 | 转向上限：`turn_angle_deg ≈ Δv/v`（小角近似，弧度转度）；10%–15% Δv → 约 5.7°–8.6° | 目标几何机动 |

## 4. 路线间接口（可分离性）

| 接口ID | 规则 |
|---|---|
| ICD-RTE-001 | 三路线产物分别标识 `route_id`（laser_sail_precursor / uncrewed_civilization_archive / crewed_interstellar_vehicle），跨路线共享结果必须注明来源路线。 |
| ICD-RTE-002 | 禁止跨路线可行性外推（RQ-SCP-003）；路线比较表中每条路线的成立条件/终止条件独立成栏。 |
| ICD-RTE-003 | 先锋探测器路线使用其自身 0.2c 飞行参数（纪要 §二.6），该参数属路线内部工况，不构成巡航情景扩展，与 0.03c 锁定不冲突。 |

## 5. 产物域间接口（交付与集成规则）

| 接口ID | 规则 |
|---|---|
| ICD-DLV-001 | 下游成员必须通过平台传递的上游输入及来源清单接力，**复制并整合**选定的本轮当前版本到自己的 `outputs/` 后再交付；不得扫描同机其他成员工作目录；不得使用旧运行文件。 |
| ICD-DLV-002 | 模型 → 图纸：图纸数值注记只能引用模型输出或基线文件；图纸附参数追踪表。 |
| ICD-DLV-003 | 模型 → 应用：应用计算与模型同公式同参数（RQ-APP-006）；导出 JSON 含追溯键与全部输入/输出。 |
| ICD-DLV-004 | 模型/图纸/应用 → 论证包：论证包数值引用须注明出处文件与追溯键。 |
| ICD-DLV-005 | 全部 → 验证包：验证矩阵按 RQ 编号双向追踪；每项检查保存有界命令及原始输出；未执行项标 NOT_RUN，不得宣称 PASS。 |
| ICD-DLV-006 | 阶段产物与最终产物可区分：阶段产物标注 `stage:` 前缀或阶段目录，最终产物具有唯一当前版本。 |

## 6. 变更对接口的影响

- CCR-001（三情景）若获批：`cruise_speed_c` 由单值扩展为 `[0.01, 0.03, 0.05]`，ICD 第 2/3 节域值同步更新，接口结构（命名、公式、追溯键）不变 —— 下游应按此预留情景枚举接口，但**当前不得实现或展示** 0.01c/0.05c 情景。
- 接口变更随基线版本递增重新冻结，旧版本接口文件保留不覆盖。
