# -*- coding: utf-8 -*-
"""
日冕计划 · 统一计算模型内核 (Level-0 / Pre-Phase A 概念级)
追溯键: baseline_id=corona-baseline-1.0.0 · baseline_version=1.0.0
        content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
参数唯一来源: outputs/model/baseline_frozen.yaml (经 derive_params.rb 派生为 parameters.json)
公式接口: 见 interface_control.md ICD-FML-001..005
事实标签纪律: 每个结果项携带 fact_label ∈ {verified_fact, derived_result, assumption, unknown}
本模型为概念级计算, 不含 construction_ready / manufacturing_ready /
flight_certified / whole_program_cost_committed 任何声明。
"""
import json
import math
import os

HERE = os.path.dirname(os.path.abspath(__file__))

TRACE_KEYS = {
    "baseline_id": "corona-baseline-1.0.0",
    "baseline_version": "1.0.0",
    "content_digest": "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2",
}

# 常用换算约定 [assumption]: 1 kg TNT = 4.184e6 J
TNT_EQUIVALENT_J_PER_KG = 4.184e6


def load_parameters(path=None):
    """读取由 baseline_frozen.yaml 派生的机器可读参数 (ICD-SOT-002)。"""
    p = path or os.path.join(HERE, "parameters.json")
    with open(p, "r", encoding="utf-8") as f:
        return json.load(f)


def _labeled(value, unit, formula, inputs, label, note=None):
    item = {
        "value": value,
        "unit": unit,
        "formula": formula,
        "inputs": inputs,
        "fact_label": label,
    }
    if note:
        item["note"] = note
    return item


# ---------------------------------------------------------------------------
# ICD-FML-001 航行时间与光行时 (匀速巡航, 不含加速/减速/航向修正 — 边界显式标注)
# ---------------------------------------------------------------------------
def travel_time_yr(distance_ly, cruise_speed_c):
    return distance_ly / cruise_speed_c


# ICD-FML-002 动能下限 (非相对论) 与相对论对照 (单列, 不得混用)
def kinetic_energy_nonrel_j(mass_kg, speed_c, c_m_s):
    return 0.5 * mass_kg * (speed_c * c_m_s) ** 2


def kinetic_energy_rel_j(mass_kg, speed_c, c_m_s):
    beta = speed_c
    gamma = 1.0 / math.sqrt(1.0 - beta * beta)
    return (gamma - 1.0) * mass_kg * c_m_s ** 2


# 加速/减速/效率情景: 最小输入能量下限 = KE/η (非相对论下限, 非能源预算)
def min_input_energy_j(kinetic_j, efficiency):
    return kinetic_j / efficiency


# ICD-FML-003 人工重力
def artificial_gravity_m_s2(radius_m, rotation_rpm):
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rpm_for_target_gravity(radius_m, target_m_s2):
    omega = math.sqrt(target_m_s2 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


# ICD-FML-004 尘埃撞击动能
def dust_impact_energy_j(dust_mass_kg, relative_speed_c, c_m_s):
    return 0.5 * dust_mass_kg * (relative_speed_c * c_m_s) ** 2


# ICD-FML-005 速度增量转向上限 (小角近似, 弧度转度)
def turn_angle_limit_deg(delta_v_fraction):
    return math.degrees(delta_v_fraction)


def compute_all(params):
    """按任务书 §4.1 八类计算输出全部结果, 全部携带单位/公式/输入/事实标签。"""
    c = params["constants"]["speed_of_light_m_s"]
    g0 = params["constants"]["standard_gravity_m_s2"]
    dist_ly = params["constants"]["proxima_distance_ly"]
    v_c = params["scenarios"]["cruise_speed_c"][0]  # 锁定单值 0.03
    cases = {rc["id"]: rc for rc in params["reference_cases"]}

    out = {"traceability": dict(TRACE_KEYS), "results": {}}

    # 1. 航行时间与光行时
    t_travel = travel_time_yr(dist_ly, v_c)
    out["results"]["travel_time"] = {
        "travel_time_yr": _labeled(
            t_travel, "yr",
            "travel_time_yr = proxima_distance_ly / cruise_speed_c",
            {"proxima_distance_ly": dist_ly, "cruise_speed_c": v_c},
            "derived_result",
            "匀速巡航下限; 不含加速、减速与航向修正; 区分'飞离太阳系'与'抵达目标恒星系统'"),
        "light_travel_time_yr": _labeled(
            dist_ly, "yr", "light_travel_time_yr = proxima_distance_ly",
            {"proxima_distance_ly": dist_ly}, "derived_result",
            "光行时, 即单向通信/数据回传最低时延"),
    }

    # 2. 非相对论与相对论动能对照 (动能仅为下限, 非完整推进能源预算)
    ke = {}
    for cid in ("crewed_1mt", "orbital_material_5mt"):
        m = cases[cid]["mass_kg"]
        ke[cid] = {
            "kinetic_energy_nonrel_j": _labeled(
                kinetic_energy_nonrel_j(m, v_c, c), "J",
                "kinetic_energy_j = 0.5 * mass_kg * (cruise_speed_c * c)^2",
                {"mass_kg": m, "cruise_speed_c": v_c, "speed_of_light_m_s": c},
                "derived_result",
                "自身动能下限; 未计发动机效率/排气动能/推进剂/减速/备用/建造损耗, "
                "不得用作工程能源预算"),
            "kinetic_energy_rel_j": _labeled(
                kinetic_energy_rel_j(m, v_c, c), "J",
                "kinetic_energy_rel_j = (gamma-1) * mass_kg * c^2",
                {"mass_kg": m, "cruise_speed_c": v_c, "speed_of_light_m_s": c},
                "derived_result", "相对论对照, 与非相对论值分列, 不得混用"),
        }
    out["results"]["kinetic_energy"] = ke

    # 3. 加速/减速/效率/质量情景 (概念级下限估算)
    accel = {}
    for cid in ("crewed_1mt", "orbital_material_5mt"):
        m = cases[cid]["mass_kg"]
        k = kinetic_energy_nonrel_j(m, v_c, c)
        per_eta = {}
        for eta in (0.1, 0.3, 0.5):
            per_eta[str(eta)] = _labeled(
                min_input_energy_j(k, eta), "J",
                "min_input_energy_j = kinetic_energy_nonrel_j / efficiency",
                {"mass_kg": m, "cruise_speed_c": v_c, "efficiency": eta},
                "derived_result",
                "η 为情景假设值 [assumption]; 结果为单边加速的最小输入能量下限, "
                "非完整能源预算; 减速需求同量级, 合计约 2×")
        accel[cid] = {
            "min_input_energy_by_efficiency_j": per_eta,
            "accel_plus_decel_note": (
                "若任务终态为进入目标恒星系统并驻留, 需与加速同量级的减速能力 "
                "[verified_fact 来源: 纪要 §二.1]; 本项仅概念级下限"),
        }
    out["results"]["accel_decel_efficiency"] = accel

    # 4. 人工重力半径与转速
    rh = cases["rotating_habitat"]
    a_ref = artificial_gravity_m_s2(rh["radius_m"], rh["rotation_rpm"])
    out["results"]["artificial_gravity"] = {
        "reference_case_m_s2": _labeled(
            a_ref, "m/s^2",
            "artificial_gravity_m_s2 = (2*pi*rotation_rpm/60)^2 * radius_m",
            {"radius_m": rh["radius_m"], "rotation_rpm": rh["rotation_rpm"]},
            "derived_result"),
        "reference_case_in_g0": _labeled(
            a_ref / g0, "g0 (9.80665 m/s^2)",
            "g_ratio = artificial_gravity_m_s2 / standard_gravity_m_s2",
            {"artificial_gravity_m_s2": a_ref, "standard_gravity_m_s2": g0},
            "derived_result"),
        "rpm_for_1g_at_1km": _labeled(
            rpm_for_target_gravity(rh["radius_m"], g0), "rpm",
            "rpm = 60/(2*pi) * sqrt(standard_gravity_m_s2 / radius_m)",
            {"radius_m": rh["radius_m"], "target_m_s2": g0},
            "derived_result",
            "同一 1km 半径取得 1g 所需转速 ≈ 0.95 rpm"),
        "design_note": ("舱体半径、转速、允许重力与人体适应性为联合设计变量 "
                        "[verified_fact 来源: 纪要 §二.3]"),
    }

    # 5. 高速尘埃撞击能量
    dm = cases["dust_1mg"]
    e_dust = dust_impact_energy_j(dm["mass_kg"], dm["relative_speed_c"], c)
    out["results"]["dust_impact"] = {
        "dust_1mg_energy_j": _labeled(
            e_dust, "J",
            "dust_impact_energy_j = 0.5 * dust_mass_kg * (relative_speed_c * c)^2",
            {"dust_mass_kg": dm["mass_kg"], "relative_speed_c": dm["relative_speed_c"],
             "speed_of_light_m_s": c},
            "derived_result"),
        "dust_1mg_tnt_kg": _labeled(
            e_dust / TNT_EQUIVALENT_J_PER_KG, "kg TNT",
            "tnt_kg = dust_impact_energy_j / 4.184e6",
            {"dust_impact_energy_j": e_dust},
            "derived_result",
            "换算约定 1 kg TNT = 4.184e6 J [assumption]"),
        "dust_10mg_energy_j": _labeled(
            dust_impact_energy_j(1e-5, dm["relative_speed_c"], c), "J",
            "dust_impact_energy_j = 0.5 * dust_mass_kg * (relative_speed_c * c)^2",
            {"dust_mass_kg": 1e-5, "relative_speed_c": dm["relative_speed_c"]},
            "derived_result", "10mg 对照工况 ≈ 405 MJ"),
        "withdrawn_note": ("旧稿'1mg≈450MJ'已撤回, 不得复现 [verified_fact 来源: 纪要 §二.4]; "
                           "尘埃通量/粒径分布/盾体面密度等为 [unknown]"),
    }

    # 6. 目标方向夹角与速度增量转向上限
    out["results"]["target_geometry"] = {
        "separation_angles_deg": {
            "proxima_barnard": _labeled(78.0, "deg", "公开天球坐标计算 (来源: 纪要 §二.5)",
                                        {}, "verified_fact"),
            "proxima_tau_ceti": _labeled(101.0, "deg", "公开天球坐标计算 (来源: 纪要 §二.5)",
                                         {}, "verified_fact"),
            "barnard_tau_ceti": _labeled(117.0, "deg", "公开天球坐标计算 (来源: 纪要 §二.5)",
                                         {}, "verified_fact"),
        },
        "turn_limit_deg": {
            "delta_v_10pct": _labeled(turn_angle_limit_deg(0.10), "deg",
                                      "turn_angle_deg ≈ degrees(Δv/v) 小角近似",
                                      {"delta_v_fraction": 0.10}, "derived_result"),
            "delta_v_15pct": _labeled(turn_angle_limit_deg(0.15), "deg",
                                      "turn_angle_deg ≈ degrees(Δv/v) 小角近似",
                                      {"delta_v_fraction": 0.15}, "derived_result"),
        },
        "conclusion": ("10%–15% Δv 仅支持约 5.7°–8.6° 转向, 无法支持三目标中途切换; "
                       "目标选择须在主加速前完成 [verified_fact 来源: 纪要 §二.5]"),
    }

    # 7. 先锋探测器到达与数据回传 (0.2c 为路线内部工况, ICD-RTE-003)
    v_probe = 0.2
    t_probe = travel_time_yr(dist_ly, v_probe)
    out["results"]["precursor_probe"] = {
        "flight_time_yr_at_0_2c": _labeled(
            t_probe, "yr", "flight_time_yr = proxima_distance_ly / 0.2c",
            {"proxima_distance_ly": dist_ly, "probe_speed_c": v_probe},
            "derived_result", "0.2c 为先锋探测器路线自身参数, 非巡航情景扩展"),
        "data_return_time_yr": _labeled(
            dist_ly, "yr", "data_return_time_yr = proxima_distance_ly (光速回传)",
            {"proxima_distance_ly": dist_ly}, "derived_result"),
        "earliest_data_at_earth_yr": _labeled(
            t_probe + dist_ly, "yr",
            "earliest_return_yr = flight_time_yr + data_return_time_yr",
            {"flight_time_yr": t_probe, "data_return_time_yr": dist_ly},
            "derived_result",
            "最低约 25.5 年; 先锋数据不得作为第 20 年以前母舰决策条件 [verified_fact 来源: 纪要 §二.6]"),
    }

    # 8. 三路线敏感性比较 (巡航情景锁定 0.03c; 质量/效率为敏感性变量)
    sens = {}
    route_cases = {
        "laser_sail_precursor": {
            "name": "激光光帆先锋探测器",
            "reference_mass_kg": None,
            "mass_note": ("克级光帆探测器自身质量与光帆/激光阵参数为 [unknown]; "
                          "其 0.2c 工况见 precursor_probe; 不得外推至其他路线"),
        },
        "uncrewed_civilization_archive": {
            "name": "无人文明档案载荷",
            "reference_mass_kg": None,
            "mass_note": "档案载荷质量规模未在基线中定义 [unknown]; 运输方案另行论证",
        },
        "crewed_interstellar_vehicle": {
            "name": "载人星际飞行器",
            "reference_mass_kg": cases["crewed_1mt"]["mass_kg"],
            "mass_note": "参考工况 crewed_1mt = 1e9 kg [verified_fact 来源: 基线 reference_cases]",
        },
    }
    for rid, rcfg in route_cases.items():
        entry = {"route_id": rid, "route_name": rcfg["name"],
                 "mass_note": rcfg["mass_note"]}
        m0 = rcfg["reference_mass_kg"]
        if m0 is not None:
            sweep = {}
            for factor in (0.9, 1.0, 1.1):
                m = m0 * factor
                k = kinetic_energy_nonrel_j(m, v_c, c)
                sweep[f"mass_x{factor}"] = {
                    "mass_kg": m,
                    "kinetic_energy_nonrel_j": k,
                    "min_input_energy_eta0.3_j": min_input_energy_j(k, 0.3),
                }
            entry["sensitivity_at_0_03c"] = _labeled(
                sweep, "见子项单位",
                "kinetic_energy_nonrel_j(mass_kg, 0.03c); min_input = KE/0.3",
                {"cruise_speed_c": v_c, "efficiency": 0.3},
                "derived_result",
                "巡航速度锁定 0.03c; 质量 ±10% 敏感性; η=0.3 为 [assumption]")
        else:
            entry["sensitivity_at_0_03c"] = _labeled(
                None, "-", "-", {}, "unknown",
                "该路线质量基线未定义, 敏感性待运输/载荷方案论证后回填")
        sens[rid] = entry
    out["results"]["route_sensitivity"] = sens

    # 汇总参考值 (供 acceptance 容差比对)
    out["reference_values"] = {
        "travel_time_yr": t_travel,
        "kinetic_energy_1mt_j": ke["crewed_1mt"]["kinetic_energy_nonrel_j"]["value"],
        "kinetic_energy_5mt_j": ke["orbital_material_5mt"]["kinetic_energy_nonrel_j"]["value"],
        "artificial_gravity_m_s2": a_ref,
        "dust_impact_energy_j": e_dust,
    }
    return out
