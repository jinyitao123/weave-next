# 机器验证日志（数字工程节点）

- **追溯键**: baseline_id = `corona-baseline-1.0.0` · baseline_version = `1.0.0` · content_digest = `cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2`
- **执行日期**: 2026-09-07 · 执行者: luna-digital-engineering-builder（本节点）
- **范围**: 本节点交付物（模型/图纸/应用）的机器可执行检查；全部命令有界、自行结束、无网络、无前台服务器。
- **纪律**: 未执行项列入 `member_acceptance.json` 的 `not_run`，不宣称 PASS。

---

## V1 · 基线完整性与参数派生

**命令**: `shasum -a 256 outputs/model/baseline_frozen.yaml`
**原始结果**:

```
cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2  outputs/model/baseline_frozen.yaml
```

**判定**: PASS — 与 `inputs/manifest.json` 及上游冻结摘要逐字节一致（复制整合自 `inputs/lead/model/baseline_frozen.yaml`，ICD-DLV-001）。

派生命令 `ruby derive_params.rb` 内含 SHA-256 复核，不匹配即非零退出；输出：`OK: parameters.json derived, digest verified cfb12b78…10c2`。

## V2 · 模型自动测试（RQ-MDL-001..011，acceptance.json 五项容差）

**命令**: `cd outputs/model && python3 run_tests.py`（有界，自行结束）
**退出码**: 0
**原始结果**（末次运行，17/17）:

```
PASS  baseline_digest_verified_and_params_derived
PASS  scenario_locked_single_0.03c  cruise_speed_c=[0.03]
PASS  reference_tolerance:travel_time_yr  actual=141.66666666666669 expected=141.6666667 rel_tol=0.001
PASS  reference_tolerance:kinetic_energy_1mt_j  actual=4.04439830431568e+22 expected=4.0443983e+22 rel_tol=0.01
PASS  reference_tolerance:kinetic_energy_5mt_j  actual=2.02219915215784e+23 expected=2.0221991e+23 rel_tol=0.01
PASS  reference_tolerance:artificial_gravity_m_s2  actual=43.864908449286034 expected=43.8649 rel_tol=0.01
PASS  reference_tolerance:dust_impact_energy_j  actual=40443983.043156795 expected=40443983.0 rel_tol=0.01
PASS  calc1_travel_time_and_light_time  light_time=4.25 yr
PASS  calc2_relativistic_comparison_separate  nonrel=4.044398e+22 rel=4.047130e+22 (分列不混用)
PASS  calc3_accel_decel_efficiency_mass_scenarios
PASS  calc4_artificial_gravity  a=43.8649 m/s^2, rpm@1g=0.9457
PASS  calc5_dust_impact  1mg=4.044398e+07 J ≈ 9.67 kg TNT
PASS  calc6_turn_limits  10%→5.7296°, 15%→8.5944°
PASS  calc7_precursor_return_time  21.25 + 4.25 = 25.5 yr
PASS  calc8_route_sensitivity  (三路线; 两条保留 unknown)
PASS  truth_labels_present_on_all_results
PASS  no_forbidden_claims_in_model_output
[summary] 17/17 checks passed -> PASS
```

**判定**: PASS。结构化结果存于 `outputs/model/output/test_results.json`。

**纠正记录**: 首次运行因 YAML 1.1 将无符号指数 `4.0443983e22` 解析为字符串导致 TypeError；已在 `run_tests.py` 对期望值/容差统一 `float()` 强转后复跑通过。另首跑时 check_app 的禁用声明语境窗口过窄（30 字符）误报纪律性引用，扩至 120 字符后复跑通过；两项均保留在此作为纠正记录。

## V3 · 应用机器检查（RQ-APP-001..003/005/006 的机器可验证部分）

**命令**: `cd outputs/app && node check_app.js`（有界，自行结束）
**退出码**: 0
**原始结果**:

```
PASS  params_traceability_keys
PASS  scenario_locked_single_0.03c  cruise_speed_c=[0.03]
PASS  app_matches_model:travel_time_yr  (与模型输出一致, 1e-12 容差)
PASS  app_matches_model:kinetic_energy_1mt_j
PASS  app_matches_model:kinetic_energy_5mt_j
PASS  app_matches_model:artificial_gravity_m_s2
PASS  app_matches_model:dust_impact_energy_j
PASS  export_payload_has_traceability
PASS  export_payload_complete
PASS  export_payload_fact_labels
PASS  app_no_cloud_dependency  (index.html 无 http(s) 外部资源)
PASS  app_drawing_links_resolve  (4 个图纸相对链接全部存在)
PASS  app_no_forbidden_claims  (仅纪律性引用)
[summary] ALL CHECKS PASSED
```

**判定**: PASS（机器可验证部分）。浏览器人工交互 DEMO 未执行，列入 not_run。

## V4 · SVG 解析与渲染（RQ-DRW-001/002/003/005 的机器部分）

**命令**: `xmllint --noout outputs/drawings/*.svg` → exit=0（三文件良构 XML）
**命令**: `rsvg-convert -o /tmp/render_<name>.png <svg>`（rsvg-convert 2.62.1）
**原始结果**:

```
RENDER_OK outputs/drawings/COR-DRW-001_architecture.svg (314080 bytes)
RENDER_OK outputs/drawings/COR-DRW-002_archive_layout.svg (223797 bytes)
RENDER_OK outputs/drawings/COR-DRW-003_interfaces.svg (218112 bytes)
```

**判定**: PASS。三图均含图号/版本/单位/假设/参数追踪表/追溯键与"概念级"显式标注（INSP 已执行）。渲染 PNG 为临时验证证据，非交付物。

## V5 · OpenSCAD 就绪检查（RQ-DRW-004）

**命令**: `command -v openscad`; `ls -la /opt/homebrew/bin/openscad`; `ls /Applications/OpenSCAD.app`
**原始结果**:

```
openscad NOT on PATH
lrwxr-xr-x … /opt/homebrew/bin/openscad -> /Applications/OpenSCAD.app/Contents/MacOS/OpenSCAD
ls: /Applications/OpenSCAD.app: No such file or directory
```

**判定**: **NOT_RUN / 待检查** — OpenSCAD 本机未就绪（悬挂符号链接，exit=127）。按 run_input 与 RQ-DRW-004 如实记录，不伪造通过。
**异常记录**: 本会话最早一次探测曾返回 `OpenSCAD version 2026.09.03` 且 CSG 导出 exit=0，此后二进制不可复现（疑似临时运行时覆盖层卸载）；以当前可复现状态为准。
**部分佐证（非替代）**: 对 `COR-SCAD-001_concept_model.scad` 的括号配对静态检查 OK（module 3、echo 校核 2 处）；该检查不是 OpenSCAD 解析。

## V6 · 跨文件一致性（本节点产物域内）

- 模型↔应用：V3 五项参考值与模型输出 1e-12 容差一致（同源公式 core.js ↔ corona_model.py，同参数 params.js ← parameters.json ← baseline_frozen.yaml）。PASS。
- 图纸数值注记（141.6667 yr / 4.0444e22 J / 2.0222e23 J / 43.8649 m/s² / 4.0444e7 J / 5.7°–8.6° / 25.5 yr）与 `outputs/model/output/reference_values.json` 及 `model_results.json` 一致（INSP，逐项对照 V2 原始输出）。PASS。
- 追溯键三元组：模型/图纸/应用/验证文件全部嵌入；应用导出 JSON 含三元组（V3 验证）。PASS。

## 未执行项（列入 member_acceptance.json not_run）

1. OpenSCAD 解析/渲染 — 未就绪，待检查。
2. 浏览器人工交互 DEMO — 未执行；node 等价机器检查已执行，不冒充 DEMO。
3. 需求验证矩阵、FINAL_ACCEPTANCE.md、严重问题清零终判 — 验证/总装节点职责。
4. `three_speed_scenarios_present` 硬门 — DISC-001，须平台裁定，本节点不扩展情景。

## 已知差异披露

DISC-001 / DISC-002 / DISC-003 原样继承并记录于 `member_acceptance.json` known_issues，未掩盖、未改写验收准则。
