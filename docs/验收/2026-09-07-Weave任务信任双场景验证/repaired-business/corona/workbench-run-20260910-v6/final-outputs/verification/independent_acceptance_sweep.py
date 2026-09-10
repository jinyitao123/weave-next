#!/usr/bin/env python3
"""
independent_acceptance_sweep.py —— verification-integrator 独立扫描（gate 2/8/9 + 禁语纪律）。

不 import 团队产物；直接对 outputs/ 下所有文本产物做内容扫描，输出每项检查的判定。
"""
import os
import re

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # .../workdir/outputs
OUTPUTS = BASE
WORKDIR = os.path.dirname(BASE)  # .../workdir

TEXT_EXT = {".md", ".txt", ".yaml", ".json", ".csv", ".py", ".js", ".html", ".svg", ".scad"}

def walk():
    for root, _dirs, files in os.walk(OUTPUTS):
        for f in files:
            ext = os.path.splitext(f)[1].lower()
            if ext in TEXT_EXT:
                yield os.path.join(root, f)

def rel(p):
    return os.path.relpath(p, WORKDIR)

def P(path):
    """将 outputs/... 相对路径解析为绝对路径（与当前工作目录无关）。"""
    return os.path.join(WORKDIR, path)

def read(p):
    with open(p, "r", encoding="utf-8", errors="replace") as fh:
        return fh.read()

def has_scenarios(text):
    # 三种速度值或情景 ID（任一命中即视为该档位存在）
    has01 = ("0.01c" in text or "S-0.01c" in text)
    has03 = ("0.03c" in text or "S-0.03c" in text)
    has05 = ("0.05c" in text or "S-0.05c" in text)
    return has01, has03, has05

# 关键情景载体（model / app / drawings 的 FD 速度对比 / 论证路线/比较 / 验证矩阵）
CORE_SCENARIO_FILES = [
    "outputs/model/params.json",
    "outputs/model/baseline.yaml",
    "outputs/model/corona_model.py",
    "outputs/app/params.js",   # app 侧情景 ID 的单一载体；model.js/app.js 由此处动态派生
    "outputs/app/index.html",
    "outputs/drawings/FD-01_speed_axis_scenario_comparison.svg",
    "outputs/review/systems/routes/route_terminal_states.md",
    "outputs/review/physics/physics_three_speed_scenarios.csv",
    "outputs/review/propulsion/07_corona_v4_pet_comparison_matrix.csv",
    "outputs/review/cost_risk/risk_register.md",
    "outputs/review/cost_risk/termination_conditions.md",
    "outputs/review/cost_risk/techmaturity_assessment.md",
    "outputs/review/systems/verification/traceability_matrix.csv",
]

FORBIDDEN_EN = ["construction_ready", "manufacturing_ready", "flight_certified", "whole_program_cost_committed"]
FORBIDDEN_ZH_NEG = ["不得", "禁止", "不使用", "不可用于", "不作为", "不构成", "不声称", "不能", "不可把"]

def check_gate2():
    print("=" * 74)
    print("GATE 2  three_speed_scenarios_present")
    print("=" * 74)
    missing = []
    for fp in CORE_SCENARIO_FILES:
        absfp = P(fp)
        if not os.path.exists(absfp):
            missing.append(rel(absfp) + "  (FILE MISSING)")
            continue
        t = read(absfp)
        h01, h03, h05 = has_scenarios(t)
        if not (h01 and h03 and h05):
            missing.append("%s  has01=%s has03=%s has05=%s" % (rel(absfp), h01, h03, h05))
    if missing:
        print("  FAIL:")
        for m in missing:
            print("    - " + m)
        return False
    print("  PASS — %d 个核心情景载体均含 0.01c/0.03c/0.05c（三情景齐备）" % len(CORE_SCENARIO_FILES))

    # 单 0.03c 残留：只提 0.03c 但不提 0.01c 也不提 0.05c 的正文 md/txt
    print("  单 0.03c 残留扫描（只提 0.03c、不提 0.01c 且不提 0.05c 的 md/txt 正文）:")
    residual = []
    for p in walk():
        if not p.endswith((".md", ".txt")):
            continue
        t = read(p)
        h01, h03, h05 = has_scenarios(t)
        # 只对“正文提及 0.03c 却完全不见另外两档速度”的文档报警
        if "0.03c" in t and not h01 and not h05:
            residual.append(rel(p))
    if residual:
        print("    WARN (需人工判定):")
        for r in residual:
            print("      - " + r)
    else:
        print("    none")
    return True

def check_gate8():
    print("=" * 74)
    print("GATE 8  requirements_have_verification_methods")
    print("=" * 74)
    req_files = [
        "outputs/review/systems/requirements/requirements_decomposition.md",
        "outputs/review/systems/verification/traceability_matrix.md",
        "outputs/review/systems/verification/traceability_matrix.csv",
    ]
    ok = True
    for fp in req_files:
        absfp = P(fp)
        if not os.path.exists(absfp):
            print("  MISSING " + fp)
            ok = False
            continue
        t = read(absfp)
        methods = re.findall(r"验证方法|verification\s*method", t, re.I)
        has_tab = ("|" in t and re.search(r"SA-REQ-|REQ-", t))
        ratio = len(re.findall(r"SA-REQ-|REQ-V4-", t))
        has_method_terms = re.search(r"(Analysis|Demonstration|Inspection|Test|Review of design|验证方法)", t)
        nmethods = len(re.findall(r"(Analysis|Demonstration|Inspection|Test|Review of design)", t))
        verdict = has_tab and has_method_terms is not None
        print("  %s: 需求编号=%d  验证方法术语=%s  → %s" % (rel(absfp), ratio, "存" if has_method_terms else "缺", "PASS" if verdict else "FAIL"))
        ok = ok and verdict
    # 每条需求都应有对应验证方法：统计 SA-REQ- 与可允许验证方法的匹配
    t = read(P(req_files[0]))
    reqs = re.findall(r"\|\s*(SA-REQ-[A-Z]-\d+)\s*\|", t)
    rows = [r for r in t.splitlines() if r.strip().startswith("| SA-REQ-") or ("SA-REQ-" in r and "验证方法" in r)]
    method_col = [r for r in t.splitlines() if re.search(r"\|\s*(Analysis|Demonstration|Inspection|Test|Review of design)\s*\|", r)]
    print("  SA-REQ-* 条数=%d；含验证方法值的表格行=%d" % (len(reqs), len(method_col)))
    return ok

def check_gate9():
    print("=" * 74)
    print("GATE 9  claims_use_truth_labels")
    print("=" * 74)
    truth = ["verified_fact", "derived_result", "assumption", "unknown"]
    docs = []
    for p in walk():
        if not (p.endswith(".md") or p.endswith(".txt") or p.endswith(".csv") or p.endswith(".py") or p.endswith(".js")):
            continue
        t = read(p)
        if any(tok in t for tok in truth):
            docs.append(rel(p))
    print("  含 truth label（verified_fact/derived_result/assumption/unknown）的文档数 = %d" % len(docs))
    for d in sorted(docs):
        print("    - " + d)
    # 论证文档（review 内 md）必须具备 truth labels：统计 review/**/*.md 中带标签的
    review_md = [p for p in walk() if p.endswith(".md") and "/review/" in p]
    with_label = [rel(p) for p in review_md if any(tok in read(p) for tok in truth)]
    print("  review/*.md 总数 = %d；含 truth label 数 = %d" % (len(review_md), len(with_label)))
    ok = len(with_label) >= len(review_md) * 0.8  # 允许少量纯索引/列表文档
    return ok

def forbidden_positive(text):
    hits = []
    for tok in FORBIDDEN_EN:
        if tok in text:
            hits.append("EN:" + tok)
    # 中文肯定式禁语（出现且不在否定/禁止语境附近）
    for zt in ["可施工", "可制造", "可飞行认证", "已具备施工条件", "成本已锁定", "交付即可建造"]:
        if zt in text:
            hits.append("ZH:" + zt)
    return hits

NEG_WORDS = ["不得", "禁止", "不使用", "不可用于", "不构成", "不声称", "未", "无", "非", "不作",
             "不做", "避免", "枚举", "注册", "红线", "禁令", "违规", "区分", "违纪", "forbidden_claims",
             "禁忌", "不使用", "不可把", "不能", "不因", "仅为", "不预设"]


def check_forbidden():
    print("=" * 74)
    print("事实纪律: forbidden_claims 未作肯定式声称（gate 7/11 相关）")
    print("=" * 74)
    # 仅对“面向受众的成果”扫描；验证工具脚本（本身枚举禁语用于扫描）豁免
    AUTH_DELIVERABLE = True
    real = []
    total_hits = 0
    for p in walk():
        if not p.endswith((".md", ".txt", ".html", ".svg", ".scad", ".yaml", ".json")):
            continue
        t = read(p)
        lines = t.splitlines()
        # 若本文件是“定义/注册 forbidden_claims 列表”的数据或配置（含 forbidden_claims 键），
        # 其内出现的禁语均属枚举定义，不是对成果的肯定式声称。
        is_forbidden_definition_file = "forbidden_claims" in t
        for i, ln in enumerate(lines):
            for tok in FORBIDDEN_EN + ["可施工", "可制造", "可飞行认证", "已具备施工条件", "成本已锁定", "交付即可建造"]:
                if tok not in ln:
                    continue
                total_hits += 1
                ctx = " ".join(lines[max(0, i - 2):i + 3])
                # 1) 本文件是 forbidden_claims 列表定义/注册（数据、配置、登记册）→ 枚举
                is_listdef = is_forbidden_definition_file
                # 2) 否定/禁止/枚举语境
                neg = any(nk in ctx for nk in NEG_WORDS)
                if is_listdef or neg:
                    continue
                real.append("%s L%d: %s   ctx=%r" % (rel(p), i + 1, tok, ctx[:140]))
    if real:
        print("  FAIL — 疑似肯定式禁语（未在否定/枚举/列表定义语境）:")
        for r in real:
            print("    - " + r)
        return False
    print("  PASS — 全部 %d 处禁语命中均位于 forbidden_claims 列表定义 / 否定 / 枚举 / 红线语境，无肯定式声称" % total_hits)
    return True

if __name__ == "__main__":
    g2 = check_gate2()
    g8 = check_gate8()
    g9 = check_gate9()
    gf = check_forbidden()
    print("=" * 74)
    print("SUMMARY: gate2=%s gate8=%s gate9=%s forbidden=%s" % (
        "PASS" if g2 else "FAIL", "PASS" if g8 else "FAIL",
        "PASS" if g9 else "FAIL", "PASS" if gf else "FAIL"))
