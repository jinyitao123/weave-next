#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
v6 · mission-physics-analyst 独立复核脚本
本脚本为「自包含、从第一性原理」的独立复算，不 import 任何 v4 物理模块，
手工重写全部闭合式，用于交叉核验 v4 冻结物理成果（06_corona_v4_physics_analysis_v1.0.md
及其 physics_model.py / physics_recompute.py / physics_three_speed_scenarios.csv）是否正确。

回判定源：只读权威 /Users/jinyitao/Documents/日冕/complex-validation/acceptance.json 的
reference_checks 五项，以及 v4-outputs/model/baseline.yaml 冻结协定系数。
真值标签：verified_fact / derived_result / assumption / unknown
"""
import math, json, os

# ============ [A] 冻结常量（与 baseline.yaml / acceptance.json 逐值一致）============
C_LIGHT = 299792458.0        # m/s  verified_fact
G0      = 9.80665            # m/s^2 verified_fact
DIST_PROXIMA_LY = 4.25       # ly    verified_fact
LY_M    = 9460730472580800.0 # m/ly  verified_fact
SEC_PER_YEAR = 31557600.0    # s/yr  verified_fact (365.25 d)
TNT_J_PER_KG = 4184000.0     # J/kg  assumption（基线约定）
AU_M    = 149597870700.0     # m/AU  verified_fact
CRUISE_C = [0.01, 0.03, 0.05]
STARSHOT_V_C = 0.2           # B级 assumption/magnitude 锚点
SUN_GLENS_AU = 550.0
HELIOPAUSE_AU = 120.0
PRECURSOR_AU = 1000.0
M_CREW_1MT_KG = 1.0e9        # 1e9 kg = 百万吨级质量参考
M_ORBITAL_5MT_KG = 5.0e9     # 5e9 kg
M_DUST_1MG_KG = 1.0e-6
M_SAIL_1G_KG = 1.0e-3

# ============ [B] 闭合式（手工重写，公式复核）============
def v_mps(beta):   return beta * C_LIGHT                     # [L T^-1]
def v_kmps(beta):  return v_mps(beta) / 1000.0               # [L T^-1]
def time_alpha_years(beta):
    # 因 1 ly = c·1 yr => t(yr) = D / beta, D=4.25 ly
    return DIST_PROXIMA_LY / beta                            # [T]
def ke_joules(mass_kg, beta):   return 0.5 * mass_kg * v_mps(beta)**2  # [M L^2 T^-2]=J
def dust_ke_joules(beta):       return ke_joules(M_DUST_1MG_KG, beta)
def tnt_kg(joules):             return joules / TNT_J_PER_KG
def artgrav_a(radius_m, rpm):
    omega = 2.0*math.pi*(rpm/60.0); return omega*omega*radius_m   # [L T^-2]
def artgrav_rpm_for_g(radius_m, g):
    omega = math.sqrt(g/radius_m); return 60.0*omega/(2.0*math.pi)
def gamma(beta):                return 1.0/math.sqrt(1.0-beta*beta)
def ke_rel(mass_kg, beta):      return (gamma(beta)-1.0)*mass_kg*C_LIGHT*C_LIGHT
def rel_corr_pct(beta):
    keN = ke_joules(1.0, beta); keR = ke_rel(1.0, beta)
    return 100.0*(keR-keN)/keN
def time_years(beta, dist_au):
    return dist_au*AU_M / v_mps(beta) / SEC_PER_YEAR
def beam_power_relative(beta, beta_ref): return (beta/beta_ref)**2

out=[]
def log(s=""): out.append(s)

# ============ [C] 量纲 / 单位检查 ============
log("=== [C] 单位/量纲检查 (dimensional_check) ===")
checks = [
    ("V_KMPS",     "L^1·T^-1",   v_kmps(0.05),      "km/s"),
    ("TIME_ALPHA", "T^1",        time_alpha_years(0.05), "yr"),
    ("E_1mt",      "M^1·L^2·T^-2", ke_joules(1e9,0.05), "J"),
    ("E_5mt",      "M^1·L^2·T^-2", ke_joules(5e9,0.05), "J"),
    ("E_sail_1g",  "M^1·L^2·T^-2", ke_joules(1e-3,0.05),"J"),
    ("dust_1mg",   "M^1·L^2·T^-2", dust_ke_joules(0.05),"J"),
    ("artgrav_a",  "L^1·T^-2",   artgrav_a(1000.0,2.0), "m/s^2"),
    ("ke_rel",     "M^1·L^2·T^-2", ke_rel(1e9,0.05),  "J"),
    ("gamma",      "dimless",    gamma(0.05),       "—"),
    ("beam_power_ratio", "dimless", beam_power_relative(0.05,0.2), "—"),
]
for name, dim, val, unit in checks:
    log(f"  {name:<16} nominal_dim={dim:<12} computed_val={val:.6g} unit={unit}")

# 2. 推导量纲代数学检查（手工做维度代数）
log("\n--- 维度代数推导（手工） ---")
log("  v = β·c:      [L]/[T]                    = L^1·T^-1  OK")
log("  t_alpha=D/v:  [L]/([L]/[T]) = [T]        = T^1       OK")
log("  E=½mv²:       [M]·([L]/[T])² = [M][L]²[T]^-2 = M^1·L^2·T^-2 = J  OK")
log("  a=ω²r:        ([T]^-1)²·[L] = [L][T]^-2  = L^1·T^-2  OK")
log("  γ=1/√(1-β²):  无量纲                      = dimless   OK")
log("  P/P_ref=(β/β_ref)²: 无量纲                = dimless   OK")

# ============ [D] 三速度情景复算 ============
log("\n=== [D] 三速度情景派生量复算 (derived_result) ===")
for b in CRUISE_C:
    ta=time_alpha_years(b)
    e1=ke_joules(1e9,b); e5=ke_joules(5e9,b); ed=dust_ke_joules(b)
    e_sail=ke_joules(1e-3,b)
    g=gamma(b); rc=rel_corr_pct(b)
    tgl=time_years(b,SUN_GLENS_AU); thel=time_years(b,HELIOPAUSE_AU); tpr=time_years(b,PRECURSOR_AU)
    br=beam_power_relative(b,STARSHOT_V_C)
    log(f"--- S-{b:.2f}c  v={v_kmps(b):.4f} km/s  gamma={g:.6f} ---")
    log(f"  TIME_ALPHA(4.25ly)   = {ta:.4f} yr")
    log(f"  E_1mt                = {e1:.6e} J  (TNT {tnt_kg(e1)/1000:.4e} t)")
    log(f"  E_5mt                = {e5:.6e} J")
    log(f"  E_sail_1g            = {e_sail:.6e} J = {e_sail/1e9:.4f} GJ  [P4 单帆动能]")
    log(f"  dust_1mg             = {ed:.6e} J = TNT {tnt_kg(ed):.4f} kg")
    log(f"  rel_correction       = {rc:.4f} %  ({'<0.2%' if rc<0.2 else '>=0.2%'})")
    log(f"  TIME_GLENS(550AU)    = {tgl:.5f} yr = {tgl*12:.3f} mo")
    log(f"  TIME_HELIOP(120AU)   = {thel:.5f} yr = {thel*12:.3f} mo")
    log(f"  TIME_PRECURSOR(1000AU)= {tpr:.5f} yr = {tpr*12:.3f} mo")
    log(f"  P_REL(0.2c)          = {br:.5f} (= {1/br:.2f}x reduction)")
    log("")

# ============ [E] 知识锚点五项 vs acceptance.json reference_checks ============
log("=== [E] 0.03c 五项冻结锚点 vs 权威 acceptance.json reference_checks ===")
anchor_req = {
    "travel_years_at_0_03c":           (time_alpha_years(0.03),  141.6666667, 0.001),
    "kinetic_energy_1mt_at_0_03c_j":   (ke_joules(1e9,0.03),     4.0443983e22, 0.01),
    "kinetic_energy_5mt_at_0_03c_j":   (ke_joules(5e9,0.03),     2.0221991e23, 0.01),
    "gravity_1km_2rpm_m_s2":           (artgrav_a(1000.0,2.0),   43.8649,      0.01),
    "dust_1mg_0_03c_j":                (dust_ke_joules(0.03),    40443983,    0.01),
}
allok=True
log(f"  {'name':<30}{'computed':>16}{'expected':>16}{'rel_err':>12}{'tol':>8}  verdict")
for name,(comp,exp,tol) in anchor_req.items():
    rel=abs(comp-exp)/abs(exp); ok=rel<=tol; allok=allok and ok
    log(f"  {name:<30}{comp:>16.6e}{exp:>16.6e}{rel:>12.2e}{tol:>8.3f}  {'OK' if ok else 'FAIL'}")
log(f"  => result: {'5/5 within tolerance' if allok else 'NOT all within tolerance'}")

# ============ [F] 事实纪律 / 未知项提醒 ============
log("\n=== [F] 事实纪律（unknown / 不可外推）===")
log("  E=½mv² 仅为动能下限；真实推进能源预算 = 束能量/帆效率/束源效率 -> unknown")
log("  克级(≈1g)探测器可行性不可外推为大型无人载荷/载人飞船 (mass scaling)")
log("  Starshot 0.2c/100GW 锚点为 B级 assumption；采购/建设依据需 A级核验")
log("  1mt 此处指 1e9kg (百万吨级) 质量参考，勿当 1 百万载荷工程定义")

# ============ [G] 与 v4 冻结成果表逐值对比 ============
log("\n=== [G] 与 v4 冻结 physics_three_speed_scenarios.csv 逐值对比 ===")
v4_rows = {
    "S-0.01c": {"v_kmps":2997.9246,"time_alpha":425.0,"E_1mt":4.493776e21,"E_sail_1g_GJ":4.4938,"dust_tnt":1.0740,"rel":0.0075,"br":0.00250},
    "S-0.03c": {"v_kmps":8993.7737,"time_alpha":141.6667,"E_1mt":4.044398e22,"E_sail_1g_GJ":40.4440,"dust_tnt":9.6663,"rel":0.0676,"br":0.02250},
    "S-0.05c": {"v_kmps":14989.6229,"time_alpha":85.0,"E_1mt":1.123444e23,"E_sail_1g_GJ":112.3444,"dust_tnt":26.8510,"rel":0.1879,"br":0.06250},
}
for sid, v4 in v4_rows.items():
    beta = float(sid.split("-")[1].replace("c",""))
    m = {
        "v_kmps": v_kmps(beta), "time_alpha": time_alpha_years(beta),
        "E_1mt": ke_joules(1e9,beta), "E_sail_1g_GJ": ke_joules(1e-3,beta)/1e9,
        "dust_tnt": tnt_kg(dust_ke_joules(beta)), "rel": rel_corr_pct(beta),
        "br": beam_power_relative(beta,STARSHOT_V_C),
    }
    log(f"  --- {sid} ---")
    for k,v in v4.items():
        mv=m[k]; tol = 0.05 if k=="rel" else 0.001
        reldiff = abs(mv-v)/abs(v) if v else 0
        log(f"    {k:<16} v4={v:<12.6} mine={mv:<12.6} rel_diff={reldiff:.2e} {'MATCH' if reldiff<=tol else 'MISMATCH'}")

print("\n".join(out))
open("physics_v6_recompute_evidence.txt","w",encoding="utf-8").write("\n".join(out))
print("\n[written] physics_v6_recompute_evidence.txt")
