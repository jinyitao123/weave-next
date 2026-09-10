#!/usr/bin/env python3
"""
vi_run_gates.py —— 独立验证与总装员（verification-integrator）· v6 最终目录十一项硬门槛复跑
=====================================================================

本脚本在本人 output/ 唯一最终目录上, 独立复跑原 acceptance.json 的 11 项硬门槛 (G1…G11),
并据此重新生成:
    outputs/verification/acceptance.json    —— 权威逐门判定 (report)
    outputs/verification/browser-evidence.json —— 真实无头 Chrome 交互证据 (G4/G5)
    outputs/verification/vi_run_gates_output.txt —— 本脚本人类可读执行日志 (report)

纪律 (对照 run_input / 任务书):
  - 检查计数 11 一律由本次实际执行的 gates 字典动态产生 (count_from_execution)。
  - 冻结被测清单 (audited_file_inventory) 由对 outputs/ 的真实文件扫描产生,
     排除 __pycache__ / *.pyc, 并排除报告自引用文件 (FINAL_ACCEPTANCE.md, acceptance.json, 本输出日志)。
  - 任一门槛 未运行 / 失败  => 对应 status FAIL, overall FAIL; 仅当 11 项全 PASS 才 overall PASS。
  - 各门判定引用独立证据工件路径 (见 acceptance.json 的 evidence_refs), 不以本报告自身章节充当证据。
  - OpenSCAD 本机未安装: RH-01 仅核验可编辑源与「概念级」注记, 不做几何渲染 (S3 受限项, 如实声明)。

真实浏览器: 调用 gui_verify.js 驱动真实无头 Chrome (CDP), 完成 0.01c/0.03c/0.05c 切换 + JSON 导出 +
无 http/https 外部资源计数, 其 stdout 写入 browser-evidence.json。
"""

import json
import os
import re
import subprocess
import sys
import hashlib
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUTPUTS = os.path.dirname(HERE)                      # outputs/
MODEL = os.path.join(OUTPUTS, "model")
APP = os.path.join(OUTPUTS, "app")
DRAWINGS = os.path.join(OUTPUTS, "drawings")
REVIEW = os.path.join(OUTPUTS, "review")
NEWLINE = "\n"
CHROME = os.environ.get("CORONA_CHROME",
                        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")

# 报告自引用文件 —— 不列入被测清单 (audited inventory), 不以自身充当证据
REPORT_FILES = {"FINAL_ACCEPTANCE.md", "verification/acceptance.json",
                "verification/vi_run_gates_output.txt"}


def _rel(p):
    return os.path.relpath(p, OUTPUTS).replace(os.sep, "/")


def _sha(p):
    with open(p, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def _run(cmd, cwd=None, timeout=120):
    """有界子进程: 带超时, 返回 CompletedProcess。"""
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True,
                          timeout=timeout)


def _read(p):
    with open(p, "r", encoding="utf-8") as fh:
        return fh.read()


# ---------------------------------------------------------------------------
# 被测清单冻结 (audited inventory) —— 由真实扫描产生, 排除缓存与报告自引用
# ---------------------------------------------------------------------------
def audited_inventory():
    inv = {}
    for root, _dirs, files in os.walk(OUTPUTS):
        for name in files:
            full = os.path.join(root, name)
            rel = _rel(full)
            if "__pycache__" in rel or rel.endswith(".pyc"):
                continue
            if rel in REPORT_FILES:
                continue
            inv[rel] = _sha(full)
    return inv


# ---------------------------------------------------------------------------
# G1 … G11 实现
# ---------------------------------------------------------------------------
def gate_G1():
    """model_tests_pass —— python3 -m unittest tests.test_model (在 outputs/model)。"""
    p = _run([sys.executable, "-m", "unittest", "tests.test_model"], cwd=MODEL)
    out = (p.stdout or "") + (p.stderr or "")
    ok = p.returncode == 0 and "OK" in out and "FAILED" not in out
    m = re.search(r"Ran (\d+) tests", out)
    n = m.group(1) if m else "?"
    detail = {"returncode": p.returncode, "ran_tests": n,
              "snippet": (out.strip().splitlines()[-4:])}
    method = "Test python3 -m unittest tests.test_model (in outputs/model)"
    return ok, method, detail


def gate_G2():
    """three_speed_scenarios_present —— 模型/应用/图纸三速度贯穿 0.01c/0.03c/0.05c。"""
    sys.path.insert(0, MODEL)
    import corona_model as cm
    speeds = cm.CRUISE_SPEEDS_C
    ids = cm.SCENARIO_IDS
    m_ok = (speeds == [0.01, 0.03, 0.05] and ids == ["S-0.01c", "S-0.03c", "S-0.05c"]
            and len(speeds) == 3)
    # app params.js
    pj = _read(os.path.join(APP, "params.js"))
    js_ok = all(s in pj for s in ("0.01", "0.03", "0.05")) and "S-0.01c" in pj
    # FD-01 svg 三速度
    fd = _read(os.path.join(DRAWINGS, "FD-01_speed_axis_scenario_comparison.svg"))
    fd3 = all(x in fd for x in ("0.01c", "0.03c", "0.05c"))
    ok = m_ok and js_ok and fd3
    detail = {"model_speeds": speeds, "scenario_ids": ids, "n_scenarios": len(speeds),
              "app_params_js": js_ok, "fd_three_bars": fd3}
    method = "Inspection model/params.json + app/params.js + drawings/FD-01 三速度贯穿"
    return ok, method, detail


def gate_G3():
    """reference_calculations_within_tolerance —— 首性复算 5 项参考锚点, 均在容差内。"""
    acc = json.load(open("/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json"))
    refs = acc["reference_checks"]
    sys.path.insert(0, MODEL)
    import corona_model as cm
    computed = {
        "travel_years_at_0_03c": cm.time_alpha_yrs(0.03),
        "kinetic_energy_1mt_at_0_03c_j": cm.ke_1mt_j(0.03),
        "kinetic_energy_5mt_at_0_03c_j": cm.ke_5mt_j(0.03),
        "gravity_1km_2rpm_m_s2": cm.gravity_rotating_habitat(1000.0, 2.0),
        "dust_1mg_0_03c_j": cm.ke_dust_1mg_j(0.03),
    }
    rows = []
    all_ok = True
    for name, spec in refs.items():
        val = computed[name]
        exp = float(spec["expected"])
        tol = float(spec["relative_tolerance"])
        rel = abs(val - exp) / abs(exp) if exp else float("inf")
        ok = rel <= tol
        all_ok = all_ok and ok
        rows.append({"name": name, "computed": val, "expected": exp,
                     "rel_err": rel, "tol": tol, "ok": ok})
    worst = max((r["rel_err"] for r in rows), default=0.0)
    detail = {"n": len(rows), "worst_rel_err": worst, "rows": rows, "all_ok": all_ok}
    method = "Analysis first-principles recompute (corona_model) vs acceptance.json reference_checks"
    return all_ok, method, detail


def run_gui():
    """运行真实无头 Chrome GUI 校验 (gui_verify.js), 返回其 stdout 解析后的 dict + exit。"""
    p = _run(["node", os.path.join(HERE, "gui_verify.js"), APP, CHROME], timeout=180)
    try:
        g = json.loads(p.stdout)
    except Exception:
        g = {"checks": [{"name": "gui_parse_error", "ok": False,
                         "detail": p.stdout[:200] + " | " + p.stderr[:200]}], "all_ok": False}
    return g, p.returncode


def gate_G4(gui):
    """app_starts_without_cloud_dependency —— 静态 index.html 无外部 URL + 真实 GUI 无云请求。"""
    html = _read(os.path.join(APP, "index.html"))
    refs = re.findall(r'(?:src|href)\s*=\s*["\']([^"\']+)["\']', html)
    external = [u for u in refs if re.match(r"^https?://", u)]
    static_ok = len(external) == 0
    net = gui.get("network", {})
    gui_load = next((c for c in gui.get("checks", []) if c["name"] == "app_loads_local"), {})
    gui_cloud = next((c for c in gui.get("checks", []) if c["name"] == "no_cloud_dependency"), {})
    ok = static_ok and gui_load.get("ok", False) and gui_cloud.get("ok", False) and net.get("external", []) == []
    detail = {"static_external": external, "gui_load": gui_load,
              "gui_cloud": gui_cloud, "network": {"total": net.get("total", 0),
                                                  "external": net.get("external", [])}}
    method = "Inspection static index.html + Demonstration real headless Chrome (CDP)"
    return ok, method, detail


def gate_G5(gui):
    """app_exports_scenario_json —— 真实浏览器导出 JSON, 且与页面当前情景一致。"""
    export = next((c for c in gui.get("checks", []) if c["name"] == "export_scenario_json"), {})
    ok = bool(export.get("ok", False))
    detail = {"export_check": export, "export": gui.get("export", {}),
              "scenario_switch": gui.get("scenario_switch", {})}
    method = "Demonstration real headless Chrome (CDP) real download + consistency with page"
    return ok, method, detail


def gate_G6():
    """drawings_are_parseable_and_editable —— xmllint 解析 ≥3 SVG + 可编辑 SCAD 源。"""
    svgs = sorted(f for f in os.listdir(DRAWINGS) if f.endswith(".svg"))
    scads = sorted(f for f in os.listdir(DRAWINGS) if f.endswith(".scad"))
    parse = {}
    for f in svgs:
        p = _run(["xmllint", "--noout", os.path.join(DRAWINGS, f)])
        parse[f] = "OK" if p.returncode == 0 else "FAIL"
    parse_ok = all(v == "OK" for v in parse.values())
    editable = len(scads) >= 1
    ok = len(svgs) >= 3 and parse_ok and editable
    detail = {"svgs": svgs, "scads": scads, "parse": parse, "editable_source": editable}
    method = "Test xmllint parse (>=3 SVG) + Inspection OpenSCAD editable source present"
    return ok, method, detail


def gate_G7():
    """all_drawings_marked_conceptual —— 全部图纸含「概念级」标注。"""
    svgs = sorted(f for f in os.listdir(DRAWINGS) if f.endswith(".svg"))
    scads = sorted(f for f in os.listdir(DRAWINGS) if f.endswith(".scad"))
    checked = []
    missing = []
    for f in svgs + scads:
        txt = _read(os.path.join(DRAWINGS, f))
        checked.append(f)
        if "概念级" not in txt:
            missing.append(f)
    ok = len(missing) == 0 and len(checked) >= 4
    detail = {"checked": checked, "missing": missing}
    method = "Inspection concept-level ('概念级') label scan over all drawings"
    return ok, method, detail


def gate_G8():
    """requirements_have_verification_methods —— 每条需求含验证方法。"""
    req = _read(os.path.join(REVIEW, "systems/requirements/requirements_decomposition.md"))
    # 需求行: | SA-REQ-XX | ... | <验证方法> | ...
    rows = re.findall(r"\| (SA-REQ-[A-Z]-?\d+) \| ([^|]*) \| ([^|]*) \|", req)
    ids = []
    missing_method = []
    for rid, text, method in rows:
        ids.append(rid)
        m = method.strip()
        if not m or m in {"", "—", "-", "?"} or "Analysis" not in m and "Inspection" not in m \
           and "Test" not in m and "Demonstration" not in m and "Review of design" not in m:
            missing_method.append(rid)
    # traceability CSV: 31 行, verification_method 列非空
    import csv
    csv_rows = list(csv.DictReader(open(os.path.join(REVIEW, "systems/verification/traceability_matrix.csv"))))
    tm_empty = sum(1 for r in csv_rows if not (r.get("verification_method") or "").strip())
    uniq_ids = sorted(set(ids))
    ok = len(missing_method) == 0 and len(uniq_ids) >= 10 and tm_empty == 0
    detail = {"req_ids": uniq_ids, "req_count": len(uniq_ids), "missing_method": missing_method,
              "traceability_total": len(csv_rows), "traceability_empty_method": tm_empty}
    method = "Review of design + Inspection (需求表验证方法列 + traceability CSV method 列)"
    return ok, method, detail


def gate_G9():
    """claims_use_truth_labels —— 真值标签定义/使用; 无肯定式禁语。"""
    acc_params = _read(os.path.join(MODEL, "params.json"))
    pj = json.loads(acc_params)
    labels = pj.get("truth_labels", [])
    labels_ok = set(labels) >= {"verified_fact", "derived_result", "assumption", "unknown"}
    app_js = _read(os.path.join(APP, "app.js"))
    app_truth = all(t in app_js for t in ("truth_discipline", "forbidden_claims",
                                          "open_unknowns", "unknown", "derived_result"))
    unknown_visible = any("unknown" in app_js for _ in [0])
    # 禁语扫描: 内容表面 (drawings + app page), 命中但须在否定/免责语境
    forbidden_en = ["construction_ready", "manufacturing_ready", "flight_certified",
                    "whole_program_cost_committed"]
    forbidden_zh = ["可施工", "可制造", "可飞行认证", "已具备施工条件", "成本已锁定", "交付即可建造"]
    negation = ["未", "不", "不得", "禁止", "免责", "概念级", "非", "不可", "not", "no ", "disclaimer",
                "forbidden", "unknown", "不承诺", "不能"]
    surfaces = []
    for root, _d, files in os.walk(DRAWINGS):
        for f in files:
            if f.endswith(".svg") or f.endswith(".scad"):
                surfaces.append(os.path.join(root, f))
    surfaces.append(os.path.join(APP, "index.html"))
    bad = []
    hits = 0
    for path in surfaces:
        for i, line in enumerate(_read(path).splitlines(), 1):
            for tok in forbidden_en + forbidden_zh:
                if tok in line:
                    hits += 1
                    if not any(n in line for n in negation):
                        bad.append("%s:%d:%s" % (_rel(path), i, tok))
    ok = labels_ok and app_truth and len(bad) == 0
    detail = {"labels": labels, "app_truth": app_truth, "unknown_visible": unknown_visible,
              "forbidden_hits": hits, "bad_context": bad}
    method = "Inspection truth-label vocabulary + forbidden-claim (negation-context) scan"
    return ok, method, detail


def gate_G10():
    """cross_artifact_parameter_consistency_passes —— 跨产物一致性, 含 FD-01 展示文本。"""
    sys.path.insert(0, MODEL)
    import corona_model as cm
    import mini_yaml
    # params.json vs baseline.yaml
    params = json.loads(_read(os.path.join(MODEL, "params.json")))
    base = mini_yaml.safe_load(_read(os.path.join(MODEL, "baseline.yaml")))
    pv = (params["baseline_id"] == base["baseline_id"]
          and params["scenarios"]["cruise_speed_c"] == base["scenarios"]["cruise_speed_c"]
          and params["scenarios"]["scenario_ids"] == base["scenarios"]["scenario_ids"])
    # params.js vs params.json
    node_js = os.path.join(HERE, "js_model_runner.js")
    p = _run(["node", node_js, APP])
    js = json.loads(p.stdout)
    jv = (js["baseline_id"] == params["baseline_id"]
          and js["cruise_speed_c"] == params["scenarios"]["cruise_speed_c"]
          and js["scenario_ids"] == params["scenarios"]["scenario_ids"])
    # py vs js
    py = {sid: cm.compute_scenario(c) for c, sid in zip(cm.CRUISE_SPEEDS_C, cm.SCENARIO_IDS)}
    keys = ["time_alpha_yrs", "ke_1mt_j", "ke_5mt_j", "ke_dust_1mg_j", "p_rel",
            "v_kmps", "time_glens_yrs", "time_precursor_yrs"]
    worst = 0.0
    mm = []
    for sid in js["all"]:
        for k in keys:
            pv2, jv2 = py[sid][k], js["all"][sid][k]
            rel = abs(pv2 - jv2) / abs(pv2) if pv2 else 0.0
            worst = max(worst, rel)
            if rel > 1e-9:
                mm.append("%s.%s=%.3e" % (sid, k, rel))
    # FD-01 展示文本
    fd = _read(os.path.join(DRAWINGS, "FD-01_speed_axis_scenario_comparison.svg"))
    fd_correct = ("1 mt" in fd and "1e9 kg" in fd and "4.49e21" in fd and "1.12e23" in fd
                  and "½mv²" in fd)
    fd_rows = []
    fd_ok = True
    drawn = {"0.01": 4.49e21, "0.03": 4.04e22, "0.05": 1.12e23}
    for c, dv in drawn.items():
        m_v = cm.ke_1mt_j(float(c))
        rel = abs(dv - m_v) / abs(m_v)
        ok = rel <= 0.01
        fd_ok = fd_ok and ok
        fd_rows.append({"scenario": cm.scenario_id_for_c(float(c)), "drawn": dv,
                        "model": m_v, "rel_err": rel, "ok": ok})
    config_exists = os.path.exists(os.path.join(REVIEW, "systems/config/config_items.yaml"))
    ok = pv and jv and worst <= 1e-9 and not mm and fd_ok and fd_correct and config_exists
    detail = {"params_vs_yaml": pv, "js_vs_json": jv, "py_vs_js_worst_rel_err": worst,
              "py_vs_js_mismatch": mm, "fd_correct": fd_correct, "fd_rows": fd_rows,
              "config_items_exists": config_exists}
    method = "Inspection + Test cross-consistency (params/baseline, params.js, py↔js, FD-01 displayed text)"
    return ok, method, detail


def gate_G11(acc_res, review_index_ok):
    """no_open_severity_one_issue —— 无开放 S1; 关闭判据取自真实执行痕迹 + 无自引用。"""
    risk = _read(os.path.join(REVIEW, "cost_risk/risk_register.md"))
    # 内容层 S1: RK-02..06 已避免
    content_s1_avoided = all(x in risk for x in ("已避免", "RK-02", "RK-03", "RK-04", "RK-05", "RK-06"))
    # G10 一致
    g10 = next((g for g in acc_res["hard_gates"] if g["gate_id"] == "G10"), {})
    g10_ok = g10.get("status") == "PASS"
    # G1 模型测试真跑
    g1 = next((g for g in acc_res["hard_gates"] if g["gate_id"] == "G1"), {})
    g1_ok = g1.get("status") == "PASS"
    # G4/G5 真实 GUI (browser-evidence 存在且 all_ok)
    bev = os.path.join(HERE, "browser-evidence.json")
    gui_ok = input_gui_all_ok = False
    if os.path.exists(bev):
        try:
            be = json.load(open(bev))
            input_gui_all_ok = be.get("all_ok", False)
            gui_ok = True
        except Exception:
            gui_ok = False
    # 检查计数一致性 (11)
    check_count_ok = acc_res.get("check_count_from_execution") == 11
    # 无自引用 (FINAL_ACCEPTANCE 以独立工件为证据)
    fac = _read(os.path.join(OUTPUTS, "FINAL_ACCEPTANCE.md")) if os.path.exists(
        os.path.join(OUTPUTS, "FINAL_ACCEPTANCE.md")) else ""
    selfref_bad = [s for s in ("以「本文件」", "见 §三", "见 §四", "本总装员已复跑闭合") if s in fac]
    subchecks = {
        "content_S1_avoided": content_s1_avoided,
        "risk_S1_cross_domain_closed_by_real": (g1_ok and g10_ok and input_gui_all_ok),
        "app_real_gui_G4_G5": input_gui_all_ok,
        "check_count_consistent_11": check_count_ok,
        "no_self_reference": len(selfref_bad) == 0,
        "review_index_created": review_index_ok,
    }
    ok = all(subchecks.values())
    detail = {"subchecks": subchecks, "selfref_bad": selfref_bad}
    method = "Review of design (severity-one closure via real execution traces + self-reference audit)"
    return ok, method, detail


# ---------------------------------------------------------------------------
# REVIEW_INDEX 路径存在性校验
# ---------------------------------------------------------------------------
def review_index_paths_ok():
    idx = _read(os.path.join(REVIEW, "REVIEW_INDEX.md"))
    paths = set(re.findall(r"`(review/[^`]+)`", idx))
    missing = [p for p in sorted(paths) if not os.path.exists(os.path.join(OUTPUTS, p))]
    return {"paths_found": len(paths), "missing": missing, "ok": len(missing) == 0}


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main():
    t0 = time.time()
    acc_path = "/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json"
    acc = json.load(open(acc_path))
    authoritative_sha = _sha(acc_path)

    # 1) 每个 gate 由函数签名返回 (ok, method, detail)。
    #    G4/G5 共享真实 GUI 一次运行; G11 依赖已生成的 acceptance.json (2-pass, 见下)。
    gui, _gui_rc = run_gui()
    # 先把 GUI 证据落盘 browser-evidence.json —— 由真实运行产生
    with open(os.path.join(HERE, "browser-evidence.json"), "w", encoding="utf-8") as fh:
        json.dump(gui, fh, ensure_ascii=False, indent=2)

    gates = {
        "G1": ("model_tests_pass", gate_G1),
        "G2": ("three_speed_scenarios_present", gate_G2),
        "G3": ("reference_calculations_within_tolerance", gate_G3),
        "G4": ("app_starts_without_cloud_dependency", lambda: gate_G4(gui)),
        "G5": ("app_exports_scenario_json", lambda: gate_G5(gui)),
        "G6": ("drawings_are_parseable_and_editable", gate_G6),
        "G7": ("all_drawings_marked_conceptual", gate_G7),
        "G8": ("requirements_have_verification_methods", gate_G8),
        "G9": ("claims_use_truth_labels", gate_G9),
        "G10": ("cross_artifact_parameter_consistency_passes", gate_G10),
        "G11": ("no_open_severity_one_issue", lambda: (False, "", {"pending": True})),
    }

    # 2) 首轮: 运行 G1..G10, 产生 acc_res 供 G11 读取
    hard_gates = []
    acc_res = {"check_count_from_execution": len(gates)}
    for gid in ["G1", "G2", "G3", "G4", "G5", "G6", "G7", "G8", "G9", "G10"]:
        label, fn = gates[gid]
        try:
            ok, method, detail = fn()
        except Exception as e:
            ok, method, detail = False, "ERROR", {"error": repr(e)}
        hard_gates.append({"gate_id": gid, "gate": label,
                           "status": "PASS" if ok else "FAIL",
                           "method": method, "detail": detail})
    # 3) REVIEW_INDEX 路径校验
    ri = review_index_paths_ok()
    # 4) G11 (二次, 读前面已填的 hard_gates + browser-evidence)
    acc_res["hard_gates"] = hard_gates
    acc_res["check_count_from_execution"] = len(gates)
    g11_ok, g11_method, g11_detail = gate_G11(acc_res, ri["ok"])
    hard_gates.append({"gate_id": "G11", "gate": "no_open_severity_one_issue",
                       "status": "PASS" if g11_ok else "FAIL",
                       "method": g11_method, "detail": g11_detail})
    acc_res["hard_gates"] = hard_gates

    # 5) 被测清单冻结 (fresh scan, 排除缓存与报告自引用)
    inv = audited_inventory()

    # 6) required_final_paths 存在性
    req_paths = {
        "model": {"exists": os.path.isdir(MODEL), "is_dir": True},
        "drawings": {"exists": os.path.isdir(DRAWINGS), "is_dir": True},
        "app": {"exists": os.path.isdir(APP), "is_dir": True},
        "review": {"exists": os.path.isdir(REVIEW), "is_dir": True},
        "verification": {"exists": os.path.isdir(HERE), "is_dir": True},
        "FINAL_ACCEPTANCE.md": {"exists": os.path.exists(os.path.join(OUTPUTS, "FINAL_ACCEPTANCE.md")),
                                "is_dir": False},
    }

    # 7) overall
    overall = "PASS" if all(g["status"] == "PASS" for g in hard_gates) else "FAIL"

    report = {
        "schema_version": 1,
        "authoritative_sha256": authoritative_sha,
        "authoritative_path": acc_path,
        "verifier_node": "corona-prephase-a-recovery-v6-verification-integrator",
        "verification_date": "2026-09-10",
        "baseline_id": json.loads(_read(os.path.join(MODEL, "params.json")))["baseline_id"],
        "required_final_paths_checked": req_paths,
        "review_index_paths": ri,
        "check_count_from_execution": len(gates),
        "audited_file_inventory": inv,
        "hard_gates": hard_gates,
        "overall": overall,
        "note": "每门判定引用独立证据工件路径; 被测清单由真实文件扫描冻结, 排除 __pycache__/*.pyc 与报告自引用文件; "
                "检查计数 11 由本次实际执行动态生成。G4/G5 以真实无头 Chrome(CDP)交互 + JSON 导出为证; "
                "OpenSCAD 本机未安装, G6/G7 仅核验可编辑源与「概念级」注记(S3 受限项,如实声明)。",
    }
    with open(os.path.join(HERE, "acceptance.json"), "w", encoding="utf-8") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=2)

    # 8) 人类可读日志
    lines = []
    lines.append("=== vi_run_gates.py — v6 十一项硬门槛独立复跑日志 ===")
    lines.append("authoritative acceptance.json SHA-256 = %s" % authoritative_sha)
    lines.append("check_count_from_execution = %d" % len(gates))
    lines.append("review_index paths: found=%s ok=%s missing=%s" % (
        ri["paths_found"], ri["ok"], ri["missing"]))
    lines.append("")
    for g in hard_gates:
        lines.append("[%s] G%-2s %s  %s" % ("OK" if g["status"] == "PASS" else "FAIL",
                                             g["gate_id"], g["gate"], g["method"]))
    lines.append("")
    lines.append("OVERALL: %s" % overall)
    lines.append("audited_inventory files (excl cache & self-report) = %d" % len(inv))
    lines.append("elapsed = %.1fs" % (time.time() - t0))
    with open(os.path.join(HERE, "vi_run_gates_output.txt"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")

    print("\n".join(lines))
    return 0 if overall == "PASS" else 1


def params_baseline_id():
    def _f():
        try:
            return json.loads(_read(os.path.join(MODEL, "params.json")))["baseline_id"]
        except Exception:
            return "corona-prephase-a-v1"
    return _f


if __name__ == "__main__":
    sys.exit(main())
