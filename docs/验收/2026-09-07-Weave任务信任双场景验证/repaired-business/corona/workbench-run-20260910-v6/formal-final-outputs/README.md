# 日冕计划 Pre-Phase A · v6 · 数字工程节点（digital-engineering）交付与复用声明

**节点**：`corona-prephase-a-recovery-v6-digital-engineering-builder`
**日期**：2026-09-10
**上游基线**：只读冻结源 `recovery-materials/2026-09-10-corona-v6-corrected-source/`（manifest SHA-256 `59c911db588fd8c1bce47f9d713fa1174c4b7109e534d75c719c2b63717aa03f`）
**职责**：数字应用（app/）+ 概念图纸（drawings/）—— 复核并原样复用冻结 v6，仅在可复现缺陷时修改。

## 1. 复用/修改自我声明

- **策略**：原样复用（copy reuse），**未做任何字节级修改**。
- 全部 `app/` 与 `drawings/` 件从冻结源按原样复制，`SHA-256` 与冻结源一致（见 §3 核对表）。
- 未发现需在本节点修复的**可复现缺陷**；因此本节点本轮**零修改**。

## 2. 交付件清单（相对本节点 `outputs/`）

| 类别 | 相对路径 | 类型 | 说明 |
|---|---|---|---|
| app | `app/app.js` | JS | 与冻结源一致 |
| app | `app/index.html` | HTML（应用入口） | 与冻结源一致 |
| app | `app/model.js` | JS | 与冻结源一致 |
| app | `app/params.js` | JS | 与冻结源一致 |
| drawings | `drawings/FA-01_laser_sail_precursor.svg` | SVG（可编辑源） | 与冻结源一致 |
| drawings | `drawings/FB-01_uncrewed_archive_probe.svg` | SVG（可编辑源） | 与冻结源一致 |
| drawings | `drawings/FC-01_crewed_interstellar_vehicle.svg` | SVG（可编辑源） | 与冻结源一致 |
| drawings | `drawings/FD-01_speed_axis_scenario_comparison.svg` | SVG（可编辑源） | 与冻结源一致 |
| drawings | `drawings/RH-01_rotating_habitat.scad` | OpenSCAD（可编辑源） | 与冻结源一致 |
| 自检脚本 | `verification/digital_engineering_selfcheck.js` | JS | 应用侧自检（G2/G3/G4/G5） |
| 自检脚本 | `verification/digital_engineering_selfcheck.py` | Python | 图纸侧自检（G6/G7/FD-01） |
| 自检输出 | `verification/digital_engineering_selfcheck_output.txt` | 文本 | 本轮实际运行结果 |

## 3. SHA-256 核对表（冻结源 vs 本节点交付件）

| 相对路径 | 冻结源 SHA-256 | 本节点 SHA-256 | 一致 |
|---|---|---|---|
| `app/app.js` | `8ddab7d3e0beaeb6…` | `8ddab7d3e0beaeb6…` | ✅ |
| `app/index.html` | `1ee4e7476c441485…` | `1ee4e7476c441485…` | ✅ |
| `app/model.js` | `3dda05bd13fe1704…` | `3dda05bd13fe1704…` | ✅ |
| `app/params.js` | `751a06c6f54698a2…` | `751a06c6f54698a2…` | ✅ |
| `drawings/FA-01_laser_sail_precursor.svg` | `b67f37efb3c5d790…` | `b67f37efb3c5d790…` | ✅ |
| `drawings/FB-01_uncrewed_archive_probe.svg` | `53f47172a62e2a03…` | `53f47172a62e2a03…` | ✅ |
| `drawings/FC-01_crewed_interstellar_vehicle.svg` | `da9ac7c419e896a2…` | `da9ac7c419e896a2…` | ✅ |
| `drawings/FD-01_speed_axis_scenario_comparison.svg` | `8b4401537db1e664…` | `8b4401537db1e664…` | ✅ |
| `drawings/RH-01_rotating_habitat.scad` | `a179e099db917529…` | `a179e099db917529…` | ✅ |

> 逐件 SHA-256 全部一致，共 9 个 app/drawings 交付件。若任何核查失败，则本语句失效。

## 4. 本节点运行的自检（有界退出、动态计数）

运行：`node outputs/verification/digital_engineering_selfcheck.js` 与 `python3 outputs/verification/digital_engineering_selfcheck.py`。

- 检查计数在**运行时**由实际对象/文件扫描动态得出（非硬编码）。
- 失败即 `exit 1`，**有界退出**；本轮实际结果：见 `outputs/verification/digital_engineering_selfcheck_output.txt`。

### 应用侧（app/）—— 覆盖 G2/G3/G4/G5
- G2 三速度情景存在（0.01c/0.03c/0.05c）：动态计数 3。
- G3 参考计算在容差内（5 个 reference 锚点）：全部 PASS。
- G4 无外网/云依赖（扫描交付 app/ 无 http/https/fetch/XHR）。
- G5 应用导出情景 JSON：`AppAPI.exportScenarioJSON()` 真实路径通过。

### 图纸侧（drawings/）—— 覆盖 G6/G7 + FD-01
- G6 全部 SVG 解析为合法 XML（4 张）；SCAD 为可编辑脚本（1 份）。
- G7 全部图纸标注概念级（含「不可用于施工/制造/飞行认证」措辞）。
- **FD-01 修正脚注保留**：`outputs/drawings/FD-01_speed_axis_scenario_comparison.svg` 第 63 行，
  文本「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」。

## 5. 与其他节点的衔接（非本节点交付）

`app/index.html` 脚注含指向 `../model/`（统一模型，model-owner）与 `../verification/`（总装校验，verification-integrator）的相对链接；
这两类资源**不属于本节点交付范围**，由对应节点在最终总装目录中提供。在本节点 `outputs/` 内，`../drawings/` 链接可正常解析（app 与 drawings 为同级交付）。

## 6. 诚实边界

- 本节点**未**运行 G1–G11 整体验收、**未**做浏览器三速切换/导出、**未**创建 `outputs/review/REVIEW_INDEX.md`，上述属 verification-integrator 职责；本节点只对本域 app/ + drawings/ 复用与自检负责。
- 未发现需要本节点修复的可复现缺陷；如发现域内缺陷，本节点按冻结基线修复并在本节记录。若后续确认存在缺陷而本节未列，则本节点复用状态需复核。

### 观察项（非缺陷，未修改）
- `app/index.html` 第 6 行 `<title>` 含陈旧亲源标签「（v4）」，而同一页头（第 61 行）、`params.js`（`corona-prephase-a-v1` / `v1.0.0`）及全部图纸（`corona-prephase-a-v1 · v1.0.0`）均为一致版本标签。
- 依据汇总决策 **D-1（原样复用冻结 v6，仅在可复现缺陷时修改）**，该标签为浏览器标题栏的装饰性字符串，**不属功能性参数/量测/声明**，与本节点硬门槛判定无关，故本节点**未就地修改**，保持冻结件字节一致。
- 是否将其纳入 G10 跨产物一致性判定，由 verification-integrator 在总装时据实评估；若判定为应改，应由 lead 依变更控制决策，本节点不自行跨域改动。
