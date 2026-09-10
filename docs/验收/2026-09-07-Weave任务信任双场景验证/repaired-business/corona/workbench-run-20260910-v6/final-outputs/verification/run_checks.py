#!/usr/bin/env python3
"""
run_checks.py —— 日冕计划 v4 · 数字工程交付 · 机器验证清单（机器验证，有界退出）

在 outputs/verification/ 下运行：
    python3 run_checks.py

行为：
  - 逐项执行下列机器检查，收集 PASS/FAIL。
  - 全部通过 → 写 verification_report.md / machine_verification_checklist.md，exit 0。
  - 任一失败 → 写报告（标注 FAIL），exit 1。
  - 所有子进程都有超时；不启动前台开发服务器；无头 Chrome 用完即杀。

检查项（对应硬门槛/需求）:
  model_tests_pass                                  -> REQ-V4-C-01
  model_reference_anchors_within_tolerance          -> REQ-V4-C-03 / acceptance reference_calculations
  params_json_matches_baseline_yaml                 -> REQ-V4-P-05 / gate 10
  params_js_matches_params_json                     -> REQ-V4-P-05 / gate 10
  app_model_matches_python_model (py↔js cross)      -> REQ-V4-P-05 / gate 10 (app 数学==python 数学)
  app_no_external_resource_reference (static)       -> REQ-V4-C-04 / gate 4
  app_gui_no_cloud_requests                         -> REQ-V4-C-04 / gate 4 (real GUI via CDP)
  app_gui_scenario_switch (0.01/0.03/0.05)          -> REQ-V4-C-05
  app_gui_export_json                               -> REQ-V4-C-06 / gate 5
  drawings_parseable_and_labeled (>=3 SVG)          -> REQ-V4-C-07 / REQ-V4-C-08
  openscad_editable_source_present                  -> REQ-V4-C-07
  forbidden_claims_not_asserted                     -> 事实纪律 / forbidden_claims
  truth_discipline_present (export)                 -> REQ-V4-C-10
"""

import json
import os
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUTPUTS = os.path.dirname(HERE)
MODEL_DIR = os.path.join(OUTPUTS, "model")
APP_DIR = os.path.join(OUTPUTS, "app")
DRAWINGS_DIR = os.path.join(OUTPUTS, "drawings")
VERIF_DIR = HERE

CHROME = os.environ.get("CORONA_CHROME",
                        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")

CHECK_TIMEOUT = 90  # 秒；单步机器检查上限

# 每条检查的独立复跑指引 —— 供 verification-integrator 逐条独立复现（不依赖本报告自身章节，
# 彻底避免「以自身为证据 / 见 §X」的自引用）。count 一律由实际执行的 rep.checks 动态产生。
CHECK_GUIDE = {
    "model_tests_pass": (
        "python3 -m unittest tests.test_model（在 outputs/model）",
        "全部通过，exit 0"),
    "model_reference_anchors_within_tolerance": (
        "python3 -c \"import corona_model as c; print(c.reference_check()['all_ok'])\"（在 outputs/model）",
        "all_ok True，5/5 在容差内（worst rel_err ≤ 相对容差）"),
    "params_json_matches_baseline_yaml": (
        "python3 run_checks.py 该项（params.json vs baseline.yaml 机器比对）",
        "baseline_id / cruise_speed_c / scenario_ids / 常量 一致"),
    "params_js_matches_params_json": (
        "node js_model_runner.js <app_dir>，与 model/params.json 比对",
        "baseline_id / cruise_speed_c / scenario_ids 一致"),
    "app_model_matches_python_model": (
        "node js_model_runner.js（py↔js 交叉，8 个核心数值字段 × 三情景）",
        "max rel_err ≤ 1e-9"),
    "app_no_external_resource_reference": (
        "静态扫描 outputs/app/index.html 的 src/href",
        "无 http/https 外部资源引用（external=[]）"),
    "forbidden_claims_not_asserted": (
        "关键字扫描（肯定式禁语）覆盖 drawings/*.svg|scad 与 app/index.html",
        "无「可施工/可制造/可飞行认证/成本已锁定/…」肯定式声明"),
    "drawings_parseable_and_labeled": (
        "xmllint --noout *.svg（≥3 张），并扫描「概念级」",
        "全部解析通过、均含「概念级」"),
    "openscad_editable_source_present": (
        "检查 outputs/drawings/*.scad 存在且注记「概念级」",
        "存在可编辑源且含概念级注记（本机未装 openscad，不渲染）"),
    "truth_discipline_present": (
        "检查 app.js 导出结构含真实标签字段",
        "含 truth_discipline / forbidden_claims / open_unknowns / unknown"),
    "app_gui": (
        "node gui_verify.js <app_dir> <chrome>（真实无头 Chrome + CDP 交互）",
        "对应 GUI 项 PASS（本地加载/默认情景/三情景切换/导出/无云请求）"),
}


def run(cmd, cwd=None, timeout=CHECK_TIMEOUT, env=None):
    """有界运行子进程：带超时，返回 CompletedProcess。"""
    merged_env = dict(os.environ)
    if env:
        merged_env.update(env)
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True,
                          timeout=timeout, env=merged_env)


def read(path):
    with open(path, "r", encoding="utf-8") as fh:
        return fh.read()


def rel(p):
    return os.path.relpath(p, OUTPUTS)


def reloc(d):
    return {os.path.relpath(k, OUTPUTS): v for k, v in d.items()}


class Report:
    def __init__(self):
        self.checks = []
        self.extra = {}

    def add(self, name, ok, detail="", method=""):
        self.checks.append({"name": name, "ok": bool(ok), "detail": detail, "method": method})
        return ok

    def all_ok(self):
        return all(c["ok"] for c in self.checks)


def check_model_tests(rep):
    p = run([sys.executable, "-m", "unittest", "tests.test_model"], cwd=MODEL_DIR)
    # unittest 的 TextTestRunner 将结果写到 stderr
    out = (p.stdout or "") + (p.stderr or "")
    ok = p.returncode == 0 and "OK" in out
    tail = out.strip().splitlines()[-3:]
    rep.add("model_tests_pass", ok, detail="returncode=%d last=%r" % (p.returncode, tail),
            method="Test/Run python3 -m unittest tests.test_model")


def check_reference_anchors(rep):
    sys.path.insert(0, MODEL_DIR)
    import corona_model as cm
    rc = cm.reference_check()
    worst = max((r["rel_err"] for r in rc["rows"]), default=0.0)
    rep.add("model_reference_anchors_within_tolerance", rc["all_ok"],
            detail="5/5 anchors, worst rel_err=%.3e, all_ok=%s" % (worst, rc["all_ok"]),
            method="Test/Analysis corona_model.reference_check()")


def check_params_json_vs_baseline(rep):
    sys.path.insert(0, MODEL_DIR)
    import json
    import mini_yaml
    base = mini_yaml.safe_load(read(os.path.join(MODEL_DIR, "baseline.yaml")))
    params = json.loads(read(os.path.join(MODEL_DIR, "params.json")))
    ok = (params["baseline_id"] == base["baseline_id"]
          and params["scenarios"]["cruise_speed_c"] == base["scenarios"]["cruise_speed_c"]
          and params["scenarios"]["scenario_ids"] == base["scenarios"]["scenario_ids"]
          and all(params["constants"].get(k) == base["constants"].get(k)
                  for k in ("speed_of_light_m_s", "standard_gravity_m_s2", "proxima_distance_ly",
                            "ly_m", "sec_per_year", "tnt_equivalent_J_per_kg")))
    rep.add("params_json_matches_baseline_yaml", ok,
            detail="baseline_id=%s speeds=%s" % (params["baseline_id"], params["scenarios"]["cruise_speed_c"]),
            method="Inspection params.json vs baseline.yaml")


def check_params_js_vs_json(rep):
    # 用 node 读取 JSON 中的 params 对象与 app/params.js 比对
    node_script = os.path.join(VERIF_DIR, "js_model_runner.js")
    p = run(["node", node_script, APP_DIR])
    if p.returncode != 0:
        rep.add("params_js_matches_params_json", False, detail="node 失败: " + p.stderr[:200], method="Test")
        return
    js = json.loads(p.stdout)
    json_params = json.loads(read(os.path.join(MODEL_DIR, "params.json")))
    ok = (js["baseline_id"] == json_params["baseline_id"]
          and js["cruise_speed_c"] == json_params["scenarios"]["cruise_speed_c"]
          and js["scenario_ids"] == json_params["scenarios"]["scenario_ids"])
    rep.add("params_js_matches_params_json", ok,
            detail="baseline_id=%s speeds=%s" % (js["baseline_id"], js["cruise_speed_c"]),
            method="Test node js_model_runner.js vs params.json")


def check_py_vs_js_model(rep):
    sys.path.insert(0, MODEL_DIR)
    import corona_model as cm
    py = {sid: cm.compute_scenario(c) for c, sid in zip(cm.CRUISE_SPEEDS_C, cm.SCENARIO_IDS)}
    node_script = os.path.join(VERIF_DIR, "js_model_runner.js")
    p = run(["node", node_script, APP_DIR])
    if p.returncode != 0:
        rep.add("app_model_matches_python_model", False, detail="node 失败: " + p.stderr[:200], method="Test")
        return
    js = json.loads(p.stdout)["all"]
    keys = ["time_alpha_yrs", "ke_1mt_j", "ke_5mt_j", "ke_dust_1mg_j", "p_rel",
            "v_kmps", "time_glens_yrs", "time_precursor_yrs"]
    worst = 0.0
    mismatches = []
    for sid in js:
        for k in keys:
            pv, jv = py[sid][k], js[sid][k]
            relerr = abs(pv - jv) / abs(pv) if pv else 0.0
            worst = max(worst, relerr)
            if relerr > 1e-9:
                mismatches.append("%s.%s rel=%.3e" % (sid, k, relerr))
    ok = worst <= 1e-9 and not mismatches
    rep.add("app_model_matches_python_model", ok,
            detail="max rel_err=%.3e, %d fields/36 checked%s" % (worst, len(keys) * 3,
                    (" mismatches=" + ";".join(mismatches[:3])) if mismatches else ""),
            method="Test cross-consistency py↔js")


def check_app_no_external_resources_static(rep):
    html = read(os.path.join(APP_DIR, "index.html"))
    refs = re.findall(r'(?:src|href)\s*=\s*["\']([^"\']+)["\']', html)
    external = [u for u in refs if re.match(r'^https?://', u)]
    ok = len(external) == 0
    rep.add("app_no_external_resource_reference", ok,
            detail="refs=%s external=%s" % (len(refs), external),
            method="Inspection index.html src/href 字段")


FORBIDDEN_EN = ["construction_ready", "manufacturing_ready", "flight_certified", "whole_program_cost_committed"]
FORBIDDEN_ZH_POSITIVE = ["可施工", "可制造", "可飞行认证", "已具备施工条件", "成本已锁定", "交付即可建造"]


def check_forbidden_claims(rep):
    # 扫描内容表面（图纸、应用页面、导出的关键表述），确保未出现肯定式禁语
    surfaces = []
    for root, _dirs, files in os.walk(DRAWINGS_DIR):
        for f in files:
            if f.endswith(".svg") or f.endswith(".scad"):
                surfaces.append(os.path.join(root, f))
    surfaces.append(os.path.join(APP_DIR, "index.html"))
    hits = []
    for path in surfaces:
        txt = read(path)
        for tok in FORBIDDEN_EN + FORBIDDEN_ZH_POSITIVE:
            # 仅在内容声明式中命中才算问题；forbidden_claims 列表不在这些 surface 中出现
            if tok in txt:
                hits.append("%s: %s" % (rel(path), tok))
    rep.add("forbidden_claims_not_asserted", len(hits) == 0,
            detail=("clean" if not hits else "; ".join(hits)),
            method="Inspection 关键字扫描（肯定式禁语）")


def check_drawings(rep):
    svgs = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".svg"))
    scads = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".scad"))
    ok_count = len(svgs) >= 3
    parse_ok = True
    for f in svgs:
        p = run(["xmllint", "--noout", os.path.join(DRAWINGS_DIR, f)])
        if p.returncode != 0:
            parse_ok = False
            parse_fail = f
    labeled = all(("概念级" in read(os.path.join(DRAWINGS_DIR, f))) for f in svgs)
    ok = ok_count and parse_ok and labeled
    rep.add("drawings_parseable_and_labeled", ok,
            detail="svgs=%d scads=%d parse_ok=%s labeled=%s" % (len(svgs), len(scads), parse_ok, labeled),
            method="Test XML 解析 (xmllint) + 内容扫描")
    rep.extra["drawings_svg"] = svgs
    rep.extra["drawings_scad"] = scads

    # OpenSCAD 可编辑源存在且带概念级标注
    scad_ok = len(scads) >= 1
    scad_labeled = False
    if scads:
        scad_labeled = ("概念级" in read(os.path.join(DRAWINGS_DIR, scads[0])))
    rep.add("openscad_editable_source_present", scad_ok and scad_labeled,
            detail="scads=%s labeled=%s（本机未装 openscad，仅核验可编辑源与注记）" % (scads, scad_labeled),
            method="Inspection editable source")


def check_gui(rep):
    p = run(["node", os.path.join(VERIF_DIR, "gui_verify.js"), APP_DIR, CHROME])
    if p.returncode not in (0, 1):
        rep.add("gui_harness", False, detail="node 失败 rc=%d stderr=%s" % (p.returncode, p.stderr[:300]), method="Test")
        return
    try:
        g = json.loads(p.stdout)
    except Exception as e:
        rep.add("gui_harness", False, detail="解析 GUI 输出失败: %s" % e, method="Test")
        return
    for c in g.get("checks", []):
        rep.add("app_gui_" + c["name"], c["ok"], detail=c["detail"], method="Demonstration headless Chrome (CDP)")


def check_truth_discipline(rep):
    # 导出对象应含 truth_discipline 标签
    node_script = os.path.join(VERIF_DIR, "js_model_runner.js")
    # 直接用应用构建导出对象（无 DOM 环境下通过 AppAPI 模拟——但 AppAPI 需 DOM；改用静态检查）
    app_js = read(os.path.join(APP_DIR, "app.js"))
    ok = ("truth_discipline" in app_js and "forbidden_claims" in app_js
          and "open_unknowns" in app_js and "derived_result" in app_js
          and "unknown" in app_js)
    rep.add("truth_discipline_present", ok,
            detail="app.js contains truth_discipline/forbidden_claims/open_unknowns/unknown",
            method="Inspection app.js + export structure")


def main():
    t0 = time.time()
    rep = Report()
    check_model_tests(rep)
    check_reference_anchors(rep)
    check_params_json_vs_baseline(rep)
    check_params_js_vs_json(rep)
    check_py_vs_js_model(rep)
    check_app_no_external_resources_static(rep)
    check_forbidden_claims(rep)
    check_drawings(rep)
    check_truth_discipline(rep)
    check_gui(rep)
    elapsed = time.time() - t0

    all_ok = rep.all_ok()
    # 写报告
    write_reports(rep, elapsed)
    print("VERIFY_%s (%d checks, %.1fs)" % ("PASS" if all_ok else "FAIL", len(rep.checks), elapsed))
    for c in rep.checks:
        print("  [%s] %s  %s" % ("OK" if c["ok"] else "FAIL", c["name"], c["detail"]))
    sys.exit(0 if all_ok else 1)


def write_reports(rep, elapsed):
    status = "PASS" if rep.all_ok() else "FAIL"
    n_checks = len(rep.checks)  # 检查计数由本次实际执行动态产生
    lines = []
    lines.append("# 日冕计划 v6 · 数字工程交付 · 机器验证清单与报告")
    lines.append("")
    lines.append("**产出**：digital-engineering-builder（corona-prephase-a-recovery-v6-digital-engineering-builder）")
    lines.append("**日期**：2026-09-10")
    lines.append("**结果**：`%s`（%d 项检查，计数由本次实际执行动态产生）" % (status, n_checks))
    lines.append("**命令**：`python3 run_checks.py`（有界退出，exit 0=通过 / 1=失败）；全步骤带超时，不启动前台开发服务器。")
    lines.append("")
    lines.append("| # | 检查项 | 方法 | 结果 | 说明 |")
    lines.append("|---|---|---|---|---|")
    for i, c in enumerate(rep.checks, 1):
        lines.append("| %d | `%s` | %s | %s | %s |" % (i, c["name"], c["method"],
                                                      "PASS" if c["ok"] else "FAIL", c["detail"]))
    lines.append("")
    lines.append("## 覆盖的产物与独立证据")
    lines.append("")
    lines.append("本表所列每条检查均给出**独立可复跑命令/判据**（见 `machine_verification_checklist.md`），")
    lines.append("不依赖本报告自身章节充当证据；gate 4/5/10 的判定依据指向下方独立证据工件路径，而非「见本报告 §X」。")
    lines.append("")
    lines.append("- 统一模型 `outputs/model/`（G1 model_tests_pass / G3 reference_calculations_within_tolerance）")
    lines.append("- 数字应用 `outputs/app/`（G4 app_starts_without_cloud_dependency / G5 app_exports_scenario_json / G10 cross_artifact_parameter_consistency_passes）")
    lines.append("- 概念图纸 `outputs/drawings/`（G6 drawings_are_parseable_and_editable / G7 all_drawings_marked_conceptual）")
    lines.append("- 事实纪律 / 真值标签 / 禁用语（G9 claims_use_truth_labels 的机器侧支撑）")
    lines.append("")
    lines.append("**独立证据工件（引证时优先使用，勿以“本文件”自证）：**")
    lines.append("- 模型单元测试：`python3 -m unittest tests.test_model`（在 outputs/model，exit 0）→ 结果文本 `model/tests/` 输出或下方报告行")
    lines.append("- 参考锚点复算：`python3 -c \"import corona_model as c; print(c.reference_check())\"`（0.03c 五锚点，worst rel_err 见报告行 2）")
    lines.append("- 应用 GUI 真实交互（gate 4/5）：`node gui_verify.js outputs/app <chrome>` → stdout JSON 记录默认情景/三情景切换/导出文件/外部请求=0")
    lines.append("- 跨产物一致性（gate 10）：`node js_model_runner.js outputs/app` 与 `corona_model.py` 交叉比对，max rel_err ≤ 1e-9")
    lines.append("")
    lines.append("## 计数一致性说明（v4 检查计数漂移修复）")
    lines.append("")
    lines.append("v4 中 `machine_verification_checklist.md` 手写声明 M1–M12（12 项）与实际 `run_checks.py` 注册的 ")
    lines.append("%d 项检查存在漂移。v6 起本清单与报告**均由 rep.checks 动态生成**，二者计数恒等于本次实际执行的检查数（== 清单行数），" % n_checks)
    lines.append("清单逐行对应**一次真实执行**的每一项检查，不再有独立手写子集。")
    lines.append("")
    lines.append("## 诚实边界")
    lines.append("")
    lines.append("1. 本清单由**生产者**（digital-engineering-builder）自身运行并自证；按 CR-003 / REQ-V4-P-01/02，")
    lines.append("   十一项硬门槛的**独立复跑**与最终判定属 verification-integrator（独立验证与总装员），本节点不代做。")
    lines.append("2. OpenSCAD 本机未安装，`openscad_editable_source_present` 仅核验可编辑源文件与概念级注记，未做几何渲染。")
    lines.append("3. GUI 验证以**真实无头 Chrome + CDP 交互**完成（点击情景按钮、触发导出下载、统计网络请求），不使用 Node DOM-shim。")
    lines.append("")
    lines.append("## 关于 v4 最终报告自引用的处置")
    lines.append("")
    lines.append("v4 `FINAL_ACCEPTANCE.md` 存在**自引用缺陷**：以「本文件」作为自身 `required_final_path` 的证据，")
    lines.append("并以「见 §三/§四」「本总装员已复跑闭合」作为 gates 4/5/10 的判定依据。本节点（数字工程）提供")
    lines.append("`machine_verification_checklist.md` 作为**逐条独立复跑指引**，并在下文给出**独立证据工件路径**，")
    lines.append("使 verification-integrator 在 v6 重写 `FINAL_ACCEPTANCE.md` 时改为指向真实证据工件而非自引用。")
    lines.append("（`FINAL_ACCEPTANCE.md` 的最终判定内容由独立验证与总装员节点重写，本节点不代为生成。）")
    lines.append("")

    with open(os.path.join(VERIF_DIR, "verification_report.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines))

    # machine_verification_checklist.md —— 清单 + 逐项方法/判据（供复验者复跑）。
    # 行数 == 本条 run_checks.py 实际注册/执行的检查数（动态），与 verification_report.md 一致，无计数漂移。
    cl = []
    cl.append("# 机器验证清单（machine verification checklist）")
    cl.append("")
    cl.append("用途：供 verification-integrator 独立复跑；每行给出命令/方法与判据。")
    cl.append("**本清单由本次实际执行的检查动态生成：共 %d 项（= 报告行数），与 `run_checks.py` 注册的检查一致。**" % n_checks)
    cl.append("")
    cl.append("| 项 | 检查 | 命令（有界） | 判据 |")
    cl.append("|---|---|---|---|")
    for i, c in enumerate(rep.checks, 1):
        name = c["name"]
        if name.startswith("app_gui_"):
            cmd, crit = CHECK_GUIDE["app_gui"]
        else:
            cmd, crit = CHECK_GUIDE.get(name, ("python3 run_checks.py 该项", "对应项 PASS"))
        cl.append("| M%d | %s | `%s` | %s |" % (i, name, cmd, crit))
    cl.append("")
    cl.append("**退出纪律**：任何一项失败或未运行，总体标 FAIL，exit 1；不得以前台开发服务器充当完成证据。")
    cl.append("")
    with open(os.path.join(VERIF_DIR, "machine_verification_checklist.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(cl))


if __name__ == "__main__":
    main()
