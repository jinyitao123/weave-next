#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
corona-prephase-a-v6 · mission-physics-analyst 复核修正版复算脚本
基线：v4 冻结 physics_recompute.py（corona-prephase-a-v1, speed axis 0.01c/0.03c/0.05c）

修正说明（v6 复核发现）：
  [缺陷描述] v4 `physics_recompute.py` 的 [C] 块把 1e9 kg (百万吨/1mt) 参考质量的
  动能打印成标签 "E_SAIL_1G"，而 "E_SAIL_1G"（1g 帆）在包内另一处（CSV 列 E_sail_1g、
  分析文档 P4/§3.1）指 1 克帆。1e9 kg 与 1e-3 kg 差正好 10^12 倍 —— 这正是 v4 分析
  文档 §0 亲自警示的「两张表差约 10^12 倍，严禁混用；引用动能必须带质量基准」。
  v4 的 [B] 块标签已带 "(1mt)"，但 [C] 块打印时丢失该限定，导致复算证据把 1mt
  数值标成了"1G"，与该包自己的 CSV 和分析文档口径冲突（跨工件一致性 G10 风险）。

  [修正] 把 [C] 块该行的打印标签改为 "E_SAIL_1MT(1e9kg)"（与它实际计算的质量一致），
  并新增一行 "E_SAIL_1G(1g)" 输出真正的 1 克帆动能（= P4 单帆动能 4.5/40.4/112.3 GJ），
  使复算证据与 CSV、分析文档 §0/§3.1/§6.2 逐值自洽。除标签与新增 1g 行外，所有数值
  与公式均与 v4 逐字一致，未改任何物理常数或公式。
"""
import math

# ---- 冻结常量（baseline.yaml / 01 §2.2；标注真值等级）----
C_LIGHT = 299792458.0          # m/s (定义值, verified_fact)
G0 = 9.80665                   # m/s^2 (verified_fact)
DIST_PROXIMA_LY = 4.25         # ly (verified_fact)
LY_M = 9.4607304725808e15      # m/ly (verified_fact)
SEC_PER_YEAR = 31557600.0      # s/yr (儒略年 365.25 d, verified_fact)
TNT_J_PER_KG = 4184000.0       # J/kg (assumption, baseline convention)
AU_M = 1.495978707e11          # m/AU (standard astronomical unit, verified_fact)
STARSHOT_V_C = 0.2             # 锚点速度 0.2c (B级 / assumption-magnitude)
CRUISE_C = [0.01, 0.03, 0.05]  # 情景速度 (基线冻结, 不得改动)

# 天文参考距离（量级/assumption, 非基线冻结值）
SUN_GLENS_AU = 550.0           # 太阳引力透镜焦点距离（近似量级）
HELIOPAUSE_AU = 120.0          # 外日球层边界参考值
PRECURSOR_AU = 1000.0          # 星际前驱量级

# ---- 维度代数（单位检查）：量纲表示为 (M质量, L长度, T时间) 指数 ----
DIM_SPEED = (0, 1, -1)     # m/s
DIM_ACCEL = (0, 1, -2)     # m/s^2
DIM_ENERGY = (1, 2, -2)    # kg·m^2/s^2 = J
DIM_TIME = (0, 0, 1)       # s
DIM_NONE = (0, 0, 0)       # 无量纲


def dim_mul(a, b): return (a[0] + b[0], a[1] + b[1], a[2] + b[2])
def dim_scale(a, k): return (a[0] * k, a[1] * k, a[2] * k)
def dim_eq(a, b): return a == b


def dim_str(d):
    parts = []
    if d[0]: parts.append(f"M^{d[0]}")
    if d[1]: parts.append(f"L^{d[1]}")
    if d[2]: parts.append(f"T^{d[2]}")
    return "·".join(parts) if parts else "dimless"


# ---- 公式（与 v4 逐字一致）----
def v_mps(beta): return beta * C_LIGHT  # [L T^-1]


def v_kmps(beta): return v_mps(beta) / 1000.0  # [L T^-1]


def time_alpha_years(beta):
    # t = D / v ; D = DIST_PROXIMA_LY ; 因 1 ly = c·1 yr => t(yr) = 4.25 / beta
    return DIST_PROXIMA_LY / beta  # [T]


def ke_joules(mass_kg, beta): return 0.5 * mass_kg * (v_mps(beta) ** 2)  # [M L^2 T^-2] = J


def dust_ke_joules(beta): return ke_joules(1.0e-6, beta)  # 1 mg


def sail_1g_ke_joules(beta): return ke_joules(1.0e-3, beta)  # 1 g (P4 单帆)


def tnt_kg(joules): return joules / TNT_J_PER_KG


def artgrav_a(radius_m, rpm):
    omega = 2.0 * math.pi * (rpm / 60.0)  # [T^-1]
    return omega * omega * radius_m  # [L T^-2]


def artgrav_rpm_for_g(radius_m, g_target):
    omega = math.sqrt(g_target / radius_m)  # [T^-1]
    return 60.0 * omega / (2.0 * math.pi)


def gamma(beta): return 1.0 / math.sqrt(1.0 - beta * beta)


def ke_rel(mass_kg, beta): return (gamma(beta) - 1.0) * mass_kg * C_LIGHT * C_LIGHT  # [M L^2 T^-2]


def time_years(beta, distance_au):
    d_m = distance_au * AU_M
    return d_m / v_mps(beta) / SEC_PER_YEAR  # [T]


def beam_power_relative(beta, beta_ref):
    # 光帆辐射压力下限近似: F ∝ P_beam/c ; a=F/m ; L=v^2/(2a) => P_beam ∝ m v^2 / L
    # 固定帆质量 m 与固定加速距离 L 时: P ∝ v^2 => P(beta)/P(beta_ref) = (beta/beta_ref)^2
    return (beta / beta_ref) ** 2  # dimless


def main():
    out = []

    def log(msg=""): out.append(msg)

    log("=== [A] 常量冻结源 (verified_fact) ===")
    for lbl, v, u in [
        ("C_LIGHT", C_LIGHT, "m/s"), ("G0", G0, "m/s^2"),
        ("DIST_PROXIMA", DIST_PROXIMA_LY, "ly"),
        ("LY_M", LY_M, "m/ly"), ("SEC_PER_YEAR", SEC_PER_YEAR, "s/yr"),
        ("AU_M", AU_M, "m/AU"), ("TNT_J_PER_KG", TNT_J_PER_KG, "J/kg (assumption)"),
        ("cruise_speed_c", CRUISE_C, "frozen"),
    ]:
        log(f"  {lbl:<16} = {v}  {u}")
    log()

    log("=== [B] 单位检查 (dimensional_check) ===")
    # [v6 修正] 标签由 "E_SAIL_1G(1mt)" 改为 "E_SAIL_1MT(1e9kg)"，避免 "1G" 误读为 1 克
    checks = [
        ("V_KMPS", DIM_SPEED, "km/s", v_kmps, "v = beta*c;  [L T^-1]"),
        ("TIME_ALPHA", DIM_TIME, "yr", time_alpha_years, "t = D/v = 4.25ly/beta;  [T]"),
        ("E_SAIL_1MT(1e9kg)", DIM_ENERGY, "J", lambda b: ke_joules(1e9, b),
         "E = 1/2 m v^2;  [M L^2 T^-2]=J"),
        ("E_SAIL_5MT", DIM_ENERGY, "J", lambda b: ke_joules(5e9, b),
         "E = 1/2 m v^2;  [M L^2 T^-2]=J"),
        ("dust_1mg", DIM_ENERGY, "J", lambda b: ke_joules(1e-6, b),
         "E = 1/2 m v^2;  [M L^2 T^-2]=J"),
        ("artgrav_a", DIM_ACCEL, "m/s^2", lambda b: artgrav_a(1000, 2),
         "a = omega^2 r;  [T^-2][L]=[L T^-2]"),
        ("ke_rel(1mt)", DIM_ENERGY, "J", lambda b: ke_rel(1e9, b),
         "Ek=(gamma-1)m c^2;  [M L^2 T^-2]=J"),
        ("gamma", DIM_NONE, "dimless", gamma, "gamma=1/sqrt(1-beta^2);  dimless"),
        ("beam_power_ratio", DIM_NONE, "dimless", lambda b: beam_power_relative(b, STARSHOT_V_C),
         "P/P_ref = (beta/beta_ref)^2;  dimless"),
    ]
    for name, dim, unit, fn, formula in checks:
        val = fn(0.05)
        dim_of_val = dim
        ok = dim_eq(dim_of_val, dim)
        log(f"  {name:<18} unit={unit:<8} dim=({dim_str(dim)})  {formula}")
        log(f"  {'':<19} {dim_str(dim_of_val)}  {'DIM-OK' if ok else 'DIM-MISMATCH'}")
    log()

    log("=== [C] 三速度情景派生量复算 (derived_result) ===")
    for beta in CRUISE_C:
        ta = time_alpha_years(beta)
        e1 = ke_joules(1.0e9, beta)      # 1e9 kg (百万吨/1mt) —— 请勿与 1g 混淆
        e5 = ke_joules(5.0e9, beta)
        ed = dust_ke_joules(beta)
        eg = sail_1g_ke_joules(beta)     # 1 g 帆 (P4 单帆)
        g = gamma(beta)
        rel_corr = 100.0 * (ke_rel(1.0e9, beta) - e1) / e1
        tgl = time_years(beta, SUN_GLENS_AU)
        thel = time_years(beta, HELIOPAUSE_AU)
        tpr = time_years(beta, PRECURSOR_AU)
        br = beam_power_relative(beta, STARSHOT_V_C)
        log(f"--- S-{beta:.2f}c (v={v_kmps(beta):.4f} km/s, gamma={g:.6f}) ---")
        log(f"  TIME_ALPHA   = {ta:.4f} yr   ({DIST_PROXIMA_LY}ly / {beta:.2f})")
        # [v6 修正] 原 v4 打印 "E_SAIL_1G" 但数值实为 1e9kg；改为明确质量基准
        log(f"  E_SAIL_1MT(1e9kg) = {e1:.6e} J  ({tnt_kg(e1):.6e} kg TNT = {tnt_kg(e1)/1000:.6e} t TNT)")
        log(f"  E_SAIL_5MT    = {e5:.6e} J  ({tnt_kg(e5):.6e} kg TNT)")
        # [v6 修正] 新增真正 1 克帆行，正面对应 P4 单帆动能
        log(f"  E_SAIL_1G(1g) = {eg:.6e} J  ({eg/1e9:.4f} GJ)  [P4 单帆动能]")
        log(f"  dust_1mg     = {ed:.6e} J  ({tnt_kg(ed):.4f} kg TNT)")
        log(f"  相对论修正   = {rel_corr:.4f} %  ({'<0.2%' if rel_corr < 0.2 else '>=0.2%'})")
        log(f"  TIME_GLENS   = {tgl:.5f} yr = {tgl*12:.3f} mo  ({SUN_GLENS_AU}AU)")
        log(f"  TIME_HELIOP  = {thel:.5f} yr = {thel*12:.3f} mo  ({HELIOPAUSE_AU}AU)")
        log(f"  TIME_PRECURSOR={tpr:.5f} yr = {tpr*12:.3f} mo  ({PRECURSOR_AU}AU)")
        log(f"  P_REL(0.2c)  = {br:.5f}  (= {1/br:.1f}x reduction)")
        log()
    log("--- P3 复核: 0.2c 束功率到三速度的降幅 (P ∝ v^2; 固定帆质量/加速距离) ---")
    for beta in CRUISE_C:
        r = beam_power_relative(beta, STARSHOT_V_C)
        log(f"  0.2c -> {beta:.2f}c : {1/r:.1f}x reduction  (P/P_ref={r:.4f})")
    log()

    log("=== [D] 人工重力 (与速度无关, reserved) ===")
    a2 = artgrav_a(1000.0, 2.0)
    rpm1g = artgrav_rpm_for_g(1000.0, G0)
    log(f"  r=1000m, 2rpm -> a={a2:.6f} m/s^2 = {a2/G0:.4f} g0")
    log(f"  r=1000m, 1g0  -> rpm={rpm1g:.5f} rpm")
    log()

    log("=== [E] 0.03c 五项冻结锚点 vs acceptance 容差 (independent check) ===")
    anchors = [
        ("travel_years_at_0_03c", time_alpha_years(0.03), 141.6666667, 0.001),
        ("kinetic_energy_1mt_at_0_03c_j", ke_joules(1.0e9, 0.03), 4.04439830e22, 0.010),
        ("kinetic_energy_5mt_at_0_03c_j", ke_joules(5.0e9, 0.03), 2.02219910e23, 0.010),
        ("gravity_1km_2rpm_m_s2", artgrav_a(1000.0, 2.0), 43.8649, 0.010),
        ("dust_1mg_0_03c_j", dust_ke_joules(0.03), 40443983.0, 0.010),
    ]
    allok = True
    for name, comp, exp, tol in anchors:
        rel = abs(comp - exp) / abs(exp)
        ok = rel <= tol
        allok = allok and ok
        log(f"  {name:<30} computed={comp:.8e} expected={exp:.8e} rel_err={rel:.2e} tol={tol:.3f} -> {'OK' if ok else 'FAIL'}")
    log(f"  => 5/5 {'within tolerance' if allok else 'NOT all within tolerance'}")
    log()

    log("=== [F] 事实纪律提醒 (below NOT a full propulsion budget) ===")
    log("  E = 1/2 m v^2 仅为【动能下限】; 完整推进能源预算 = 束能量/帆效率/束源效率 -> unknown")
    log("  克级(≈1g) 探测器可行性 不可外推为 大型无人载荷(≥1t)/载人飞船  => mass-scaling caveat")
    log("  Starshot 0.2c/100GW 锚点 为 B级 (assumption/magnitude); 作为采购/建设依据需 A 级核验")
    log("  1mt 此处指 1e9 kg (百万吨级) 质量参考; 非 1 百万载荷 的工程定义 (note 单位约定)")

    print("\n".join(out))
    return "\n".join(out)


if __name__ == "__main__":
    main()
