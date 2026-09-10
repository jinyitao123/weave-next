# 日冕计划 Pre-Phase A · v6 · 数字工程节点（digital-engineering）交付与复用声明

**节点**：`corona-prephase-a-recovery-v6-digital-engineering-builder`
**日期**：2026-09-10
**本轮冻结上游**：只读冻结源 `recovery-materials/2026-09-10-corona-v6-formal-r1-source/`（manifest SHA-256 `349726a3e30426e48eb21f0bec303198cf2ca5c0948074a98a0c997634a93015`）
**交付范围**：统一计算模型（model/）+ 数字应用（app/）+ 概念图纸（drawings/）+ 机器验证清单（verification/digital_engineering_selfcheck*）
**职责**：优先逐字节复用冻结 v6 数字成果，仅在出现可复现缺陷时修改。上游冻结件本身源自更早的 `corona-v6-corrected-source` 基线（manifest `59c911db588fd8c1bce47f9d713fa1174c4b7109e534d75c719c2b63717aa03f`），本轮按 D-1 原样继承，不重复记录其来源细节。

## 1. 复用/修改自我声明

- **策略**：原样复用（copy reuse），**未做任何字节级修改**。本轮**零修改**（未发现需在本节点修复的可复现缺陷）。
- 全部 `model/`、`app/`、`drawings/` 交付件及两个自检脚本从冻结源按原样复制，`SHA-256` 与冻结源一致（见 §3 核对表）。
- 本轮**重新实际运行**了自检（动态计数、有界退出），结果见 §4 与本节点 `outputs/verification/digital_engineering_selfcheck_output.txt`。

## 2. 交付件清单（相对本节点 `outputs/`）

| 类别 | 相对路径 | 类型 | 说明 |
|---|---|---|---|
| model | `model/corona_model.py` | Python（统一计算内核） | 与冻结源一致 |
| model | `model/params.json` | JSON（机器可读参数） | 与冻结源一致 |
| model | `model/baseline.yaml` | YAML（冻结基线自包含副本） | 与冻结源一致 |
| model | `model/mini_yaml.py` | Python（无依赖 YAML 子集解析） | 与冻结源一致 |
| model | `model/anchors_reverified_2026-09-09.txt` | 文本（0.03c 参考锚点证据） | 与冻结源一致 |
| model | `model/README.md` | Markdown（模型说明） | 与冻结源一致 |
| model | `model/tests/__init__.py`、`model/tests/test_model.py` | Python（模型测试） | 与冻结源一致 |
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
| 自检输出 | `verification/digital_engineering_selfcheck_output.txt` | 文本 | **本轮实际重跑结果** |

> 说明：本节点交付范围含统一模型/应用/图纸/自检清单；`review/`、`verification/acceptance.json`、`browser-evidence.json`、`vi_run_gates*`、`FINAL_ACCEPTANCE.md` 均属其他节点职责，本节点不交付、不代做判定。

## 3. SHA-256 核对表（冻结源 vs 本节点交付件）

| 相对路径 | 冻结源 SHA-256 | 本节点 SHA-256 | 一致 |
|---|---|---|---|
| `model/corona_model.py` | `787029bb740f432c…` | `787029bb740f432c…` | ✅ |
| `model/params.json` | `4d1ff4bd2d45e4f6…` | `4d1ff4bd2d45e4f6…` | ✅ |
| `model/baseline.yaml` | `09351b046422984b…` | `09351b046422984b…` | ✅ |
| `model/mini_yaml.py` | `347bee291f9039ca…` | `347bee291f9039ca…` | ✅ |
| `model/anchors_reverified_2026-09-09.txt` | `1fcf10137d6fc3a1…` | `1fcf10137d6fc3a1…` | ✅ |
| `app/app.js` | `8ddab7d3e0beaeb6…` | `8ddab7d3e0beaeb6…` | ✅ |
| `app/index.html` | `1ee4e7476c441485…` | `1ee4e7476c441485…` | ✅ |
| `app/model.js` | `3dda05bd13fe1704…` | `3dda05bd13fe1704…` | ✅ |
| `app/params.js` | `751a06c6f54698a2…` | `751a06c6f54698a2…` | ✅ |
| `drawings/FA-01_laser_sail_precursor.svg` | `b67f37efb3c5d790…` | `b67f37efb3c5d790…` | ✅ |
| `drawings/FB-01_uncrewed_archive_probe.svg` | `53f47172a62e2a03…` | `53f47172a62e2a03…` | ✅ |
| `drawings/FC-01_crewed_interstellar_vehicle.svg` | `da9ac7c419e896a2…` | `da9ac7c419e896a2…` | ✅ |
| `drawings/FD-01_speed_axis_scenario_comparison.svg` | `8b4401537db1e664…` | `8b4401537db1e664…` | ✅ |
| `drawings/RH-01_rotating_habitat.scad` | `a179e099db917529…` | `a179e099db917529…` | ✅ |
| `verification/digital_engineering_selfcheck.js` | `c235a2bd59132ea2…` | `c235a2bd59132ea2…` | ✅ |
| `verification/digital_engineering_selfcheck.py` | `55b3f689b79db4d6…` | `55b3f689b79db4d6…` | ✅ |

> 本轮实际逐件 `shasum -a 256` 比对，全部一致。若任何核查失败，则本语句失效。

## 4. 本轮运行的自检（有界退出、动态计数）

本轮**实际执行**以下自检，退出码均为 0（有界退出），计数由运行时动态得出，未硬编码：

- `node outputs/verification/digital_engineering_selfcheck.js` → **exit 0**，应用侧：动态计数共 **31** 项、**31 通过 / 0 失败**。覆盖 G2（三速度情景 0.01c/0.03c/0.05c）、G3（5 个参考锚点全部容差内）、G4（无外网/云依赖）、G5（`AppAPI.exportScenarioJSON()` 真实导出路径）、FD-01 面板 B 一致性。
- `python3 outputs/verification/digital_engineering_selfcheck.py` → **exit 0**，图纸侧：扫描动态计数 **SVG=4 / SCAD=1**，共 **13 项**、**13 通过 / 0 失败**。覆盖 G6（4 张 SVG 均解析为合法 `<svg>` 根且含 viewBox；SCAD 含可编辑参数）、G7（全部图纸标注概念级）、FD-01 修正脚注（位于第 63 行）。
- `python3 -m unittest tests.test_model`（model 目录）→ **exit 0**，**26 tests OK**。统一计算内核功能正常（常数/情景/锚点/派生/一致性/事实纪律）。
- `python3 corona_model.py` → **exit 0**，三情景全派生量打印正常，0.03c 五项参考锚点复算 `ALL_OK = True`（max rel_err 2.58e-8）。

运行日志（动态计数、有界）见 `outputs/verification/digital_engineering_selfcheck_output.txt`。

### 覆盖验收门槛（本节点域内）
- **G2** 三速度情景存在：应用侧动态计数 3。
- **G3** 参考计算在容差内：5 个 reference 锚点全部 PASS。
- **G4** 无外网/云依赖：扫描交付 `app/` 无 http/https/fetch/XHR。
- **G5** 应用导出情景 JSON：`AppAPI.exportScenarioJSON()` 真实路径通过。
- **G6** 图纸可解析且可编辑：4 张 SVG 合法 XML；1 份 SCAD 可编辑脚本。
- **G7** 全部图纸标注概念级（含「不可用于施工/制造/飞行认证」措辞）。
- **FD-01 修正脚注保留**：`outputs/drawings/FD-01_speed_axis_scenario_comparison.svg` 第 63 行，文本「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」。

## 5. 与其他节点的衔接（非本节点交付）

- `app/index.html` 页脚含指向 `../model/`（统一模型）与 `../verification/`（总装校验，verification-integrator）的相对链接；这两个资源由对应节点在最终总装目录提供。在本节点 `outputs/` 内，`../drawings/` 链接可正常解析（app 与 drawings 同级交付）。
- `review/`、G1–G11 整体验收、真实浏览器三速切换与导出、`verification/acceptance.json`/`browser-evidence.json`/`vi_run_gates*`/`FINAL_ACCEPTANCE.md` 均属 `verification-integrator` 职责，本节点不代做。

## 6. 诚实边界

- 本节点**已**在本轮实际运行本域自检（应用侧/图纸侧/模型测试/计算内核），结果见 §4；但**未**执行 G1–G11 整体验收、**未**做浏览器三速切换/导出、**未**创建 `outputs/review/REVIEW_INDEX.md`、**未**生成 `verification/acceptance.json`/`browser-evidence.json`/`vi_run_gates_output.txt`/`FINAL_ACCEPTANCE.md` —— 上述属 `verification-integrator` 职责。
- **未发现**需要本节点修复的可复现缺陷；如后续确认本域存在缺陷而未在本节记录，则本节点复用状态需复核。

### 观察项（非缺陷，未修改）
- `app/index.html` 第 6 行 `<title>` 含陈旧亲源标签「（v4）」，而同一页头（第 60–61 行）、`params.js`（`corona-prephase-a-v1` / `v1.0.0`）及全部图纸（`corona-prephase-a-v1 · v1.0.0`）均为一致版本标签。
- 依据汇总决策 **D-1（原样复用冻结 v6，仅在可复现缺陷时修改）**，该标签为浏览器标题栏的**装饰性字符串**，**不属功能性参数/量测/声明**，与本节点硬门槛判定无关，故本节点**未就地修改**，保持冻结件字节一致。
- 是否将其纳入 G10 跨产物一致性判定，由 `verification-integrator` 在总装时据实评估；若判定为应改，应由 lead 依变更控制决策，本节点不自行跨域改动。

### 观察项（应用页脚外链, 非缺陷，未修改——需总装/lead 评估）
- `app/index.html` 第 132–133 行页脚链接指向 `../verification/machine_verification_checklist.md` 与 `../verification/verification_report.md`。
- 经在冻结 `formal-r1-source` 交付及 `manifest.json` 全量核对，**这两份文件未被收集**，亦不在本轮任何节点的「本轮产出清单」内（本轮 `verification-integrator` 重生成四项为 `acceptance.json`/`browser-evidence.json`/`vi_run_gates_output.txt`/`FINAL_ACCEPTANCE.md`）。本节点的机器验证清单为 `verification/digital_engineering_selfcheck_output.txt`（本轮实际重跑，动态计数）。
- 可能为 v4 旧命名遗留。鉴于其为**装饰性页脚链接**且修改需 lead 变更控制，本节点**未就地修改**、**未伪造缺失文件**，如实上报供 `verification-integrator`/lead 在总装时评估（判定应改则由 lead 决策，或由对应节点补齐，本节点不跨域代造）。

## 7. 本节点本轮真实工具调用证据（关键）

| 检查项 | 结果 |
|---|---|
| 冻结源 `formal-r1-source/manifest.json` SHA-256 = `349726a3…` | ✅ 与 run_input 一致 |
| 自检运行 `digital_engineering_selfcheck.js` | exit 0，31/31 PASS（动态） |
| 自检运行 `digital_engineering_selfcheck.py` | exit 0，13/13 PASS（动态） |
| 模型单测 `python3 -m unittest tests.test_model` | exit 0，26 tests OK |
| 计算内核 `python3 corona_model.py` | exit 0，ALL_OK=True |
| `outputs/verification/digital_engineering_selfcheck_output.txt` | 本轮由真实执行重新生成 |
