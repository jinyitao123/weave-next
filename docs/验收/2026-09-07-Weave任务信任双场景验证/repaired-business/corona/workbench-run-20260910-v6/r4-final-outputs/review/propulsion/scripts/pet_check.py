#!/usr/bin/env python3
"""
日冕计划 Pre-Phase A · v6 复审核 · propulsion-energy-analyst 节点
有界自动检查：推进 / 能源 / 热控 比较 + 成立/终止条件 + 验证清单

复用输入（只读，冻结基线）：
  v4-outputs/model/baseline.yaml  —— 冻结共同参数基线（唯一参数源，随冻结清单
                                     以 manifest 哈希校验、只读）
  本脚本由 v4 outputs/scripts/pet_check.py 复用而来，仅接受权调整：
    将 v4 运行时路径 inputs/lead/baseline/baseline.yaml 改为冻结输入中的
    v4-outputs/model/baseline.yaml（同源参数，见 baseline.yaml 头部 "self-contained copy"）。
  除基线路径外，计算逻辑与 v4 完全一致，用于独立复核 v6 是否可复现 v4 结果。

原则：
  - 速度轴 0.01c / 0.03c / 0.05c；0.03c 仅为 acceptance 参考锚点。
  - 本脚本是「有界」执行：单线程、有限次数、无循环、无长服务、退出即止。
  - 涉及真实采购/许可的功率数值仅作量级（P_REL 语义），并明确标注。
  - 所有派生物理量标为 derived_result（模型推导）；锚点标 B 级；设计假设标 assumption。

输出：
  stdout 表格 + 结果 JSON (outputs/review/propulsion/checks/pet_results.json)
"""
import json, math, sys, os, re

# ---------- 从冻结基线文件读取常量（只读输入，不复制到别的主机） ----------
BASE = "<MAINTAINER_LOCAL_PATH>"
if not os.path.exists(BASE):
    # 若冻结输入不可见，尝试回退到 v4 运行时相对布局（不寄望于其存在）
    _alt = os.path.join(os.path.dirname(__file__), "..", "..", "..", "..", "..", "..", "inputs", "lead", "baseline", "baseline.yaml")
    BASE = os.path.abspath(_alt)

def _parse_flat_yaml(path):
    """极简 flat yaml 解析：只取 key: value 行（用于本冻结基线），不引入外部 yaml 依赖。"""
    d = {}
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.rstrip("\n")
            if not line.strip() or line.strip().startswith("#"):
                continue
            m = re.match(r"^\s*([A-Za-z0-9_]+)\s*:\s*(.+?)\s*$", line)
            if not m:
                continue
            key, val = m.group(1), m.group(2).strip()
            # 去掉行尾注释
            val = val.split(" #")[0].strip().strip('"').strip("'")
            # 顶层键优先：嵌套 reference_cases 的 cruise_speed_c: 0.03 不得覆盖顶层数组，故取首次出现
            if key not in d:
                d[key] = val
    return d

y = _parse_flat_yaml(BASE)

def _num(k):
    v = y[k]
    try:
        return float(v)
    except ValueError:
        return v

C_LIGHT = _num("speed_of_light_m_s")            # 299792458.0  verified_fact
G0      = _num("standard_gravity_m_s2")         # 9.80665      verified_fact
DIST_LY = _num("proxima_distance_ly")           # 4.25         verified_fact
LY_M    = _num("ly_m")                          # 9.4607304725808e15 verified_fact
SEC_YR  = _num("sec_per_year")                  # 31557600     verified_fact
TNT_JKG = _num("tnt_equivalent_J_per_kg")       # 4184000      assumption(基线约定)
base_id = y.get("baseline_id", "?")
base_ver = y.get("baseline_version", "?")
# 提取 cruise_speed_c 列表（容忍 yaml 行内数组的括号/引号书写）
_speed_raw = y.get("cruise_speed_c", "0.01,0.03,0.05")
speeds = [float(x) for x in re.findall(r"[\d.]+", _speed_raw)]
speeds = [s for s in speeds if 0 <= s <= 1]
DIST_PROXIMA_M = DIST_LY * LY_M

# 参考锚点（acceptance.json 口径，来自 baseline.yaml acceptance_reference_checks）
ANCHORS = {
    "travel_years_at_0_03c": ("travel_years_at_0_03c", 141.6666667, 0.001),
    "kinetic_energy_1mt_at_0_03c_j": ("kinetic_energy_1mt_at_0_03c_j", 4.0443983e22, 0.01),
    "kinetic_energy_5mt_at_0_03c_j": ("kinetic_energy_5mt_at_0_03c_j", 2.0221991e23, 0.01),
}
# 后三项在 y 中不再单独解析，直接用期望值构建
ANCHORS["gravity_1km_2rpm_m_s2"] = (None, 43.8649, 0.01)
ANCHORS["dust_1mg_0_03c_j"] = (None, 40443983.0, 0.01)

# ---------- 物理常数 ----------
STE_B = 5.670374419e-8        # Stefan-Boltzmann  W/m^2/K^4   verified_fact
AU_M  = 1.495978707e11        # 天文单位 m                     verified_fact
LENS_AU   = 550.0             # 太阳引力透镜最小焦距（量级）   assumption (B 级锚点口径)
PREC_AU   = 1000.0            # 星际前驱参考距离              assumption (模型口径)
STARSHOT_V = 0.2              # 锚点：Starshot 目标速度 c      B 级
STARSHOT_P = 100e9            # 锚点：Starshot 概念束功率 W    B 级
EFF_REFL   = 0.95             # 帆净反射率（高反射介质镜）     assumption
ALPHA_ABS  = 1 - EFF_REFL     # 吸收份额 α                    assumption
EPS_EM     = 0.90             # 辐射发射率 ε                  assumption

def beta_c(b):  return b * C_LIGHT
def v_kmps(b):  return b * C_LIGHT / 1000.0
def gamma(b):   return 1.0 / math.sqrt(1.0 - b * b)
def ke_classic(m, v): return 0.5 * m * v * v
def ke_rel(m, b):
    g = gamma(b)
    return (g - 1.0) * m * C_LIGHT * C_LIGHT

# ---------- 逐情景计算 ----------
scenarios = []
for b in speeds:
    v = beta_c(b)
    sis = {
        "scenario": f"S-{b:.2f}c",
        "beta": b,
        "v_kmps": v_kmps(b),
        "gamma": gamma(b),
        # 航时（弧长/速度，动参考系时间尺度近似=静止系，因 γ≈1）
        "t_proxima_yr": (DIST_PROXIMA_M / v) / SEC_YR,
        "t_lens_days": (LENS_AU * AU_M / v) / 86400.0,
        "t_precursor_months": (PREC_AU * AU_M / v) / (SEC_YR / 12.0),
        "ke_1mt_j": ke_classic(1e9, v),                 # 1e9 kg = 1 兆吨级 (载人级)
        "ke_5mt_j": ke_classic(5e9, v),
        "ke_1mt_tnt_mt": ke_classic(1e9, v) / 4.184e15,  # 1 Mt TNT = 4.184e15 J
        "ke_5mt_tnt_mt": ke_classic(5e9, v) / 4.184e15,
        "dust_1mg_j": ke_classic(1e-6, v),              # 尘埃 1mg @ 本情景相对速度
        "dust_1mg_tnt_kg": ke_classic(1e-6, v) / TNT_JKG,  # 1mg 尘埃撞击能换算 TNT(kg)
        # 相对论修正 (rel - classic)/rel
        "rel_correction": abs(ke_rel(1.0, b) - ke_classic(1.0, v)) / ke_rel(1.0, b),
        # 单帆(1g)动能  E_SAIL_1G(CS)   —— 对应基线 P4
        "keg_1g_j": ke_classic(1e-3, v),
        # 束功率相对 Starshot 锚点：P ∝ v^2（同帆质量、同加速剖面） → P_REL(CS)
        "p_rel_gw": STARSHOT_P * (b / STARSHOT_V) ** 2 / 1e9,
        "p_reduction_x": (b / STARSHOT_V) ** -2,
    }
    scenarios.append(sis)

# ---------- 参考锚点复算（0.03c 逐项，容差） ----------
def ans():
    a03 = next(s for s in scenarios if abs(s["beta"] - 0.03) < 1e-9)
    rows = [
        ("travel_years_at_0_03c", a03["t_proxima_yr"], ANCHORS["travel_years_at_0_03c"][1], ANCHORS["travel_years_at_0_03c"][2]),
        ("kinetic_energy_1mt_at_0_03c_j", a03["ke_1mt_j"], ANCHORS["kinetic_energy_1mt_at_0_03c_j"][1], ANCHORS["kinetic_energy_1mt_at_0_03c_j"][2]),
        ("kinetic_energy_5mt_at_0_03c_j", a03["ke_5mt_j"], ANCHORS["kinetic_energy_5mt_at_0_03c_j"][1], ANCHORS["kinetic_energy_5mt_at_0_03c_j"][2]),
        ("gravity_1km_2rpm_m_s2", (2*math.pi/60*2)**2 * 1000, ANCHORS["gravity_1km_2rpm_m_s2"][1], ANCHORS["gravity_1km_2rpm_m_s2"][2]),
        ("dust_1mg_0_03c_j", a03["dust_1mg_j"], ANCHORS["dust_1mg_0_03c_j"][1], ANCHORS["dust_1mg_0_03c_j"][2]),
    ]
    return rows

# ---------- 热控（帆束流平衡温度，量级） ----------
# 参考：吸收入射功率占比 α，帆双面辐射平衡  T = (α·P_beam / (2·ε·σ·A))^1/4
def sail_temp(p_beam_w, area_m2):
    p_abs = ALPHA_ABS * p_beam_w
    return (p_abs / (2.0 * EPS_EM * STE_B * area_m2)) ** 0.25

# ---------- 输出 ----------
lines = []
w = lines.append
w(f"# 推进/能源/热控 · 有界自动检查输出")
w(f"基线: {base_id} v{base_ver}  (唯一参数源: {os.path.basename(BASE)})")
w(f"情景速度轴: {speeds} c   默认参考: 0.03 c")
w("")
w("## 1) 派生量（模型推导，derived_result）")
w(f"{'情景':<12}{'v(km/s)':>12}{'γ':>10}{'到比邻星(yr)':>14}{'到透镜区(d)':>12}{'到前驱(月)':>12}{'1mt动能(J)':>18}{'5mt动能(J)':>18}{'相对论修正':>14}")
for s in scenarios:
    w(f"{s['scenario']:<12}{s['v_kmps']:>12.1f}{s['gamma']:>10.5f}{s['t_proxima_yr']:>14.3f}{s['t_lens_days']:>12.1f}{s['t_precursor_months']:>12.1f}"
      f"{s['ke_1mt_j']:>18.4e}{s['ke_5mt_j']:>18.4e}{s['rel_correction']:>14.2e}")
w("")
w("## 2) 动能 / 尘埃撞击 / 束功率相对量级（P_REL，量级语义；推导模型）")
for s in scenarios:
    w(f"{s['scenario']:<12} 1mt动能={s['ke_1mt_j']:>10.3e}J(~{s['ke_1mt_tnt_mt']:>6.1f}Mt TNT)  5mt={s['ke_5mt_j']:>10.3e}J(~{s['ke_5mt_tnt_mt']:>6.1f}Mt TNT)"
      f"  尘埃1mg(本情景)={s['dust_1mg_j']:>10.3e}J(~{s['dust_1mg_tnt_kg']:>6.2f}kg TNT)")
for s in scenarios:
    w(f"{s['scenario']:<12} P_REL={s['p_rel_gw']:>8.3f} GW  较0.2c/100GW下降 {s['p_reduction_x']:>6.1f} 倍  单帆(1g)动能 E_SAIL_1G={s['keg_1g_j']:>10.3e} J")
w("")
w("## 3) 参考锚点复算（0.03c，acceptance.json 容差）")
rows = ans()
allok = True
for name, got, exp, tol in rows:
    err = abs(got - exp) / exp
    ok = err <= tol
    allok = allok and ok
    w(f"  {name:<34} got={got:>14.6e} exp={exp:>14.6e} rel_err={err:>10.3e} tol={tol:>6.3f}  {'PASS' if ok else 'FAIL'}")
w(f"  总体: {'5/5 在容差内' if allok else '存在超差'}")
w("")
w("## 4) 热控分析：帆束流平衡温度（assumption 驱动，量级）")
w(f"  模型: T = (α·P_beam / (2·ε·σ·A))^(1/4)，α(吸收)={ALPHA_ABS:g}, ε(发射)={EPS_EM:g}")
w("  定值基准 A=1 m²（克级/单片帆，物性临界警示）:")
for s in scenarios:
    T = sail_temp(s["p_rel_gw"] * 1e9, 1.0)
    w(f"    {s['scenario']:<12} P_beam={s['p_rel_gw']:>8.3f}GW  → T≈{T:>8.0f} K")
w("  设计空间扫掠 (平衡温度 K, 不同 α×A):")
w("    " + "".join(f"{'A'+a:>10}" for a in ["0.5", "1", "5", "10", "100"]))
for s in scenarios:
    row = f"    {s['scenario']:<10}"
    for a in ["0.5", "1", "5", "10", "100"]:
        row += f"{sail_temp(s['p_rel_gw']*1e9, float(a)):>10.0f}"
    w(row)

res = {
    "baseline_id": base_id, "baseline_version": base_ver,
    "baseline_source": os.path.basename(BASE),
    "speeds": speeds,
    "scenarios": scenarios,
    "anchors": [{"name": n, "got": g, "exp": e, "tol": t, "pass": abs(g-e)/e <= t} for n, g, e, t in rows],
    "anchors_all_pass": allok,
}
print("\n".join(lines))

outdir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "checks"))
os.makedirs(outdir, exist_ok=True)
with open(os.path.join(outdir, "pet_results.json"), "w", encoding="utf-8") as f:
    json.dump(res, f, indent=2, ensure_ascii=False)
print(f"\n[written] {os.path.join(outdir, 'pet_results.json')}")
