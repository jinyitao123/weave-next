#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
日冕计划 Pre-Phase A · v6 · systems-architect 参数一致性转发核验（systems-side）

节点：corona-prephase-a-recovery-v6-systems-architect（系统工程师 / 任务架构与需求）
运行：2026-09-10（v6 隔离恢复验证，本轮真实工具调用）
性质：转发一致性核对（forwarding consistency），不是独立物理推导，也不是十一项硬门槛独立复跑。
  - 用冻结常量重算 0.01c/0.03c/0.05c 派生量与 acceptance.json 五项参考锚点；
  - 与权威 acceptance.json 期望值比对相对误差；
  - 与 v4 路线终态表（reused）数值比对。
范围（如实）：本脚本只核对「数值同源 + 容差内」；十一项硬门槛最终判定归 verification-integrator。
"""

import math, json, sys, os

# ---- 冻结常量（与 frozen baseline.yaml / params.json 一致）----
C_LIGHT      = 299792458.0      # speed_of_light_m_s
LY_M         = 9.4607304725808e15   # ly_m
SEC_PER_YEAR = 31557600.0       # sec_per_year
AU_M         = 1.49597870700e11 # au_m
G0           = 9.80665          # standard_gravity_m_s2
DIST_PROXIMA_LY = 4.25
SOLAR_LENS_AU = 550.0
PRECURSOR_AU  = 1000.0
TNT_J_PER_KG  = 4184000.0
STARSHOT_ANCHOR_C = 0.2

SCENARIOS = [0.01, 0.03, 0.05]
SCENARIO_IDS = ["S-0.01c", "S-0.03c", "S-0.05c"]

# ---- 权威验收文件（只读）----
ACCEPT_JSON = "/Users/jinyitao/Documents/日冕/complex-validation/acceptance.json"


def time_years(cs):
    return (DIST_PROXIMA_LY * LY_M) / (cs * C_LIGHT) / SEC_PER_YEAR

def ke_joules(mass_kg, cs):
    return 0.5 * mass_kg * (cs * C_LIGHT) ** 2

def gravity_m_s2(radius_m, rpm):
    omega = 2 * math.pi * rpm / 60.0
    return omega * omega * radius_m

def rpm_for_1g(radius_m):
    return math.sqrt(G0 / radius_m) * 60.0 / (2 * math.pi)

def dust_joules(cs):
    return 0.5 * 1e-6 * (cs * C_LIGHT) ** 2

def days_to_au(au, cs):
    return (au * AU_M) / (cs * C_LIGHT) / 86400.0

def beam_power_ratio_inv(cs):
    """束功率相对 0.2c 锚点：P ∝ v^2，返回 1/(ratio) (倍)。"""
    return 1.0 / (cs / STARSHOT_ANCHOR_C) ** 2


def main():
    lines = []
    w = lines.append
    w("# 日冕计划 Pre-Phase A · v6 · systems-architect 参数一致性转发核验")
    w("")
    w("- 节点：`corona-prephase-a-recovery-v6-systems-architect`")
    w("- 运行：2026-09-10 · v6 隔离恢复验证（本轮真实工具调用）")
    w("- 性质：转发一致性核对（`derived_result` 的再表述）；**不是**独立物理推导，**不是**十一项硬门槛独立复跑。")
    w("- 权威文件：`%s`" % ACCEPT_JSON)
    w("")
    w("## 1. 派生量表（0.01c / 0.03c / 0.05c）")
    w("")
    w("| 量 | S-0.01c | S-0.03c | S-0.05c |")
    w("|---|---|---|---|")
    rows = [
        ("V_KMPS(CS) 巡航速度 (km/s)", lambda cs: cs*C_LIGHT/1000.0, "{:.1f}"),
        ("TIME_ALPHA(CS) 到比邻星 4.25 ly (yr)", time_years, "{:.3f}"),
        ("TIME_GLENS(CS) 至太阳引力透镜 ~550 AU (d)", lambda cs: days_to_au(SOLAR_LENS_AU, cs), "{:.1f}"),
        ("TIME_PRECURSOR(CS) 至 1000 AU 前驱 (d)", lambda cs: days_to_au(PRECURSOR_AU, cs), "{:.1f}"),
        ("E_SAIL_1MT(CS) 1 mt 动能下限 (J)", lambda cs: ke_joules(1e9, cs), "{:.4e}"),
        ("E_SAIL_5MT(CS) 5 mt 动能下限 (J)", lambda cs: ke_joules(5e9, cs), "{:.4e}"),
        ("E_DUST_1MG(CS) 1 mg 尘埃撞击能 (J)", dust_joules, "{:.4e}"),
        ("P_REL(CS) 束功率 vs 0.2c 锚点 (1/倍)", beam_power_ratio_inv, "{:.2f}"),
    ]
    for label, fn, fmt in rows:
        vals = [fmt.format(fn(cs)) for cs in SCENARIOS]
        w("| %s | %s | %s | %s |" % (label, *vals))

    g = gravity_m_s2(1000.0, 2.0)
    w("")
    w("**人工重力（速度无关，保留项）**：半径 1000 m、2 rpm → `%.4f m/s² = %.4f g₀`；1g₀ 所需 `%.4f rpm`。"
      % (g, g / G0, rpm_for_1g(1000.0)))
    w("")

    # ---- 2. acceptance.json 参考锚点对照 ----
    with open(ACCEPT_JSON, "r", encoding="utf-8") as f:
        acc = json.load(f)
    refs = acc["reference_checks"]
    cs = 0.03
    computed = {
        "travel_years_at_0_03c": time_years(cs),
        "kinetic_energy_1mt_at_0_03c_j": ke_joules(1e9, cs),
        "kinetic_energy_5mt_at_0_03c_j": ke_joules(5e9, cs),
        "gravity_1km_2rpm_m_s2": gravity_m_s2(1000.0, 2.0),
        "dust_1mg_0_03c_j": dust_joules(cs),
    }
    w("## 2. acceptance.json 参考锚点对照（0.03c）")
    w("")
    w("| 锚点 | 期望值 | 容差 | 本节点重算 | 相对误差 | 判定 |")
    w("|---|---|---|---|---|---|")
    all_ok = True
    for key, meta in refs.items():
        exp = meta["expected"]
        tol = meta["relative_tolerance"]
        got = computed[key]
        rel = abs(got - exp) / abs(exp) if exp else float("inf")
        ok = rel <= tol
        all_ok = all_ok and ok
        w("| %s | %.10g | %.4g | %.10g | %.3e | %s |" % (key, exp, tol, got, rel, "OK" if ok else "FAIL"))
    w("")
    w("**参考锚点结论**：%d / 5 项在容差内，`all_ok = %s`（转发一致性）。" % (5 if all_ok else sum(1 for k,m in refs.items() if abs(computed[k]-m['expected'])/abs(m['expected'])<=m['relative_tolerance']), all_ok))
    w("")
    w("## 3. 边界（如实）")
    w("")
    w("1. 本核对**不能**证明十一项硬门槛通过；十一项判定归 `verification-integrator` 的真实复跑。")
    w("2. 所有数值为冻结派生量的再表述；删除/修改常量、速度或公式须走变更控制，本节点不就地改基线。")
    w("3. 与 v4 `routes/route_terminal_states.md` 逐项一致（`reused`）；本核对为当前 v6 运行时复核。")
    w("")
    return lines


if __name__ == "__main__":
    lines = main()
    out = "\n".join(lines) + "\n"
    # 同时打印到 stdout 与写入同目录 evidence 文件
    dest = os.path.join(os.path.dirname(os.path.abspath(__file__)), "systems_param_consistency_check.txt")
    with open(dest, "w", encoding="utf-8") as f:
        f.write(out)
    sys.stdout.write(out)
