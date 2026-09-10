"""跨产物一致性检查（有界退出）。从 outputs/verification 目录执行:
   python3 cross_artifact_check.py
覆盖: 基线摘要、追溯键嵌入、图纸数值与模型结果一致、概念级标注、
      0.01c/0.05c 仅出现于 CCR-001 注记语境、应用导出 JSON 与模型一致。
"""
import json, re, hashlib, os, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # outputs/
ok = True
def rep(name, cond, detail=""):
    global ok
    print(("PASS" if cond else "FAIL"), name, detail)
    ok = ok and cond

DIGEST = "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2"
SHORT = "cfb12b78"

def R(*p): return os.path.join(ROOT, *p)

actual = hashlib.sha256(open(R("model", "baseline_frozen.yaml"), "rb").read()).hexdigest()
rep("baseline digest", actual == DIGEST, actual[:16] + "…")

targets = {
 "drawings/DRW-ARCH-001_mission_architecture.svg": [SHORT, "corona-baseline-1.0.0", "概念级"],
 "drawings/DRW-ARCH-002_archive_payload_zones.svg": [SHORT, "corona-baseline-1.0.0", "概念级"],
 "drawings/DRW-ARCH-003_system_interfaces.svg": [SHORT, "corona-baseline-1.0.0", "概念级"],
 "drawings/CAD-HAB-001_concept_habitat.scad": [SHORT, "corona-baseline-1.0.0"],
 "app/index.html": [DIGEST, "corona-baseline-1.0.0"],
 "app/corona_core.js": [DIGEST, "corona-baseline-1.0.0"],
 "app/sample_export_scenario.json": [DIGEST, "corona-baseline-1.0.0"],
 "model/corona_model.py": [DIGEST],
 "model/results.json": [DIGEST],
}
for rel, needles in targets.items():
    txt = open(R(rel), encoding="utf-8").read()
    missing = [n for n in needles if n not in txt]
    rep("trace keys in outputs/" + rel, not missing, ("missing " + str(missing)) if missing else "")

res = json.load(open(R("model", "results.json"), encoding="utf-8"))
svg_all = ""
svg_files = [f for f in os.listdir(R("drawings")) if f.endswith(".svg")]
for f in svg_files:
    svg_all += open(R("drawings", f), encoding="utf-8").read()
rep("SVG count >= 3", len(svg_files) >= 3, str(len(svg_files)))
rep("SVG: travel 141.67", "141.67" in svg_all)
rep("SVG: KE 4.0444e22", "4.0444e22" in svg_all or "4.0443983e22" in svg_all)
rep("SVG: gravity 43.86", "43.8649" in svg_all or "43.86" in svg_all)
rep("SVG: dust 40.4MJ/4.04e7", ("40443983" in svg_all) or ("4.0444e7" in svg_all) or ("40.4MJ" in svg_all))
rep("SVG: conceptual marking on all 3", svg_all.count("概念级") >= 3)

bad = []
for f in svg_files:
    txt = open(R("drawings", f), encoding="utf-8").read()
    for m in re.finditer(r"0\.0[15]c", txt):
        ctx = txt[max(0, m.start() - 90):m.end() + 90]
        if not any(k in ctx for k in ("CCR-001", "待平台确认", "pending", "未纳入", "三情景", "待确认变更")):
            bad.append((f, ctx[:50]))
rep("SVG: 0.01c/0.05c only in CCR-001 note context", not bad, str(bad[:2]))

# 模型源码与图纸中 forbidden_claims 只出现在“禁用/不得”语境（按整行判定）
for rel in ("app/index.html", "model/corona_model.py"):
    for i, line in enumerate(open(R(rel), encoding="utf-8")):
        for fc in ("construction_ready", "manufacturing_ready", "flight_certified", "whole_program_cost_committed"):
            if fc in line:
                rep("%s line-context in %s:%d" % (fc, rel, i + 1),
                    any(k in line for k in ("禁用", "不得", "forbidden", "非", "不构成")), "")

exp = json.load(open(R("app", "sample_export_scenario.json"), encoding="utf-8"))
rep("export travel_time == model", abs(exp["outputs"]["travel_time_yr"]["value"] - res["travel_time"]["travel_time_yr"]) < 1e-6)
rep("export KE_1mt == model", abs(exp["outputs"]["kinetic_energy_classical_j"]["value"] - res["kinetic_energy"]["crewed_1mt_classical_j"]) < 1e12)

print("CROSS-ARTIFACT:", "ALL-PASS" if ok else "HAS-FAILURES")
sys.exit(0 if ok else 1)
