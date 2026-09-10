#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
日冕计划 · 统一计算模型内核 (corona_model.py)
追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
        content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2

参数唯一来源: 本目录 baseline_frozen.yaml 的机器可读副本 params.json (ICD-SOT-001/002)。
公式接口: ICD 第 3 节 (ICD-FML-001..005)。
范围: Level-0 / Pre-Phase A 概念级；单一 0.03c 巡航情景（CCR-001 待平台确认）。
事实标签: verified_fact / derived_result / assumption / unknown。
仅使用 Python 标准库；全部函数为纯函数，有界退出。
"""
import json
import math
import os

HERE = os.path.dirname(os.path.abspath(__file__))
PARAMS_PATH = os.path.join(HERE, "params.json")

TNT_JOULES_PER_KG = 4.184e6  # 常用换算约定 [assumption] (ICD-FML-004)

# 目标方向夹角（度），源自纪要 §二.5 所载公开天球坐标计算结果 [verified_fact]
TARGET_SEPARATION_DEG = {
    "proxima-barnard": 78.0,
    "proxima-tau_ceti": 101.0,
    "barnard-tau_ceti": 117.0,
}


def load_params(path=PARAMS_PATH):
    """读取基线参数（机器可读副本，与 baseline_frozen.yaml 同 digest 链）。"""
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def traceability(params):
    return {
        "baseline_id": params["baseline_id"],
        "baseline_version": params["baseline_version"],
        "content_digest": (
            "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2"
        ),
    }


# ---------------------------------------------------------------------------
# 1. 航行时间与光行时 (ICD-FML-001)
# ---------------------------------------------------------------------------
def travel_time_yr(distance_ly, cruise_speed_c):
    """匀速巡航航行时间（年）。边界：不含加速、减速与航向修正 [derived_result]。"""
    return distance_ly / cruise_speed_c


def light_travel_time_yr(distance_ly):
    """光行时（年），数值上等于距离光年数 [derived_result]。"""
    return distance_ly


# ---------------------------------------------------------------------------
# 2. 非相对论与相对论动能对照 (ICD-FML-002)
# ---------------------------------------------------------------------------
def kinetic_energy_classical_j(mass_kg, cruise_speed_c, c):
    """非相对论动能下限 KE=0.5*m*v^2。
    边界：仅是飞行器自身动能下限，未计发动机效率、排气动能、推进剂、
    减速、备用和建造损耗，不得表述为完整推进能源预算 [derived_result]。"""
    v = cruise_speed_c * c
    return 0.5 * mass_kg * v * v


def kinetic_energy_relativistic_j(mass_kg, cruise_speed_c, c):
    """相对论动能对照 (gamma-1)*m*c^2，与非相对论值分列，不得混用 [derived_result]。"""
    beta = cruise_speed_c
    gamma = 1.0 / math.sqrt(1.0 - beta * beta)
    return (gamma - 1.0) * mass_kg * c * c


# ---------------------------------------------------------------------------
# 3. 加速、减速、效率与质量情景
# ---------------------------------------------------------------------------
def mission_energy_scenarios(mass_kg, cruise_speed_c, c, propulsion_efficiency):
    """能量情景（概念级下限估计）。
    返回:
      ke_flyby_j        飞越（飞离太阳系）工况自身动能下限
      ke_arrive_stay_j  抵达并驻留工况 = 2x 动能（加速与减速同量级）[assumption: 对称加速/减速]
      input_energy_flyby_j   计入推进效率的输入能量下限 = KE/eta
      input_energy_arrive_j  抵达驻留输入能量下限 = 2*KE/eta
    推进效率 eta 为可调假设量 [assumption]；结果均为下限，不是能源预算。
    """
    if not (0.0 < propulsion_efficiency <= 1.0):
        raise ValueError("propulsion_efficiency must be in (0, 1]")
    ke = kinetic_energy_classical_j(mass_kg, cruise_speed_c, c)
    return {
        "ke_flyby_j": ke,
        "ke_arrive_stay_j": 2.0 * ke,
        "input_energy_flyby_j": ke / propulsion_efficiency,
        "input_energy_arrive_j": 2.0 * ke / propulsion_efficiency,
        "propulsion_efficiency": propulsion_efficiency,
        "boundary": (
            "仅为动能下限及其效率折算；未计排气动能、推进剂、备用与建造损耗；"
            "不得表述为完整推进能源预算"
        ),
        "fact_label": "derived_result",
    }


# ---------------------------------------------------------------------------
# 4. 人工重力 (ICD-FML-003)
# ---------------------------------------------------------------------------
def artificial_gravity_m_s2(radius_m, rotation_rpm):
    """向心加速度 a = omega^2 * r, omega = 2*pi*rpm/60 [derived_result]。"""
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rpm_for_gravity(radius_m, target_m_s2):
    """在给定半径取得目标加速度所需转速（rpm） [derived_result]。"""
    omega = math.sqrt(target_m_s2 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


# ---------------------------------------------------------------------------
# 5. 高速尘埃撞击 (ICD-FML-004)
# ---------------------------------------------------------------------------
def dust_impact_energy_j(dust_mass_kg, relative_speed_c, c):
    """尘埃相对撞击动能（非相对论，0.03c 下相对论修正 <0.1%） [derived_result]。"""
    v = relative_speed_c * c
    return 0.5 * dust_mass_kg * v * v


def tnt_equivalent_kg(energy_j):
    """TNT 当量（kg），换算约定 1 kg TNT = 4.184e6 J [assumption]。"""
    return energy_j / TNT_JOULES_PER_KG


# ---------------------------------------------------------------------------
# 6. 目标方向夹角与速度增量转向上限 (ICD-FML-005)
# ---------------------------------------------------------------------------
def turn_angle_deg(dv_over_v):
    """小角近似转向上限 turn_angle_deg ~ degrees(dv/v) [derived_result]。"""
    return math.degrees(dv_over_v)


def retargeting_assessment():
    """三目标中途切换评估：现有 10%-15% 速度增量无法支持三目标间切换 [derived_result]。
    夹角值源自纪要 §二.5 [verified_fact]。"""
    limits = {
        "dv_10pct_turn_deg": turn_angle_deg(0.10),
        "dv_15pct_turn_deg": turn_angle_deg(0.15),
    }
    separations = dict(TARGET_SEPARATION_DEG)
    max_turn = limits["dv_15pct_turn_deg"]
    switching_feasible = {
        pair: (angle <= max_turn) for pair, angle in separations.items()
    }
    return {
        "target_separation_deg": separations,
        "turn_limits": limits,
        "mid_course_switching_feasible": switching_feasible,
        "conclusion": (
            "现有速度增量无法支持三个目标之间的中途切换；目标选择应在主加速前完成，"
            "航程中的速度增量主要用于小角度修正和避险"
        ),
        "fact_label_separations": "verified_fact",
        "fact_label_limits": "derived_result",
    }


# ---------------------------------------------------------------------------
# 7. 先锋探测器到达与数据回传（路线内部工况 0.2c，ICD-RTE-003）
# ---------------------------------------------------------------------------
def precursor_timeline(distance_ly, precursor_speed_c=0.2):
    """先锋探测器：飞行时间 + 数据回传光行时 [derived_result]。
    0.2c 为探测器路线自身参数（纪要 §二.6），非巡航情景扩展。"""
    flight_yr = distance_ly / precursor_speed_c
    return_yr = light_travel_time_yr(distance_ly)
    return {
        "flight_time_yr": flight_yr,
        "data_return_yr": return_yr,
        "earliest_data_at_earth_yr": flight_yr + return_yr,
        "boundary": (
            "最低估计：未计系统研制与部署时间；先锋数据不得作为第 20 年以前的母舰决策条件"
        ),
        "fact_label": "derived_result",
    }


# ---------------------------------------------------------------------------
# 8. 三路线敏感性比较（速度锁定 0.03c；敏感变量为质量/效率/半径/转速）
# ---------------------------------------------------------------------------
def route_sensitivity(params):
    """三路线概念级敏感性比较。速度按基线锁定 0.03c 不变（CCR-001 未确认），
    各路线仅在其职责相关变量上做敏感性展示 [derived_result]。"""
    c = params["constants"]["speed_of_light_m_s2"] if False else params["constants"]["speed_of_light_m_s"]
    g0 = params["constants"]["standard_gravity_m_s2"]
    v_c = params["scenarios"]["cruise_speed_c"][0]
    d_ly = params["constants"]["proxima_distance_ly"]
    cases = {c_["id"]: c_ for c_ in params["reference_cases"]}

    # 光帆先锋：对探测器飞行速度（路线内部参数）与距离的回传时间敏感性
    prec = precursor_timeline(d_ly)
    laser = {
        "route_id": "laser_sail_precursor",
        "locked_cruise_speed_c": v_c,
        "precursor_speed_c": 0.2,
        "earliest_data_at_earth_yr": prec["earliest_data_at_earth_yr"],
        "sensitivity": {
            "flight_time_yr_at_0_15c": travel_time_yr(d_ly, 0.15),
            "flight_time_yr_at_0_20c": travel_time_yr(d_ly, 0.20),
        },
        "dominant_unknowns": ["尘埃通量与粒径分布", "克级帆面材料性能", "推进效率与地面阵列规模"],
    }

    # 档案载荷：动能下限对质量的线性敏感性
    m1 = cases["crewed_1mt"]["mass_kg"]
    ke1 = kinetic_energy_classical_j(m1, v_c, c)
    archive = {
        "route_id": "uncrewed_civilization_archive",
        "locked_cruise_speed_c": v_c,
        "travel_time_yr": travel_time_yr(d_ly, v_c),
        "ke_lower_bound_j_per_1mt": ke1,
        "sensitivity": {
            "ke_j_at_0_5mt": kinetic_energy_classical_j(0.5 * m1, v_c, c),
            "ke_j_at_2mt": kinetic_energy_classical_j(2.0 * m1, v_c, c),
        },
        "dominant_unknowns": ["长期存储介质寿命直接证据", "读取设备再制造方案", "运输方案（另行论证）"],
    }

    # 载人飞行器：人工重力对半径/转速的敏感性 + 5Mt 动能下限
    m5 = cases["orbital_material_5mt"]["mass_kg"]
    hab = cases["rotating_habitat"]
    crewed = {
        "route_id": "crewed_interstellar_vehicle",
        "locked_cruise_speed_c": v_c,
        "travel_time_yr": travel_time_yr(d_ly, v_c),
        "ke_lower_bound_j_at_5mt": kinetic_energy_classical_j(m5, v_c, c),
        "artificial_gravity_m_s2_at_1km_2rpm": artificial_gravity_m_s2(
            hab["radius_m"], hab["rotation_rpm"]
        ),
        "sensitivity": {
            "rpm_for_1g_at_1km": rpm_for_gravity(hab["radius_m"], g0),
            "gravity_m_s2_at_1km_1rpm": artificial_gravity_m_s2(hab["radius_m"], 1.0),
            "gravity_m_s2_at_0_5km_2rpm": artificial_gravity_m_s2(500.0, 2.0),
        },
        "dominant_unknowns": ["封闭生态百年级验证", "辐射防护方案", "在轨工业与再制造能力", "长期社会治理"],
    }

    return {
        "note": (
            "速度按基线锁定 0.03c；敏感性仅在质量/效率/半径/转速与路线内部参数上展开。"
            "三路线可分离，禁止跨路线可行性外推 (RQ-SCP-003)。"
        ),
        "routes": [laser, archive, crewed],
        "fact_label": "derived_result",
    }


# ---------------------------------------------------------------------------
# 全量计算
# ---------------------------------------------------------------------------
def compute_all(params, propulsion_efficiency=0.5):
    """以基线参数执行八类计算，返回带单位/公式/来源标识的结果树。"""
    consts = params["constants"]
    c = consts["speed_of_light_m_s"]
    g0 = consts["standard_gravity_m_s2"]
    d_ly = consts["proxima_distance_ly"]
    scenarios = params["scenarios"]["cruise_speed_c"]
    if scenarios != [0.03]:
        raise ValueError("baseline scenario lock violated: expected [0.03]")
    v_c = scenarios[0]
    cases = {c_["id"]: c_ for c_ in params["reference_cases"]}

    m1 = cases["crewed_1mt"]["mass_kg"]
    m5 = cases["orbital_material_5mt"]["mass_kg"]
    dust_m = cases["dust_1mg"]["mass_kg"]
    dust_v = cases["dust_1mg"]["relative_speed_c"]
    hab = cases["rotating_habitat"]

    ke1 = kinetic_energy_classical_j(m1, v_c, c)
    dust_e = dust_impact_energy_j(dust_m, dust_v, c)

    results = {
        "traceability": traceability(params),
        "scope": "Level-0 / Pre-Phase A conceptual study; single 0.03c scenario (locked)",
        "formulas": {
            "travel_time_yr": "proxima_distance_ly / cruise_speed_c (ICD-FML-001; 匀速, 不含加减速)",
            "kinetic_energy_j": "0.5 * mass_kg * (cruise_speed_c * c)^2 (ICD-FML-002; 下限, 非完整能源预算)",
            "kinetic_energy_relativistic_j": "(gamma-1) * m * c^2 (对照, 分列不混用)",
            "artificial_gravity_m_s2": "(2*pi*rotation_rpm/60)^2 * radius_m (ICD-FML-003)",
            "dust_impact_energy_j": "0.5 * dust_mass_kg * (relative_speed_c * c)^2 (ICD-FML-004)",
            "turn_angle_deg": "degrees(dv/v) 小角近似 (ICD-FML-005)",
        },
        "travel": {
            "travel_time_yr": travel_time_yr(d_ly, v_c),
            "light_travel_time_yr": light_travel_time_yr(d_ly),
            "cruise_speed_c": v_c,
            "distance_ly": d_ly,
            "boundary": "匀速巡航；未计加速、减速与航向修正；区分'飞离太阳系'与'抵达目标恒星系统'",
            "fact_label": "derived_result",
        },
        "kinetic_energy": {
            "classical_1mt_j": ke1,
            "classical_5mt_j": kinetic_energy_classical_j(m5, v_c, c),
            "relativistic_1mt_j": kinetic_energy_relativistic_j(m1, v_c, c),
            "relativistic_5mt_j": kinetic_energy_relativistic_j(m5, v_c, c),
            "relativistic_correction_pct_1mt": 100.0
            * (kinetic_energy_relativistic_j(m1, v_c, c) / ke1 - 1.0),
            "boundary": "动能下限；未计效率、排气动能、推进剂、减速、备用、建造损耗；非完整推进能源预算",
            "fact_label": "derived_result",
        },
        "energy_scenarios": {
            "crewed_1mt": mission_energy_scenarios(m1, v_c, c, propulsion_efficiency),
            "orbital_material_5mt": mission_energy_scenarios(m5, v_c, c, propulsion_efficiency),
        },
        "artificial_gravity": {
            "reference_a_m_s2": artificial_gravity_m_s2(hab["radius_m"], hab["rotation_rpm"]),
            "reference_g_ratio": artificial_gravity_m_s2(hab["radius_m"], hab["rotation_rpm"]) / g0,
            "rpm_for_1g_at_1km": rpm_for_gravity(hab["radius_m"], g0),
            "inputs": {"radius_m": hab["radius_m"], "rotation_rpm": hab["rotation_rpm"]},
            "boundary": "舱体半径、转速、允许重力和人体适应性为联合设计变量",
            "fact_label": "derived_result",
        },
        "dust_impact": {
            "energy_1mg_j": dust_e,
            "tnt_equivalent_kg_1mg": tnt_equivalent_kg(dust_e),
            "energy_10mg_j": dust_impact_energy_j(10.0 * dust_m, dust_v, c),
            "tnt_convention": "1 kg TNT = 4.184e6 J [assumption]",
            "withdrawn_claim_guard": "旧稿'1mg约450兆焦'已撤回，不得复现 (RQ-MDL-006)",
            "fact_label": "derived_result",
        },
        "retargeting": retargeting_assessment(),
        "precursor": precursor_timeline(d_ly),
        "route_sensitivity": route_sensitivity(params),
        "unknowns_open": [
            "尘埃通量与粒径分布（无直接证据）",
            "帆面/盾体材料性能（无直接证据）",
            "推进效率（无直接证据，eta 为可调假设）",
            "退款/回传时间之外的任务时间线细节（无直接证据）",
        ],
    }
    return results


def main():
    params = load_params()
    results = compute_all(params)
    out_path = os.path.join(HERE, "out", "model_results.json")
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    print("wrote", out_path)
    print("travel_time_yr =", results["travel"]["travel_time_yr"])
    print("ke_1mt_j =", results["kinetic_energy"]["classical_1mt_j"])
    print("ke_5mt_j =", results["kinetic_energy"]["classical_5mt_j"])
    print("gravity_1km_2rpm =", results["artificial_gravity"]["reference_a_m_s2"])
    print("dust_1mg_j =", results["dust_impact"]["energy_1mg_j"])


if __name__ == "__main__":
    main()
