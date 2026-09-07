#!/usr/bin/env python3
"""corona_model.py — 日冕计划先期论证统一计算模型（Level-0 / Pre-Phase A 概念级）。

追溯键: baseline_id = corona-baseline-1.0.0 · baseline_version = 1.0.0
        content_digest = cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2

事实纪律:
- 全部参数派生自 params.json（由 make_params.py 从 baseline_frozen.yaml 生成）[verified_fact]
- 公式与 interface_control.md ICD-FML-001..005 一致 [verified_fact]
- 动能仅为自身动能下限，不得表述为完整推进能源预算 [verified_fact]（纪要 §二.2）
- 航行时间为匀速巡航值，不含加速、减速与航向修正 [verified_fact]（纪要 §二.1）
- 本文件不声称施工级、制造级或飞行认证级设计。

命令行用法（有界，自行退出）:
    python3 corona_model.py [--output model_results.json]
"""
import argparse
import json
import math
import os

HERE = os.path.dirname(os.path.abspath(__file__))
PARAMS_FILE = os.path.join(HERE, "params.json")
DEFAULT_OUT = os.path.join(HERE, "model_results.json")

SECONDS_PER_YEAR = 365.25 * 86400.0
LY_IN_M = 9.4607304725808e15  # = c * 365.25*86400，常用定义 [assumption：仅用于展示换算，不进入参考校核]


def load_params(path: str = PARAMS_FILE) -> dict:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


# ---------------------------------------------------------------------------
# 核心公式（ICD-FML-001..005）
# ---------------------------------------------------------------------------

def travel_time_yr(distance_ly: float, cruise_speed_c: float) -> float:
    """ICD-FML-001：匀速航行时间 = 距离 / 速度（不含加减速与航向修正）。"""
    return distance_ly / cruise_speed_c


def light_travel_time_yr(distance_ly: float) -> float:
    """光行时：信号单程传播时间（年），数值上等于光年距离。"""
    return distance_ly


def kinetic_energy_j(mass_kg: float, cruise_speed_c: float, c_m_s: float) -> float:
    """ICD-FML-002：非相对论动能下限 KE = 1/2·m·(βc)²。仅自身动能下限。"""
    v = cruise_speed_c * c_m_s
    return 0.5 * mass_kg * v * v


def kinetic_energy_relativistic_j(mass_kg: float, cruise_speed_c: float, c_m_s: float) -> float:
    """相对论对照 KE = (γ−1)·m·c²，单列，不得与非相对论值混用。"""
    beta = cruise_speed_c
    gamma = 1.0 / math.sqrt(1.0 - beta * beta)
    return (gamma - 1.0) * mass_kg * c_m_s * c_m_s


def artificial_gravity_m_s2(radius_m: float, rotation_rpm: float) -> float:
    """ICD-FML-003：a = ω²·r，ω = 2π·rpm/60。"""
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rpm_for_gravity(radius_m: float, target_m_s2: float) -> float:
    """反解：给定半径与目标加速度求转速（rpm）。"""
    omega = math.sqrt(target_m_s2 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


def dust_impact_energy_j(dust_mass_kg: float, relative_speed_c: float, c_m_s: float) -> float:
    """ICD-FML-004：尘埃撞击动能 = 1/2·m·(βc)²。"""
    v = relative_speed_c * c_m_s
    return 0.5 * dust_mass_kg * v * v


def tnt_equivalent_kg(energy_j: float, j_per_kg: float) -> float:
    """TNT 当量换算（1 kg TNT = 4.184e6 J，常用约定 [assumption]）。"""
    return energy_j / j_per_kg


def max_turn_angle_deg(delta_v_fraction: float) -> float:
    """ICD-FML-005：小角近似转向上限 ≈ (Δv/v) rad → 度。"""
    return math.degrees(delta_v_fraction)


# ---------------------------------------------------------------------------
# 扩展计算（任务书 §4.1 覆盖项）
# ---------------------------------------------------------------------------

def accel_profile(cruise_speed_c: float, c_m_s: float, accel_m_s2: float) -> dict:
    """匀速近似下的加/减速边界：达速时间与加速段距离（经典力学，β<<1 适用边界注明）。"""
    v = cruise_speed_c * c_m_s
    t_s = v / accel_m_s2
    d_m = 0.5 * v * t_s
    return {
        "accel_m_s2": accel_m_s2,
        "time_to_cruise_yr": t_s / SECONDS_PER_YEAR,
        "accel_distance_ly": d_m / LY_IN_M,
        "boundary_note": "经典匀加速近似；β=0.03 时相对论修正 <0.1%，概念级可接受 [derived_result]",
    }


def beam_energy_j(delivered_ke_j: float, efficiency: float) -> float:
    """能量供应链情景：源端能量 = 交付动能下限 / 端到端效率（效率为情景假设）。"""
    return delivered_ke_j / efficiency


def scenario_report(params: dict, mass_kg: float, efficiency: float,
                    radius_m: float, rotation_rpm: float,
                    dust_mass_kg: float = 1e-6) -> dict:
    """单情景完整计算。当前基线仅批准 0.03c；本函数接受任意输入供应用交互，
    但交付产物默认只展示基线锁定值（CCR-001 预留接口不实现）。"""
    c = params["constants"]["speed_of_light_m_s"]
    g0 = params["constants"]["standard_gravity_m_s2"]
    dist = params["constants"]["proxima_distance_ly"]
    beta = params["scenarios"]["cruise_speed_c"][0]

    ke = kinetic_energy_j(mass_kg, beta, c)
    ke_rel = kinetic_energy_relativistic_j(mass_kg, beta, c)
    dust_e = dust_impact_energy_j(dust_mass_kg, beta, c)
    ag = artificial_gravity_m_s2(radius_m, rotation_rpm)

    return {
        "inputs": {
            "cruise_speed_c": beta,
            "mass_kg": mass_kg,
            "efficiency": efficiency,
            "radius_m": radius_m,
            "rotation_rpm": rotation_rpm,
            "dust_mass_kg": dust_mass_kg,
        },
        "outputs": {
            "travel_time_yr": travel_time_yr(dist, beta),
            "light_travel_time_yr": light_travel_time_yr(dist),
            "kinetic_energy_j": ke,
            "kinetic_energy_relativistic_j": ke_rel,
            "relativistic_correction_pct": (ke_rel - ke) / ke * 100.0,
            "beam_energy_at_efficiency_j": beam_energy_j(ke, efficiency),
            "artificial_gravity_m_s2": ag,
            "artificial_gravity_g": ag / g0,
            "rpm_for_1g_at_radius": rpm_for_gravity(radius_m, g0),
            "dust_impact_energy_j": dust_e,
            "dust_tnt_equivalent_kg": tnt_equivalent_kg(dust_e, params["tnt_equivalent_j_per_kg"]["value"]),
        },
        "notes": {
            "kinetic_energy": "仅自身动能下限，未计效率、排气动能、推进剂、减速、备用与建造损耗；"
                              "不得用作完整推进能源预算 [verified_fact]",
            "travel_time": "匀速巡航，不含加速、减速与航向修正 [verified_fact]",
            "beam_energy": "源端能量 = 动能下限 / 端到端效率；效率为用户情景输入 [assumption]",
        },
    }


def precursor_probe_report(params: dict) -> dict:
    """先锋探测器路线（0.2c 为路线内部工况，ICD-RTE-003）。"""
    dist = params["constants"]["proxima_distance_ly"]
    beta = params["precursor_probe"]["cruise_speed_c"]
    flight = travel_time_yr(dist, beta)
    back = light_travel_time_yr(dist)
    return {
        "cruise_speed_c": beta,
        "one_way_flight_yr": flight,
        "data_return_yr": back,
        "earliest_data_arrival_yr": flight + back,
        "rule": "先锋数据不得作为第 20 年以前母舰决策条件 [verified_fact]（纪要 §二.6）",
    }


def turn_capability_report(params: dict) -> dict:
    """目标几何与转向上限（ICD-FML-005，RQ-MDL-007）。"""
    tg = params["target_geometry_deg"]
    low = max_turn_angle_deg(0.10)
    high = max_turn_angle_deg(0.15)
    separations = {
        "proxima_barnard_deg": tg["proxima_barnard"],
        "proxima_tau_ceti_deg": tg["proxima_tau_ceti"],
        "barnard_tau_ceti_deg": tg["barnard_tau_ceti"],
    }
    min_sep = min(separations.values())
    return {
        "turn_limit_deg_at_10pct_dv": low,
        "turn_limit_deg_at_15pct_dv": high,
        "target_separations": separations,
        "midcourse_target_switch_feasible": bool(high >= min_sep),
        "conclusion": "10%%–15%% Δv 仅支持约 %.1f°–%.1f° 转向，三目标最小角距 %.0f°；"
                      "中途切换目标不可行，目标选择须在主加速前完成 [derived_result]"
                      % (low, high, min_sep),
    }


def route_sensitivity_report(params: dict) -> dict:
    """三路线敏感性比较（在 0.03c 锁定情景内：质量缩放、局部敏感指数）。
    跨速度情景比较为 CCR-001 预留接口，当前不实现 [verified_fact]。"""
    c = params["constants"]["speed_of_light_m_s"]
    dist = params["constants"]["proxima_distance_ly"]
    beta = params["scenarios"]["cruise_speed_c"][0]
    cases = {case["id"]: case for case in params["reference_cases"]}

    crew_ke = kinetic_energy_j(cases["crewed_1mt"]["mass_kg"], beta, c)
    mat_ke = kinetic_energy_j(cases["orbital_material_5mt"]["mass_kg"], beta, c)

    return {
        "locked_scenario_c": beta,
        "travel_time_yr": travel_time_yr(dist, beta),
        "routes": {
            "laser_sail_precursor": {
                "characteristic_speed_c": params["precursor_probe"]["cruise_speed_c"],
                "one_way_flight_yr": travel_time_yr(dist, params["precursor_probe"]["cruise_speed_c"]),
                "note": "克级帆探测器；其可行性不得外推至另两条路线 [verified_fact]（RQ-SCP-003）",
            },
            "uncrewed_civilization_archive": {
                "reference_mass_kg": cases["orbital_material_5mt"]["mass_kg"],
                "kinetic_energy_lower_bound_j": mat_ke,
                "note": "档案载荷质量规模自身待定 [unknown]；此处引用 5Mt 入轨物资参考工况仅作数量级锚点",
            },
            "crewed_interstellar_vehicle": {
                "reference_mass_kg": cases["crewed_1mt"]["mass_kg"],
                "kinetic_energy_lower_bound_j": crew_ke,
                "note": "载人路线依赖推进/生态/辐射防护/在轨工业等未成熟能力 [verified_fact]（纪要 §一）",
            },
        },
        "sensitivity": {
            "ke_vs_mass": "动能与质量线性成正比：∂KE/KE = ∂m/m [derived_result]",
            "ke_vs_speed_local": "动能随速度二次增长：β=0.03 处 ∂KE/KE = 2·∂β/β [derived_result]",
            "cross_speed_comparison": "0.01c/0.05c 情景比较 = CCR-001 预留接口，当前未实现 [verified_fact]",
        },
        "separability_rule": params.get("route_separability", {}).get("rule",
            "三路线可分离；禁止跨路线可行性外推 [verified_fact]"),
    }


def full_run(params: dict) -> dict:
    """完整基线计算，返回可序列化结果（供 CLI 写出与测试比对）。"""
    c = params["constants"]["speed_of_light_m_s"]
    g0 = params["constants"]["standard_gravity_m_s2"]
    dist = params["constants"]["proxima_distance_ly"]
    beta = params["scenarios"]["cruise_speed_c"][0]
    cases = {case["id"]: case for case in params["reference_cases"]}

    crew = cases["crewed_1mt"]
    mat = cases["orbital_material_5mt"]
    dust = cases["dust_1mg"]
    hab = cases["rotating_habitat"]

    reference_checks = {
        "travel_time_yr": travel_time_yr(dist, beta),
        "kinetic_energy_1mt_j": kinetic_energy_j(crew["mass_kg"], crew["cruise_speed_c"], c),
        "kinetic_energy_5mt_j": kinetic_energy_j(mat["mass_kg"], mat["cruise_speed_c"], c),
        "artificial_gravity_m_s2": artificial_gravity_m_s2(hab["radius_m"], hab["rotation_rpm"]),
        "dust_impact_energy_j": dust_impact_energy_j(dust["mass_kg"], dust["relative_speed_c"], c),
    }

    accel_scenarios = [
        accel_profile(beta, c, a) for a in (0.01 * g0, 0.1 * g0, 1.0 * g0)
    ]
    efficiency_scenarios = [
        {
            "efficiency": eta,
            "beam_energy_1mt_j": beam_energy_j(reference_checks["kinetic_energy_1mt_j"], eta),
            "fact_label": "assumption（效率为情景假设，非工程承诺）",
        }
        for eta in (0.1, 0.25, 0.5)
    ]

    return {
        "traceability": params["traceability"],
        "scope": "Level-0 / Pre-Phase A 概念级；非施工/制造/飞行认证设计 [verified_fact]",
        "locked_scenario": {
            "cruise_speed_c": beta,
            "locked": params["scenarios"]["locked"],
            "pending_change": "CCR-001（0.01c/0.05c 扩展）待平台确认，未纳入 [verified_fact]",
        },
        "reference_checks": reference_checks,
        "relativistic_comparison": {
            "kinetic_energy_1mt_relativistic_j": kinetic_energy_relativistic_j(crew["mass_kg"], beta, c),
            "correction_pct": (kinetic_energy_relativistic_j(crew["mass_kg"], beta, c)
                               - reference_checks["kinetic_energy_1mt_j"])
                              / reference_checks["kinetic_energy_1mt_j"] * 100.0,
            "rule": "相对论对照单列，不得与非相对论下限混用 [verified_fact]（ICD-FML-002）",
        },
        "artificial_gravity": {
            "reference_case": {"radius_m": hab["radius_m"], "rotation_rpm": hab["rotation_rpm"]},
            "gravity_m_s2": reference_checks["artificial_gravity_m_s2"],
            "gravity_g": reference_checks["artificial_gravity_m_s2"] / g0,
            "rpm_for_1g_same_radius": rpm_for_gravity(hab["radius_m"], g0),
        },
        "dust_impact": {
            "mass_kg": dust["mass_kg"],
            "relative_speed_c": dust["relative_speed_c"],
            "energy_j": reference_checks["dust_impact_energy_j"],
            "tnt_equivalent_kg": tnt_equivalent_kg(
                reference_checks["dust_impact_energy_j"], params["tnt_equivalent_j_per_kg"]["value"]),
            "retracted_claim": "旧稿『1毫克约450兆焦』已撤回，不得复现 [verified_fact]（纪要 §二.4）",
            "unknowns": ["尘埃通量", "粒径分布", "盾体面密度", "间距", "消耗率", "补充方式", "总质量"],
        },
        "accel_decel_scenarios": accel_scenarios,
        "efficiency_scenarios": efficiency_scenarios,
        "precursor_probe": precursor_probe_report(params),
        "turn_capability": turn_capability_report(params),
        "route_sensitivity": route_sensitivity_report(params),
        "baseline_scenario_example": scenario_report(
            params, mass_kg=crew["mass_kg"], efficiency=0.25,
            radius_m=hab["radius_m"], rotation_rpm=hab["rotation_rpm"]),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="日冕统一计算模型 CLI（有界，自行退出）")
    parser.add_argument("--output", default=DEFAULT_OUT, help="结果 JSON 输出路径")
    args = parser.parse_args()

    params = load_params()
    results = full_run(params)
    with open(args.output, "w", encoding="utf-8") as fh:
        json.dump(results, fh, ensure_ascii=False, indent=2)
        fh.write("\n")

    rc = results["reference_checks"]
    print("traceability     = %s / %s / %s" % (
        results["traceability"]["baseline_id"],
        results["traceability"]["baseline_version"],
        results["traceability"]["content_digest"]))
    print("travel_time_yr           = %.7f" % rc["travel_time_yr"])
    print("kinetic_energy_1mt_j     = %.7e" % rc["kinetic_energy_1mt_j"])
    print("kinetic_energy_5mt_j     = %.7e" % rc["kinetic_energy_5mt_j"])
    print("artificial_gravity_m_s2  = %.4f" % rc["artificial_gravity_m_s2"])
    print("dust_impact_energy_j     = %.4f" % rc["dust_impact_energy_j"])
    print("written                  = %s" % args.output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
