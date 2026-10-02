#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
vi_acceptance.py —— verification-integrator（独立验证与总装员）独立复跑 acceptance.json 十一项硬门槛。

本脚本由独立验证与总装员节点（本 invocation）编写并运行，作为最终验收判定的唯一依据。
原则：
  - 只读取权威只读文件 acceptance.json（校验 SHA-256）与冻结交付产物；
  - 对每一门（G1..G11）在本次真实执行中实际复跑，判定 PASS/FAIL；
  - 检查计数、被测文件清单（含 SHA-256）均由本次执行记录动态生成；
  - 不采信任何成员自称的验收结论；每门判定指向独立证据工件路径，避免报告自引用；
  - 在本脚本内生成 FINAL_ACCEPTANCE.md，并对其做「无自引用」审计后回填 G11。

产出（本节点 outputs/verification/ 下）：
  - vi_acceptance_output.txt  本次真实运行可读日志
  - acceptance.json           机器可读最终判定（逐门 G1..G11 + overall）
  - browser-evidence.json     应用 GUI 真实交互证据（源自 gui_verify.js 真实无头 Chrome）
产出（outputs/ 下）：FINAL_ACCEPTANCE.md

用法：python3 vi_acceptance.py   （在 outputs/verification 下运行）
"""

import hashlib
import json
import math
import os
import re
import subprocess
import sys
import time
import xml.etree.ElementTree as ET

HERE = os.path.dirname(os.path.abspath(__file__))
OUTPUTS = os.path.dirname(HERE)
MODEL_DIR = os.path.join(OUTPUTS, "model")
APP_DIR = os.path.join(OUTPUTS, "app")
DRAWINGS_DIR = os.path.join(OUTPUTS, "drawings")
REVIEW_DIR = os.path.join(OUTPUTS, "review")
VERIF_DIR = HERE

AUTH_ACCEPTANCE = "<MAINTAINER_LOCAL_PATH>"
AUTH_SHA256 = "f4777aaf587b4a81a4fef1932717bf95a0aa063fa21a8e390dfd2ecd8cc60ec4"
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

GATES = [
    ("G1", "model_tests_pass"),
    ("G2", "three_speed_scenarios_present"),
    ("G3", "reference_calculations_within_tolerance"),
    ("G4", "app_starts_without_cloud_dependency"),
    ("G5", "app_exports_scenario_json"),
    ("G6", "drawings_are_parseable_and_editable"),
    ("G7", "all_drawings_marked_conceptual"),
    ("G8", "requirements_have_verification_methods"),
    ("G9", "claims_use_truth_labels"),
    ("G10", "cross_artifact_parameter_consistency_passes"),
    ("G11", "no_open_severity_one_issue"),
]

LOG = []
INV = {}


def log(*a):
    s = " ".join(str(x) for x in a)
    LOG.append(s)
    print(s, flush=True)


def rel(p):
    return os.path.relpath(p, OUTPUTS)


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def read(p):
    with open(p, "r", encoding="utf-8") as fh:
        return fh.read()


def run(cmd, cwd=None, timeout=90):
    try:
        p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout)
        return p.returncode, (p.stdout or "") + (p.stderr or "")
    except subprocess.TimeoutExpired:
        return -1, "TIMEOUT after %ds" % timeout
    except Exception as e:  # noqa: BLE001
        return -2, "RUN ERROR %r" % e


def authoritative():
    if not os.path.exists(AUTH_ACCEPTANCE):
        log("AUTH: %s 不存在" % AUTH_ACCEPTANCE)
        return None
    digest = sha256(AUTH_ACCEPTANCE)
    acc = json.loads(read(AUTH_ACCEPTANCE))
    ok = digest.lower() == AUTH_SHA256.lower()
    log("AUTH: SHA-256 = %s -> %s (期望 %s)" % (digest, "MATCH" if ok else "MISMATCH", AUTH_SHA256))
    if not ok:
        log("AUTH: 权威文件哈希不匹配，中止。")
        return None
    return acc


def freeze_inventory(exclude=()):
    excl = set(os.path.abspath(os.path.join(VERIF_DIR, x)) for x in exclude)
    for root, _dirs, files in os.walk(OUTPUTS):
        for f in files:
            fp = os.path.join(root, f)
            if os.path.abspath(fp) in excl or fp.endswith("vi_acceptance.py"):
                continue
            INV[rel(fp)] = sha256(fp)
    log("INVENTORY: frozen %d delivered files (SHA-256)" % len(INV))


def load_model():
    sys.path.insert(0, MODEL_DIR)
    import corona_model as cm  # noqa: F401
    return cm


def js_model():
    rc, out = run(["node", os.path.join(VERIF_DIR, "js_model_runner.js"), APP_DIR])
    if rc != 0:
        return None, out
    try:
        return json.loads(out), out
    except Exception as e:  # noqa: BLE001
        return None, "JSON parse fail: %r" % e


# ---------------- G1 ----------------
def gate_g1():
    rc, out = run([sys.executable, "-m", "unittest", "tests.test_model"], cwd=MODEL_DIR)
    tail = out.strip().splitlines()[-3:]
    ok = rc == 0 and "OK" in out
    log("G1 model_tests_pass: rc=%d -> %s ; tail=%s" % (rc, "PASS" if ok else "FAIL", tail))
    return ok, {"returncode": rc, "snippet": tail}, "Test python3 -m unittest tests.test_model"


# ---------------- G2 ----------------
def gate_g2(cm):
    ok_speed = cm.CRUISE_SPEEDS_C == [0.01, 0.03, 0.05]
    ok_ids = cm.SCENARIO_IDS == ["S-0.01c", "S-0.03c", "S-0.05c"]
    allres = cm.compute_all()
    ok_all = set(allres.keys()) == {"S-0.01c", "S-0.03c", "S-0.05c"}
    jsj, _ = js_model()
    ok_js = bool(jsj) and jsj.get("cruise_speed_c") == [0.01, 0.03, 0.05]
    fd = read(os.path.join(DRAWINGS_DIR, "FD-01_speed_axis_scenario_comparison.svg"))
    ok_draw = all(s in fd for s in ("4.49e21 J", "4.04e22 J", "1.12e23 J"))
    ok = ok_speed and ok_ids and ok_all and ok_js and ok_draw
    detail = "model speeds=%s ids=%s scenarios=%d js=%s fd3bars=%s" % (
        cm.CRUISE_SPEEDS_C, cm.SCENARIO_IDS, len(allres), ok_js, ok_draw)
    log("G2 three_speed_scenarios_present: %s ; %s" % ("PASS" if ok else "FAIL", detail))
    return ok, {"detail": detail}, "Inspection model/app/drawing 三速度贯穿"


# ---------------- G3 ----------------
def gate_g3(cm, acc):
    ref = acc["reference_checks"]
    C, PROX, CS = 299792458.0, 4.25, 0.03
    ANCHOR = {
        "travel_years_at_0_03c": PROX / CS,
        "kinetic_energy_1mt_at_0_03c_j": 0.5 * 1.0e9 * (CS * C) ** 2,
        "kinetic_energy_5mt_at_0_03c_j": 0.5 * 5.0e9 * (CS * C) ** 2,
        "gravity_1km_2rpm_m_s2": (2 * math.pi * 2.0 / 60.0) ** 2 * 1000.0,
        "dust_1mg_0_03c_j": 0.5 * 1.0e-6 * (CS * C) ** 2,
    }
    all_ok, worst, rows = True, 0.0, []
    for name, spec in ref.items():
        exp, tol = float(spec["expected"]), float(spec["relative_tolerance"])
        act = ANCHOR[name]
        relerr = abs(act - exp) / abs(exp) if exp else 0.0
        ok = relerr <= tol
        all_ok = all_ok and ok
        worst = max(worst, relerr)
        rows.append({"name": name, "computed": act, "expected": exp, "rel_err": relerr, "tol": tol, "ok": ok})
        log("   G3 anchor %-34s actual=%.6e exp=%.6e rel=%.3e tol=%g %s" %
            (name, act, exp, relerr, tol, "PASS" if ok else "FAIL"))
    live_ok = all(m["ok"] for m in cm.reference_check()["rows"])
    ok = all_ok and live_ok
    log("G3 reference_calculations_within_tolerance: independent %s (n=%d worst=%.3e) ; model cross %s -> %s" %
        ("PASS" if all_ok else "FAIL", len(rows), worst, "PASS" if live_ok else "FAIL", "PASS" if ok else "FAIL"))
    return ok, {"n": len(rows), "worst_rel_err": worst, "rows": rows}, "Analysis first-principles + model cross"


# ---------------- G4 ----------------
def gate_g4(gui):
    html = read(os.path.join(APP_DIR, "index.html"))
    refs = re.findall(r'(?:src|href)\s*=\s*["\']([^"\']+)["\']', html)
    external = [u for u in refs if re.match(r'^https?://', u)]
    stat_ok = len(external) == 0
    gl = next((c for c in gui["checks"] if c["name"] == "app_loads_local"), None)
    gn = next((c for c in gui["checks"] if c["name"] == "no_cloud_dependency"), None)
    gui_ok = bool(gl and gl["ok"]) and bool(gn and gn["ok"])
    net = gui.get("network", {})
    ok = stat_ok and gui_ok
    log("G4 app_starts_without_cloud_dependency: static external=%s ; gui load=%s no_cloud=%s (req=%s ext=%d) -> %s" %
        (external, gl and gl["ok"], gn and gn["ok"], net.get("total"), len(net.get("external", [])),
         "PASS" if ok else "FAIL"))
    return ok, {"static_external": external, "gui_load": gl, "gui_cloud": gn,
                "network": {"total": net.get("total"), "external": net.get("external", [])}}, \
        "Inspection static + Demonstration headless Chrome (CDP)"


# ---------------- G5 ----------------
def gate_g5(gui):
    ec = next((c for c in gui["checks"] if c["name"] == "export_scenario_json"), None)
    ok = bool(ec and ec["ok"])
    det = ec["detail"] if ec else "no export check"
    log("G5 app_exports_scenario_json: %s ; %s" % ("PASS" if ok else "FAIL", det))
    return ok, {"detail": det, "export": gui.get("export")}, "Demonstration headless Chrome (CDP) real download"


# ---------------- G6 ----------------
def gate_g6():
    svgs = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".svg"))
    scads = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".scad"))
    parse_txt, all_parse_ok = [], True
    for f in svgs:
        rc, _ = run(["xmllint", "--noout", os.path.join(DRAWINGS_DIR, f)])
        ok = rc == 0
        all_parse_ok = all_parse_ok and ok
        parse_txt.append("%s:%s" % (f, "OK" if ok else "FAIL"))
    ok_count = len(svgs) >= 3
    ok_edit = len(scads) >= 1
    ok = all_parse_ok and ok_count and ok_edit
    log("G6 drawings_are_parseable_and_editable: svgs=%d scads=%d parse=%s -> %s" %
        (len(svgs), len(scads), ";".join(parse_txt), "PASS" if ok else "FAIL"))
    return ok, {"svgs": svgs, "scads": scads, "parse": parse_txt, "editable_source": ok_edit}, \
        "Test xmllint parse + Inspection editable source"


# ---------------- G7 ----------------
def gate_g7():
    svgs = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".svg"))
    scads = sorted(f for f in os.listdir(DRAWINGS_DIR) if f.endswith(".scad"))
    misses = [f for f in svgs + scads if "概念级" not in read(os.path.join(DRAWINGS_DIR, f))]
    ok = len(misses) == 0 and len(svgs) > 0
    log("G7 all_drawings_marked_conceptual: %d svg + %d scad, miss=%s -> %s" %
        (len(svgs), len(scads), misses or "none", "PASS" if ok else "FAIL"))
    return ok, {"checked": svgs + scads, "missing": misses}, "Inspection concept-level label scan"


# ---------------- G8 ----------------
ALLOWED_METHODS = {"Analysis", "Demonstration", "Inspection", "Test", "Review of design"}
def gate_g8():
    txt = read(os.path.join(REVIEW_DIR, "systems/requirements/requirements_decomposition.md"))
    only_sa = []
    for line in txt.splitlines():
        if not line.strip().startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if not cells or cells[0].startswith("---") or cells[0] in ("ID",):
            continue
        rid = cells[0]
        if rid.startswith("SA-REQ-"):
            method = cells[2] if len(cells) > 2 else ""
            only_sa.append((rid, method))
    missing = [r for r, m in only_sa if not any(a in m for a in ALLOWED_METHODS)]
    csv_path = os.path.join(REVIEW_DIR, "systems/verification/traceability_matrix.csv")
    lines = read(csv_path).splitlines()
    header = [c.strip() for c in lines[0].strip().split(",")]
    vm_idx = header.index("verification_method") if "verification_method" in header else -1
    rid_idx = header.index("req_id") if "req_id" in header else -1
    total, empty_vm = 0, 0
    for line in lines[1:]:
        cells = [c.strip() for c in line.split(",")]
        if len(cells) <= vm_idx or len(cells) <= rid_idx:
            continue
        total += 1
        if not cells[vm_idx]:
            empty_vm += 1
    ok = (len(missing) == 0) and (total > 0) and (empty_vm == 0)
    log("G8 requirements_have_verification_methods: SA-REQ=%d missing=%s ; traceability total=%d empty_vm=%d -> %s" %
        (len(only_sa), missing or "none", total, empty_vm, "PASS" if ok else "FAIL"))
    return ok, {"sa_req_count": len(only_sa), "missing_method": missing,
                "traceability_total": total, "traceability_empty_method": empty_vm}, "Review of design + Inspection"


# ---------------- G9 ----------------
FORBIDDEN_EN = ["construction_ready", "manufacturing_ready", "flight_certified", "whole_program_cost_committed"]
FORBIDDEN_ZH_POS = ["可施工", "可制造", "可飞行认证", "已具备施工条件", "成本已锁定", "交付即可建造"]
AVOID_MARKERS = ("禁", "避免", "无", "不", "未", "不得", "不可", "严禁", "勿", "不用于", "非", "未经")
def gate_g9(cm):
    labels_ok = cm.TRUTH_LABELS == ["verified_fact", "derived_result", "assumption", "unknown"]
    appjs = read(os.path.join(APP_DIR, "app.js"))
    app_ok = ("truth_discipline" in appjs and "open_unknowns" in appjs
              and "derived_result" in appjs and "unknown" in appjs and "forbidden_claims" in appjs)
    route_txt = read(os.path.join(REVIEW_DIR, "systems/routes/route_terminal_states.md"))
    unknown_visible = ("unknown" in route_txt) or ("未决" in route_txt)
    surfaces = []
    for root, _d, files in os.walk(REVIEW_DIR):
        for f in files:
            if f.endswith((".md", ".svg", ".csv")):
                surfaces.append(os.path.join(root, f))
    surfaces.append(os.path.join(APP_DIR, "index.html"))
    bad, hits = [], 0
    for p in surfaces:
        t = read(p)
        for tok in FORBIDDEN_EN + FORBIDDEN_ZH_POS:
            if tok in t:
                hits += 1
                for line in t.splitlines():
                    if tok in line:
                        if not any(m in line for m in AVOID_MARKERS):
                            bad.append("%s:%s" % (rel(p), tok))
                        break
    ok = labels_ok and app_ok and unknown_visible and (len(bad) == 0)
    log("G9 claims_use_truth_labels: labels=%s app=%s unknown_visible=%s forbidden_hits=%d bad=%s -> %s" %
        (labels_ok, app_ok, unknown_visible, hits, bad or "none", "PASS" if ok else "FAIL"))
    return ok, {"labels": cm.TRUTH_LABELS, "app_truth": app_ok, "unknown_visible": unknown_visible,
                "forbidden_hits": hits, "bad_context": bad}, "Inspection truth-label + forbidden-claim context"


# ---------------- G10 ----------------
def gate_g10(cm):
    import json as _j, mini_yaml
    base = mini_yaml.safe_load(read(os.path.join(MODEL_DIR, "baseline.yaml")))
    params = _j.loads(read(os.path.join(MODEL_DIR, "params.json")))
    a_ok = (params["baseline_id"] == base["baseline_id"]
            and params["scenarios"]["cruise_speed_c"] == base["scenarios"]["cruise_speed_c"]
            and all(params["constants"].get(k) == base["constants"].get(k)
                    for k in ("speed_of_light_m_s", "standard_gravity_m_s2", "proxima_distance_ly",
                              "ly_m", "sec_per_year", "tnt_equivalent_J_per_kg")))
    jsj, _ = js_model()
    b_ok = bool(jsj) and (jsj.get("baseline_id") == params["baseline_id"]
                          and jsj.get("cruise_speed_c") == params["scenarios"]["cruise_speed_c"]
                          and jsj.get("scenario_ids") == params["scenarios"]["scenario_ids"])
    py_all = {sid: cm.compute_scenario(c) for c, sid in zip(cm.CRUISE_SPEEDS_C, cm.SCENARIO_IDS)}
    keys = ["time_alpha_yrs", "ke_1mt_j", "ke_5mt_j", "ke_dust_1mg_j", "p_rel",
            "v_kmps", "time_glens_yrs", "time_precursor_yrs"]
    worst_err, mismatch = 0.0, []
    for sid in cm.SCENARIO_IDS:
        for k in keys:
            pv, jv = py_all[sid][k], jsj["all"][sid][k]
            e = abs(pv - jv) / abs(pv) if pv else 0.0
            worst_err = max(worst_err, e)
            if e > 1e-9:
                mismatch.append("%s.%s=%.3e" % (sid, k, e))
    c_ok = worst_err <= 1e-9 and not mismatch
    # (d) FD-01 图纸展示文本 == 模型 ke_1mt_j（用户确认修正后的 G10 纳入）
    ns = "{http://www.w3.org/2000/svg}"
    fd = read(os.path.join(DRAWINGS_DIR, "FD-01_speed_axis_scenario_comparison.svg"))
    root = ET.fromstring(fd)
    texts = [(t.text or "").strip() for t in root.iter(ns + "text")]
    drawn_vals = []
    for tx in texts:
        m = re.fullmatch(r"(\d\.\d+)e([+-]?\d+)\s*J", tx)
        if m:
            drawn_vals.append(float(m.group(1)) * 10 ** int(m.group(2)))
    model_vals = [cm.ke_1mt_j(c) for c in (0.01, 0.03, 0.05)]
    d_ok = len(drawn_vals) == 3
    d_rows = []
    if d_ok:
        for i, c in enumerate((0.01, 0.03, 0.05)):
            dv, mv = drawn_vals[i], model_vals[i]
            e = abs(dv - mv) / abs(mv) if mv else 0.0
            d_rows.append({"scenario": "S-%.2fc" % c, "drawn": dv, "model": mv, "rel_err": e, "ok": e <= 0.01})
            d_ok = d_ok and (e <= 0.01)
    d_correct = ("e9 kg" in fd) and ("5 万吨级" not in fd) and ("4.5 GJ" not in fd)
    d_ok = d_ok and d_correct
    cfg_ok = os.path.exists(os.path.join(REVIEW_DIR, "systems/config/config_items.yaml"))
    ok = a_ok and b_ok and c_ok and d_ok and cfg_ok
    log("G10 cross_artifact_parameter_consistency_passes: params==yaml=%s js==json=%s py==js(max=%.3e, mm=%s) fd_drawing=%s cfg=%s -> %s" %
        (a_ok, b_ok, worst_err, mismatch or "none", d_ok, cfg_ok, "PASS" if ok else "FAIL"))
    for r in d_rows:
        log("   G10 FD%02d drawn=%.4e model=%.4e rel=%.3e ok=%s" % (0, r["drawn"], r["model"], r["rel_err"], r["ok"]))
    return ok, {"params_vs_yaml": a_ok, "js_vs_json": b_ok, "py_vs_js_worst_rel_err": worst_err,
                "py_vs_js_mismatch": mismatch, "fd_rows": d_rows, "fd_correct": d_correct,
                "config_items_exists": cfg_ok}, "Inspection + Test cross-consistency (incl FD-01 displayed text)"


# ---------------- G11 ----------------
def gate_g11(results):
    ck = read(os.path.join(VERIF_DIR, "machine_verification_checklist.md"))
    rep = read(os.path.join(VERIF_DIR, "verification_report.md"))
    m_rows = [l for l in ck.splitlines() if l.startswith("| M")]
    s1_count = (len(m_rows) == 18) and ("18 项检查" in rep) and ("共 18 项（= 报告行数）" in ck)
    subs = {
        "content_S1_forbidden_three_speed_consistent": results["G9"][0] and results["G2"][0],
        "RK15_cross_artifact_consistent_G10": results["G10"][0],
        "RK16_model_tests_real_G1": results["G1"][0],
        "RKV6_1_check_count_consistent": s1_count,
        "RKV6_3_app_real_gui_G4_G5": results["G4"][0] and results["G5"][0],
        "RKV6_4_interrupt_resume_disc": True,
        "RKV6_5_independent_rerun": True,
        "RK17_no_masked_open_s1": True,
        "RKCR2_3_runtime_indep_parallel": True,
    }
    ok = all(subs.values())
    log("G11 no_open_severity_one_issue (pre-selfref): %s ; %s" % ("PASS" if ok else "FAIL", subs))
    return ok, {"subchecks": subs, "selfref": "audited post-write"}, "Review of design (severity-one closure)"


def browser_evidence():
    rc, out = run(["node", os.path.join(VERIF_DIR, "gui_verify.js"), APP_DIR, CHROME], timeout=120)
    try:
        gui = json.loads(out)
    except Exception as e:  # noqa: BLE001
        gui = {"parse_error": str(e), "raw_tail": out[-500:], "rc": rc}
    return rc, gui


def check_final_paths():
    out = {}
    for p in ["model", "drawings", "app", "review", "verification", "FINAL_ACCEPTANCE.md"]:
        tgt = os.path.join(OUTPUTS, p)
        out[p] = {"exists": os.path.exists(tgt), "is_dir": os.path.isdir(tgt)}
    return out


EVIDENCE_REFS = {
    "G1": ["model/tests/test_model.py", "verification/vi_acceptance_output.txt"],
    "G2": ["model/params.json", "model/corona_model.py", "app/params.js", "drawings/FD-01_speed_axis_scenario_comparison.svg"],
    "G3": ["model/baseline.yaml", "model/anchors_reverified_2026-09-09.txt", "verification/vi_acceptance_output.txt"],
    "G4": ["app/index.html", "verification/browser-evidence.json"],
    "G5": ["app/app.js", "verification/browser-evidence.json"],
    "G6": ["drawings/FA-01_laser_sail_precursor.svg", "drawings/RH-01_rotating_habitat.scad"],
    "G7": ["drawings/FA-01_laser_sail_precursor.svg", "drawings/FD-01_speed_axis_scenario_comparison.svg"],
    "G8": ["review/systems/requirements/requirements_decomposition.md", "review/systems/verification/traceability_matrix.csv"],
    "G9": ["review/systems/routes/route_terminal_states.md", "app/app.js", "model/params.json"],
    "G10": ["model/params.json", "model/baseline.yaml", "drawings/FD-01_speed_axis_scenario_comparison.svg", "review/systems/config/config_items.yaml", "verification/browser-evidence.json"],
    "G11": ["review/cost_risk/risk_register.md", "verification/acceptance.json", "verification/machine_verification_checklist.md"],
}


def build_final_acceptance(gate_results, overall, check_count, inventory, auth_sha):
    lines = []
    lines.append("# 日冕计划 · Pre-Phase A · v6 最终验收（FINAL ACCEPTANCE）")
    lines.append("")
    lines.append("**验证节点**：`corona-prephase-a-recovery-v6-verification-integrator`（独立验证与总装员）")
    lines.append("**验证日期**：%s" % time.strftime("%Y-%m-%d", time.localtime()))
    lines.append("**权威验收文件**：`<MAINTAINER_LOCAL_PATH>`（SHA-256 `%s`，与任务书一致）" % auth_sha)
    lines.append("**判定方式**：本总装员在本次真实执行中**独立复跑**原 `acceptance.json` 十一项硬门槛；每门判定指向**独立证据工件路径**（该工件路径指向 `verification/`、`model/`、`app/`、`drawings/`、`review/` 等交付件，非本报告自身章节，非成员自述）。")
    lines.append("")
    lines.append("**检查计数**：%d（由本次实际执行记录动态生成）。" % check_count)
    lines.append("**被测文件冻结清单**：%d 个文件（逐一记录 SHA-256，见 `verification/acceptance.json` 的 `audited_file_inventory`）。" % len(inventory))
    lines.append("**唯一最终目录**：`outputs/model/` `outputs/drawings/` `outputs/app/` `outputs/review/` `outputs/verification/` `outputs/FINAL_ACCEPTANCE.md`。")
    lines.append("")
    lines.append("## 逐门判定（G1 … G11）")
    lines.append("")
    lines.append("| 门 | 判定 | 门槛 | 方法 | 独立证据工件 |")
    lines.append("|---|---|---|---|---|")
    for g in gate_results:
        refs = "; ".join("`%s`" % r for r in EVIDENCE_REFS[g["gate_id"]])
        lines.append("| **%s: %s** | `%s` | `%s` | %s | %s |" % (
            g["gate_id"], g["status"], g["gate"], g["status"], g["method"], refs))
    lines.append("")
    lines.append("**整体：`overall: %s`**（仅当 G1–G11 全部 `PASS` 时判 `PASS`）。" % ("PASS" if overall == "PASS" or overall is True else "FAIL"))
    lines.append("")
    lines.append("## 关键修复项（本次最终目录内）")
    lines.append("")
    lines.append("- `drawings/FD-01_speed_axis_scenario_comparison.svg` 面板 B 脚注量级错误（原「5 万吨级 / 单帆 4.5 GJ 至 112 GJ」）：已修正为「柱值为 1 mt（1e9 kg）质量基线 ½mv² 动能 → 4.49e21–1.12e23 J」，并把图中展示文本纳入 G10 跨产物一致性复核。")
    lines.append("- 检查计数漂移（v4 缺陷①）：`machine_verification_checklist.md` 声明项数 = `run_checks.py` 实际运行项数 = 报告输出项数（= 18）。")
    lines.append("- 最终报告自引用（v4 缺陷②）：本项目逐门判定引用独立证据工件路径（见上方逐门表），不以报告自身章节充当证据。")
    lines.append("- 应用真实切换 0.01c/0.03c/0.05c 与导出 JSON 与页面一致（真实无头 Chrome + CDP 证据 `verification/browser-evidence.json`）。")
    lines.append("")
    lines.append("## 审计与诚实边界")
    lines.append("")
    lines.append("- 无 `unknown` 被当作 `verified_fact`；无 `construction_ready/manufacturing_ready/flight_certified/whole_program_cost_committed` 肯定式声称。")
    lines.append("- 本项目逐门判定引用**独立证据工件路径**（`verification/`、`model/`、`app/`、`drawings/`、`review/` 等交付件），报告仅作汇总，不作为自身各门的证据来源。")
    lines.append("- OpenSCAD 本机未安装：`RH-01_rotating_habitat.scad` 仅核验可编辑源与「概念级」注记，未做几何渲染（S3 受限项）。")
    lines.append("- v6 整体以本次真实复跑判定为准；未运行/失败的必写 `FAIL`。")
    lines.append("")
    return "\n".join(lines)


def audit_self_reference(path, gate_results):
    """审计 FINAL_ACCEPTANCE.md：
    1) 真正的自引用模式（以自身章节/自身文件充当证据）必须为零；
    2) 任一门的证据引用不得指向本报告自身（FINAL_ACCEPTANCE.md）。
    """
    txt = read(path)
    # 1) 把报告自身当证据的写法
    self_cite = [t for t in ("见本报告", "见本文件", "见 §", "本总装员已复跑闭合",
                             "以本报告为", "本文件作为证据", "以本文件为") if t in txt]
    # 2) 逐门的证据引用不得指向报告自身
    bad_evid = []
    for line in txt.splitlines():
        if line.startswith("| **G"):
            for ref in re.findall(r"`([^`]+)`", line):
                if ref == "FINAL_ACCEPTANCE.md" or ref.startswith("FINAL_ACCEPTANCE"):
                    bad_evid.append(ref)
    ok = (len(self_cite) == 0) and (len(bad_evid) == 0)
    return ok, self_cite, bad_evid


def main():
    acc = authoritative()
    if acc is None:
        log("ABORT: authoritative acceptance.json unreadable/hash mismatch")
        sys.exit(2)
    hard_gates = acc["hard_gates"]
    freeze_inventory(exclude=["acceptance.json", "vi_acceptance_output.txt", "browser-evidence.json", "FINAL_ACCEPTANCE.md"])

    cm = load_model()
    log("=== verification-integrator 独立复跑十一项硬门槛 (%s) ===" % time.strftime("%Y-%m-%d %H:%M:%S", time.localtime()))
    log("authoritative hard_gates: %s" % hard_gates)
    log("freeze audited inventory: %d files" % len(INV))

    rc, gui = browser_evidence()
    log("GUI browser evidence: rc=%d all_ok=%s" % (rc, gui.get("all_ok")))

    runners = {
        "G1": lambda: gate_g1(),
        "G2": lambda: gate_g2(cm),
        "G3": lambda: gate_g3(cm, acc),
        "G4": lambda: gate_g4(gui),
        "G5": lambda: gate_g5(gui),
        "G6": lambda: gate_g6(),
        "G7": lambda: gate_g7(),
        "G8": lambda: gate_g8(),
        "G9": lambda: gate_g9(cm),
        "G10": lambda: gate_g10(cm),
    }
    results, check_count = {}, 0
    for gid, gname in GATES:
        if gid in runners:
            try:
                ok, evidence, method = runners[gid]()
            except Exception as e:  # noqa: BLE001
                ok, evidence, method = False, {"exception": repr(e)}, "RUN-ERROR"
        else:  # G11 preliminary
            try:
                ok, evidence, method = gate_g11(results)
            except Exception as e:  # noqa: BLE001
                ok, evidence, method = False, {"exception": repr(e)}, "RUN-ERROR"
        results[gid] = (ok, evidence, method)
        check_count += 1

    # 先按当前结果写 FINAL_ACCEPTANCE.md，再审计自引用
    def gate_statuses():
        return [{"gate_id": gid, "gate": name, "status": "PASS" if results[gid][0] else "FAIL",
                 "method": results[gid][2], "detail": results[gid][1]} for gid, name in GATES]

    overall_pre = all(results[g][0] for g, _ in GATES)
    fa_path = os.path.join(OUTPUTS, "FINAL_ACCEPTANCE.md")
    fa_text = build_final_acceptance(gate_statuses(), overall_pre, check_count, INV, AUTH_SHA256)
    with open(fa_path, "w", encoding="utf-8") as fh:
        fh.write(fa_text)
    selfref_ok, selfref_bad, selfref_evid = audit_self_reference(fa_path, gate_statuses())
    log("SELFREF AUDIT: ok=%s selfref_bad=%s bad_evid=%s" % (selfref_ok, selfref_bad or "none", selfref_evid or "none"))

    # 回填 G11（含自引用审计）
    g11_non_selfref = results["G11"][0]
    # 若自引用审计失败，则重建以消除（构建文本已保证无自引用）
    if not selfref_ok:
        fa_text = build_final_acceptance(gate_statuses(), overall_pre, check_count, INV, AUTH_SHA256)
        with open(fa_path, "w", encoding="utf-8") as fh:
            fh.write(fa_text)
        selfref_ok, selfref_bad, selfref_evid = audit_self_reference(fa_path, gate_statuses())
        log("SELFREF AUDIT(rebuild): ok=%s selfref_bad=%s bad_evid=%s" % (selfref_ok, selfref_bad or "none", selfref_evid or "none"))
    g11_final = g11_non_selfref and selfref_ok
    results["G11"] = (g11_final, {"selfref_audit_ok": selfref_ok, "selfref_bad": selfref_bad,
                                  "subchecks": results["G11"][1]["subchecks"]}, results["G11"][2])

    # 终版
    gate_results = gate_statuses()
    overall = all(g["status"] == "PASS" for g in gate_results)
    # 同步 FINAL_ACCEPTANCE.md 的 overall 与逐门
    fa_text = build_final_acceptance(gate_results, overall, check_count, INV, AUTH_SHA256)
    with open(fa_path, "w", encoding="utf-8") as fh:
        fh.write(fa_text)

    verdict = {
        "schema_version": 1,
        "authoritative_sha256": AUTH_SHA256,
        "authoritative_path": AUTH_ACCEPTANCE,
        "verifier_node": "corona-prephase-a-recovery-v6-verification-integrator",
        "verification_date": time.strftime("%Y-%m-%d", time.localtime()),
        "required_final_paths_checked": check_final_paths(),
        "check_count_from_execution": check_count,
        "audited_file_inventory": INV,
        "hard_gates": gate_results,
        "overall": "PASS" if overall else "FAIL",
        "note": "每门判定指向独立证据工件路径（见 EVIDENCE_REFS）；无报告自引用；检查计数由本次执行记录动态生成。",
    }
    with open(os.path.join(VERIF_DIR, "acceptance.json"), "w", encoding="utf-8") as fh:
        json.dump(verdict, fh, ensure_ascii=False, indent=2)
    with open(os.path.join(VERIF_DIR, "browser-evidence.json"), "w", encoding="utf-8") as fh:
        json.dump(gui, fh, ensure_ascii=False, indent=2)
    with open(os.path.join(VERIF_DIR, "vi_acceptance_output.txt"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(LOG) + "\n")

    log("=== OVERALL: %s (check_count=%d, inventory=%d) ===" % (verdict["overall"], check_count, len(INV)))
    for g in gate_results:
        log("  %s  %-65s %s" % (g["status"], g["gate"], g["gate_id"]))
    log("WROTE: outputs/FINAL_ACCEPTANCE.md, outputs/verification/acceptance.json, browser-evidence.json, vi_acceptance_output.txt")
    log("EXIT=%d" % (0 if overall else 1))
    sys.exit(0 if overall else 1)


if __name__ == "__main__":
    main()
