# 机器验证清单（machine verification checklist）

用途：供 verification-integrator 独立复跑；每行给出命令/方法与判据。
**本清单由本次实际执行的检查动态生成：共 18 项（= 报告行数），与 `run_checks.py` 注册的检查一致。**

| 项 | 检查 | 命令（有界） | 判据 |
|---|---|---|---|
| M1 | model_tests_pass | `python3 -m unittest tests.test_model（在 outputs/model）` | 全部通过，exit 0 |
| M2 | model_reference_anchors_within_tolerance | `python3 -c "import corona_model as c; print(c.reference_check()['all_ok'])"（在 outputs/model）` | all_ok True，5/5 在容差内（worst rel_err ≤ 相对容差） |
| M3 | params_json_matches_baseline_yaml | `python3 run_checks.py 该项（params.json vs baseline.yaml 机器比对）` | baseline_id / cruise_speed_c / scenario_ids / 常量 一致 |
| M4 | params_js_matches_params_json | `node js_model_runner.js <app_dir>，与 model/params.json 比对` | baseline_id / cruise_speed_c / scenario_ids 一致 |
| M5 | app_model_matches_python_model | `node js_model_runner.js（py↔js 交叉，8 个核心数值字段 × 三情景）` | max rel_err ≤ 1e-9 |
| M6 | app_no_external_resource_reference | `静态扫描 outputs/app/index.html 的 src/href` | 无 http/https 外部资源引用（external=[]） |
| M7 | forbidden_claims_not_asserted | `关键字扫描（肯定式禁语）覆盖 drawings/*.svg|scad 与 app/index.html` | 无「可施工/可制造/可飞行认证/成本已锁定/…」肯定式声明 |
| M8 | drawings_parseable_and_labeled | `xmllint --noout *.svg（≥3 张），并扫描「概念级」` | 全部解析通过、均含「概念级」 |
| M9 | openscad_editable_source_present | `检查 outputs/drawings/*.scad 存在且注记「概念级」` | 存在可编辑源且含概念级注记（本机未装 openscad，不渲染） |
| M10 | truth_discipline_present | `检查 app.js 导出结构含真实标签字段` | 含 truth_discipline / forbidden_claims / open_unknowns / unknown |
| M11 | app_gui_app_loads_local | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M12 | app_gui_default_scenario_0_03c | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M13 | app_gui_switch_S-0.01c | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M14 | app_gui_switch_S-0.03c | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M15 | app_gui_switch_S-0.05c | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M16 | app_gui_export_scenario_json | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M17 | app_gui_no_cloud_dependency | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |
| M18 | app_gui_gui_harness | `node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）` | 对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求） |

**退出纪律**：任何一项失败或未运行，总体标 FAIL，exit 1；不得以前台开发服务器充当完成证据。
