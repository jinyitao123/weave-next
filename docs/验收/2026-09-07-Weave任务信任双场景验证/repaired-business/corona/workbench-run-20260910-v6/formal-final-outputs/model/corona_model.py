#!/usr/bin/env python3
"""
corona_model.py - 日冕计划 Pre-Phase A 统一计算内核（单一事实来源）

冻结基线: corona-prephase-a-v1 (v1.0.0, frozen_for_validation)
速度轴:   0.01c / 0.03c / 0.05c

本模块与 app/model.js 同源: 使用完全相同的公式与参数来源。
参数唯一事实源: model/params.json ( == baseline/baseline.yaml 的机器可读副本 )

真值标签:
    verified_fact  - 定义值/测量值 (光速、标准重力、比邻星距离等)
    derived_result - 由本模型公式推导得出
    assumption     - 基线约定或简化假设 (TNT 当量、光帆功率随 v^2 缩放等)
    unknown        - 缺少直接证据，显式保留为未知

派生变量命名规则: <量>_<对象>(CS)
    V_KMPS(CS)        巡航速度 km/s
    TIME_ALPHA(CS)    到比邻星(4.25 ly) 航时 yr
    TIME_GLENS(CS)    到太阳引力透镜区(550 AU) 航时 yr
    TIME_PRECURSOR(CS) 到 1000 AU 星际前驱 航时 yr
    E_SAIL_1G(CS)     单帆(1 g) 动能下限 J
    P_REL(CS)         相对 0.2c 光帆束动力比
"""

import json
import os
import math

_HERE = os.path.dirname(os.path.abspath(__file__))
_PARAMS_PATH = os.path.join(_HERE, "params.json")


def _load_params():
    with open(_PARAMS_PATH, "r", encoding="utf-8") as fh:
        return json.load(fh)


PARAMS = _load_params()

# --- 冻结常量 (verified_fact / baseline 约定) -------------------------------
C_LIGHT = float(PARAMS["constants"]["speed_of_light_m_s"])            # m/s  (verified_fact)
G0 = float(PARAMS["constants"]["standard_gravity_m_s2"])              # m/s^2 (verified_fact)
DIST_PROXIMA_LY = float(PARAMS["constants"]["proxima_distance_ly"])   # ly   (verified_fact)
LY_M = float(PARAMS["constants"]["ly_m"])                             # m    (verified_fact)
SEC_PER_YEAR = float(PARAMS["constants"]["sec_per_year"])             # s    (verified_fact)
TNT_J_PER_KG = float(PARAMS["constants"]["tnt_equivalent_J_per_kg"])  # J/kg (assumption)
AU_M = float(PARAMS["constants"]["au_m"])                             # m    (verified_fact)
GLENS_DIST_AU = float(PARAMS["constants"]["solar_lens_distance_au"])  # AU   (derived/anchor)
PRECURSOR_DIST_AU = float(PARAMS["constants"]["precursor_distance_au"])  # AU (baseline)

STARSHOT_V = float(PARAMS["starshot_anchor"]["target_velocity_c"])     # 0.2c (anchor, B 级)
STARSHOT_P = float(PARAMS["starshot_anchor"]["beam_power_W"])          # 100 GW (anchor, B 级)

CRUISE_SPEEDS_C = [float(x) for x in PARAMS["scenarios"]["cruise_speed_c"]]
SCENARIO_IDS = list(PARAMS["scenarios"]["scenario_ids"])
DEFAULT_SCENARIO_C = float(PARAMS["scenarios"]["default_scenario_c"])

FORBIDDEN_CLAIMS = list(PARAMS["forbidden_claims"])
TRUTH_LABELS = list(PARAMS["truth_labels"])

REFERENCE_CASES = list(PARAMS["reference_cases"])
ACCEPTANCE_REFERENCE_CHECKS = dict(PARAMS["acceptance_reference_checks"])

SCENARIO_TO_ID = {round(c, 6): sid for c, sid in zip(CRUISE_SPEEDS_C, SCENARIO_IDS)}


def scenario_id_for_c(c):
    """返回给定巡航速度分数对应的规范情景 ID。"""
    return SCENARIO_TO_ID[round(float(c), 6)]


def _kr():
    """内圈半径 1000 m、2 rpm 的旋转居住舱角速度 (rad/s)。"""
    return 2.0 * math.pi * 2.0 / 60.0


def gravity_rotating_habitat(radius_m=1000.0, rotation_rpm=2.0):
    """旋转人工重力场 a = omega^2 * r (m/s^2)。(derived_result)"""
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rpm_for_g0(radius_m=1000.0):
    """产生 1 g0 人工重力所需转速 (rpm)。(derived_result)"""
    omega = math.sqrt(G0 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


def v_kmps(c):
    """V_KMPS(CS): 巡航速度 km/s。(derived_result)"""
    return float(c) * C_LIGHT / 1000.0


def speed_m_s(c):
    """巡航速度 m/s。(derived_result)"""
    return float(c) * C_LIGHT


def time_alpha_yrs(c):
    """TIME_ALPHA(CS): 到比邻星(4.25 ly) 匀速航时 yr。(derived_result)"""
    return DIST_PROXIMA_LY / float(c)


def time_glens_yrs(c):
    """TIME_GLENS(CS): 到太阳引力透镜区(550 AU) 匀速航时 yr。(derived_result)"""
    v_au_yr = float(c) * LY_M / AU_M
    return GLENS_DIST_AU / v_au_yr


def time_precursor_yrs(c):
    """TIME_PRECURSOR(CS): 到 1000 AU 星际前驱 匀速航时 yr。(derived_result)"""
    v_au_yr = float(c) * LY_M / AU_M
    return PRECURSOR_DIST_AU / v_au_yr


def ke_kg(kg, c):
    """经典动能下限 0.5 * m * v^2 (J)。低于 0.2c 相对论修正 <0.2%，故用经典式。(derived_result)"""
    v = speed_m_s(c)
    return 0.5 * kg * v * v


def ke_1mt_j(c):
    """E_SAIL_1G(CS): 1 mt (1e9 kg) 帆/载荷动能下限 J。(derived_result)"""
    return ke_kg(1.0e9, c)


def ke_5mt_j(c):
    """5 mt (5e9 kg) 动能下限 J。(derived_result)"""
    return ke_kg(5.0e9, c)


def ke_dust_1mg_j(c):
    """1 mg 高速尘埃撞击动能 J。(derived_result)"""
    return ke_kg(1.0e-6, c)


def tnt_tonnes(joules):
    """能量等效 TNT 吨数 (assumption: 1 kg TNT = 4.184 MJ)。(derived_result)"""
    return joules / (TNT_J_PER_KG * 1000.0)


def relativistic_ke_1mt_j(c):
    """相对论动能 (gamma-1)*m*c^2 (J)，用于量化非相对论式的修正幅度。(derived_result)"""
    beta = float(c)
    gamma = 1.0 / math.sqrt(1.0 - beta * beta)
    return (gamma - 1.0) * 1.0e9 * C_LIGHT * C_LIGHT


def p_rel(c):
    """P_REL(CS): 与 0.2c 锚点相比的束动力比 (0.2/c)^2。(derived_result, 缩放为 assumption)"""
    return (STARSHOT_V / float(c)) ** 2


def beam_power_gw(c):
    """由 100 GW @ 0.2c 与 P_REL 推出的束动力 (GW)。(derived_result)"""
    return STARSHOT_P / p_rel(c) / 1.0e9


def compute_scenario(c):
    """对给定巡航速度分数 c 计算全部标准派生量。"""
    c = float(c)
    return {
        "scenario_id": scenario_id_for_c(c),
        "cruise_speed_c": c,
        "v_kmps": v_kmps(c),
        "time_alpha_yrs": time_alpha_yrs(c),
        "time_glens_yrs": time_glens_yrs(c),
        "time_glens_days": time_glens_yrs(c) * 365.25,
        "time_precursor_yrs": time_precursor_yrs(c),
        "time_precursor_months": time_precursor_yrs(c) * 12.0,
        "ke_1mt_j": ke_1mt_j(c),
        "ke_5mt_j": ke_5mt_j(c),
        "ke_1mt_tnt_tonnes": tnt_tonnes(ke_1mt_j(c)),
        "ke_dust_1mg_j": ke_dust_1mg_j(c),
        "ke_dust_1mg_tnt_kg": tnt_tonnes(ke_dust_1mg_j(c)) * 1000.0,
        "rel_ke_1mt_j": relativistic_ke_1mt_j(c),
        "rel_correction_pct": (relativistic_ke_1mt_j(c) - ke_1mt_j(c)) / ke_1mt_j(c) * 100.0,
        "p_rel": p_rel(c),
        "beam_power_gw": beam_power_gw(c),
    }


def compute_all():
    """计算三情景下的全部派生量，作为统一结果表。"""
    return {sid: compute_scenario(c) for c, sid in zip(CRUISE_SPEEDS_C, SCENARIO_IDS)}


def reference_check():
    """复算 acceptance.json 的五项 0.03c 冻结参考锚点，返回逐项结果。"""
    checks = []
    travel = time_alpha_yrs(0.03)
    ke_1mt = ke_1mt_j(0.03)
    ke_5mt = ke_5mt_j(0.03)
    grav = gravity_rotating_habitat(1000.0, 2.0)
    dust = ke_dust_1mg_j(0.03)
    computed = {
        "travel_years_at_0_03c": travel,
        "kinetic_energy_1mt_at_0_03c_j": ke_1mt,
        "kinetic_energy_5mt_at_0_03c_j": ke_5mt,
        "gravity_1km_2rpm_m_s2": grav,
        "dust_1mg_0_03c_j": dust,
    }
    all_ok = True
    rows = []
    for name, spec in ACCEPTANCE_REFERENCE_CHECKS.items():
        val = computed[name]
        expected = float(spec["expected"])
        tol = float(spec["relative_tolerance"])
        rel_err = abs(val - expected) / abs(expected) if expected else float("inf")
        ok = rel_err <= tol
        all_ok = all_ok and ok
        rows.append({
            "name": name,
            "computed": val,
            "expected": expected,
            "rel_err": rel_err,
            "tol": tol,
            "ok": ok,
        })
    return {"all_ok": all_ok, "rows": rows}


if __name__ == "__main__":
    import pprint
    print("=== corona_model: 统一计算内核 ===")
    print("baseline_id =", PARAMS["baseline_id"], "version", PARAMS["baseline_version"])
    print("cruise_speed_c =", CRUISE_SPEEDS_C, " scenario_ids =", SCENARIO_IDS)
    print()
    allres = compute_all()
    for sid, res in allres.items():
        print(f"--- {sid} ---")
        pprint.pprint(res)
    print()
    print("=== 参考锚点复算 (0.03c) ===")
    rc = reference_check()
    for r in rc["rows"]:
        print(f"  {r['name']:36s} computed={r['computed']:.9e} expected={r['expected']:.9e} rel_err={r['rel_err']:.3e} tol={r['tol']} -> {'OK' if r['ok'] else 'FAIL'}")
    print("  ALL_OK =", rc["all_ok"])
