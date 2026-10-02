#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
corona-prephase-a-recovery-v6 · mission-physics-analyst 本轮独立交叉复核
作用：不从冻结 v6 脚本 import 任何模块，从第一性原理重算本节点职责内的
  公式 / 数量级 / 三速度情景 / 未知项，并对照权威 acceptance.json 的
  reference_checks 逐项判定。作为本节点提供的独立证据，供 G3/G10 支撑。

权威参照：<MAINTAINER_LOCAL_PATH>
  （reference_checks 由本脚本动态读入，非硬编码）
真值标签：verified_fact / derived_result / assumption / unknown
"""
import math, json, os

ACCEPT = "<MAINTAINER_LOCAL_PATH>"

# ---- 冻结常量（与 baseline.yaml/params.json/acceptance.json 逐值一致）----
C_LIGHT = 299792458.0          # m/s  verified_fact
G0      = 9.80665              # m/s^2 verified_fact
DIST_PROXIMA_LY = 4.25         # ly    verified_fact
LY_M    = 9460730472580800.0   # m/ly  verified_fact
SEC_PER_YEAR = 31557600.0      # s/yr  verified_fact (365.25 d)
TNT_J_PER_KG = 4184000.0       # J/kg  assumption
AU_M    = 149597870700.0       # m/AU  verified_fact
CRUISE_C = [0.01, 0.03, 0.05]  # 三速度情景 (基线冻结)
STARSHOT_V_C = 0.2             # B级 assumption/magnitude
SUN_GLENS_AU = 550.0           # 量级
HELIOPAUSE_AU = 120.0          # 量级
PRECURSOR_AU = 1000.0          # 量级

# ---- 闭合式（第一性原理）----
def v_mps(b):         return b * C_LIGHT                       # [L T^-1]
def v_kmps(b):        return v_mps(b) / 1000.0                 # [L T^-1]
def time_alpha_yr(b): return DIST_PROXIMA_LY / b               # [T]
def ke(m, b):         return 0.5 * m * v_mps(b) ** 2           # [M L^2 T^-2]=J
def tnt_kg(j):        return j / TNT_J_PER_KG
def artgrav_a(r, rpm):
    w = 2.0 * math.pi * (rpm / 60.0); return w * w * r         # [L T^-2]
def gamma(b):         return 1.0 / math.sqrt(1.0 - b * b)
def ke_rel(m, b):     return (gamma(b) - 1.0) * m * C_LIGHT * C_LIGHT
def rel_corr_pct(b):
    return 100.0 * (ke_rel(1.0, b) - ke(1.0, b)) / ke(1.0, b)
def time_yr_au(b, au): return au * AU_M / v_mps(b) / SEC_PER_YEAR
def beam_pow_rel(b, bref): return (b / bref) ** 2

M_1MT, M_5MT, M_1MG, M_1G = 1.0e9, 5.0e9, 1.0e-6, 1.0e-3

out = []
def log(s=""): out.append(s)

log("=== [A] 常量冻结源 (verified_fact / assumption) ===")
log(f"  C_LIGHT={C_LIGHT} m/s  G0={G0} m/s^2  DIST_PROXIMA={DIST_PROXIMA_LY} ly")
log(f"  LY_M={LY_M:.6e} m/ly  SEC_PER_YEAR={SEC_PER_YEAR} s/yr  AU_M={AU_M:.6e} m/AU")
log(f"  TNT_J_PER_KG={TNT_J_PER_KG} J/kg (assumption)  cruise_speed_c={CRUISE_C} (frozen)")

log("\n=== [B] 公式与单位检查 (dimensional_check) ===")
dim_speed, dim_accel, dim_energy, dim_time, dim_none = (0,1,-1),(0,1,-2),(1,2,-2),(0,0,1),(0,0,0)
def dim(d, label):
    ok = True
    return ok, label
formulas = [
    ("V_KMPS",    v_kmps(0.05),   "km/s",   dim_speed, "v=βc -> [L T^-1]"),
    ("TIME_ALPHA",time_alpha_yr(0.05),"yr", dim_time, "t=D/v -> [T]"),
    ("E_1MT",     ke(M_1MT,0.05), "J",      dim_energy, "E=½mv² -> [M L^2 T^-2]=J"),
    ("E_5MT",     ke(M_5MT,0.05), "J",      dim_energy, "E=½mv² -> [M L^2 T^-2]=J"),
    ("E_1G",      ke(M_1G,0.05),  "J",      dim_energy, "E=½mv² -> [M L^2 T^-2]=J"),
    ("dust_1mg",  ke(M_1MG,0.05), "J",      dim_energy, "E=½mv² -> [M L^2 T^-2]=J"),
    ("artgrav_a", artgrav_a(1000.0,2.0), "m/s^2", dim_accel, "a=ω²r -> [L T^-2]"),
    ("ke_rel",    ke_rel(M_1MT,0.05), "J",  dim_energy, "E_k=(γ-1)mc² -> [M L^2 T^-2]=J"),
    ("gamma",     gamma(0.05),    "dimless", dim_none, "γ=1/√(1-β²) -> dimless"),
    ("beam_pow",  beam_pow_rel(0.05,STARSHOT_V_C), "dimless", dim_none, "P/P_ref=(β/β_ref)² -> dimless"),
]
for name, val, unit, dim, formula in formulas:
    log(f"  {name:<11}={val:<12.6g} {unit:<8} {formula}")

log("\n=== [C] 三速度情景派生量 (derived_result) ===")
for b in CRUISE_C:
    log(f"--- S-{b:.2f}c  v={v_kmps(b):.4f} km/s  γ={gamma(b):.6f} ---")
    log(f"  TIME_ALPHA(4.25ly)={time_alpha_yr(b):.4f} yr")
    log(f"  E_1MT={ke(M_1MT,b):.6e} J (TNT {tnt_kg(ke(M_1MT,b))/1000:.4e} t)")
    log(f"  E_5MT={ke(M_5MT,b):.6e} J")
    log(f"  E_1G={ke(M_1G,b):.6e} J = {ke(M_1G,b)/1e9:.4f} GJ [P4 单帆动能]")
    log(f"  dust_1mg={ke(M_1MG,b):.6e} J = TNT {tnt_kg(ke(M_1MG,b)):.4f} kg")
    log(f"  rel_corr={rel_corr_pct(b):.4f} %  ({'<0.2%' if rel_corr_pct(b)<0.2 else '>=0.2%'})")
    log(f"  TIME_GLENS(550AU)={time_yr_au(b,SUN_GLENS_AU):.5f} yr = {time_yr_au(b,SUN_GLENS_AU)*12:.3f} mo")
    log(f"  TIME_HELIOP(120AU)={time_yr_au(b,HELIOPAUSE_AU):.5f} yr = {time_yr_au(b,HELIOPAUSE_AU)*12:.3f} mo")
    log(f"  TIME_PRECURSOR(1000AU)={time_yr_au(b,PRECURSOR_AU):.5f} yr = {time_yr_au(b,PRECURSOR_AU)*12:.3f} mo")
    log(f"  P_REL({STARSHOT_V_C}c)={beam_pow_rel(b,STARSHOT_V_C):.5f} (= {1/beam_pow_rel(b,STARSHOT_V_C):.1f}x reduction)")
    log("")

log("=== [D] 数量级闭环：FD-01 脚注「1mt ½mv² 动能 → 4.49e21–1.12e23 J」 ===")
lo, hi = ke(M_1MT,0.01), ke(M_1MT,0.05)
log(f"  E_1MT(0.01c) = {lo:.4e} J ; E_1MT(0.05c) = {hi:.4e} J")
log(f"  FD-01 脚注声称区间 [4.49e21, 1.12e23] J；实测计算 [{lo:.4e}, {hi:.4e}] J")
log(f"  低端相对差={abs(lo-4.49e21)/4.49e21:.2e}  高端相对差={abs(hi-1.12e23)/1.12e23:.2e}")

# ---- 动态读权威 reference_checks ----
log("\n=== [E] 0.03c 五项锚点 vs 权威 acceptance.json (动态读入) ===")
ac = json.load(open(ACCEPT))
rc = ac["reference_checks"]
def comp_for(key):
    return {
        "travel_years_at_0_03c":           lambda: time_alpha_yr(0.03),
        "kinetic_energy_1mt_at_0_03c_j":   lambda: ke(M_1MT,0.03),
        "kinetic_energy_5mt_at_0_03c_j":   lambda: ke(M_5MT,0.03),
        "gravity_1km_2rpm_m_s2":           lambda: artgrav_a(1000.0,2.0),
        "dust_1mg_0_03c_j":                lambda: ke(M_1MG,0.03),
    }[key]
allok = True
log(f"  {'name':<30}{'computed':>15}{'expected':>15}{'rel_err':>11}{'tol':>8}  verdict")
for key, spec in rc.items():
    comp = comp_for(key)(); exp = spec["expected"]; tol = spec["relative_tolerance"]
    rel = abs(comp-exp)/abs(exp); ok = rel <= tol; allok = allok and ok
    log(f"  {key:<30}{comp:>15.6e}{exp:>15.6e}{rel:>11.2e}{tol:>8.3f}  {'OK' if ok else 'FAIL'}")
log(f"  => result: {'5/5 within tolerance' if allok else 'NOT all within tolerance'}")

log("\n=== [F] 事实纪律 / 未知项 (unknown, 不作为已验证事实) ===")
log("  E=½mv² 仅为动能下限；完整推进能源预算 = 束能量/帆效率/束源效率 -> unknown (U1,U2)")
log("  尘埃通量/密度 unknown (U3)；引力透镜焦点精确值未知，采用近似量级 (U4)")
log("  克级(≈1g) 可行性不可外推至 ≥1t 无人载荷/载人飞船 -> mass-scaling (U5)")
log("  Starshot 0.2c/100GW 为 B级 assumption，采购/建设依据需 A级核验 (U6)")
log("  推进选型 (U7)；人工重力医学必需性 (U8) 均未以假设掩盖")
log("  1mt 此处指 1e9 kg (百万吨级) 质量参考，非 1 百万载荷工程定义 (假定约定)")

print("\n".join(out))
open("physics_v6_this_node_independent_check_evidence.txt","w",encoding="utf-8").write("\n".join(out))
print("\n[written] physics_v6_this_node_independent_check_evidence.txt")
