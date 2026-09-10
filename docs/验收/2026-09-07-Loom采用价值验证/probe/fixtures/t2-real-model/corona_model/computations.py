# ============================================================================
# corona_model/computations.py — 任务书 §4.1 八类计算（统一情景计算层）
# ----------------------------------------------------------------------------
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# 每个计算函数返回结构化记录：
#   {id, value(s), unit, formula_ref, inputs, fact_label, boundary, source}
# 满足 RQ-MDL-010（单位/公式/输入/输出/来源标识）与四类事实标签纪律。
# 本轮唯一批准巡航情景：0.03c（baseline 1.0.0 锁定，RQ-SCP-001）。
# ============================================================================
"""Eight computation categories required by 任务书 §4.1."""

from __future__ import annotations

from . import physics

# 目标方向夹角（源自纪要 §二.5 所载公开天球坐标计算结果）[verified_fact]
TARGET_SEPARATIONS_DEG = {
    "proxima_to_barnard": 78.0,
    "proxima_to_tau_ceti": 101.0,
    "barnard_to_tau_ceti": 117.0,
}

# 先锋探测器路线内部工况（ICD-RTE-003：非巡航情景扩展）[verified_fact，源自纪要 §二.6]
PRECURSOR_PROBE_SPEED_C = 0.2

# 三路线参考质量（用于敏感性比较）：
# - 克级光帆探测器：纪要明确"克级"，取 1 g 量级代表值 [assumption]
# - 无人档案载荷：无直接证据，取 100 t 作为概念级代表值 [assumption]
# - 载人飞行器：1 Mt（reference_cases.crewed_1mt）[verified_fact]
ROUTE_REFERENCE_MASS_KG = {
    "laser_sail_precursor": {"mass_kg": 1e-3, "fact_label": "assumption",
                             "note": "克级代表值（纪要 §一：克级探测器）；具体质量属路线内部课题 [unknown]"},
    "uncrewed_civilization_archive": {"mass_kg": 1e5, "fact_label": "assumption",
                                      "note": "概念级代表值；运输方案另行论证，质量无直接证据 [unknown]"},
    "crewed_interstellar_vehicle": {"mass_kg": 1e9, "fact_label": "verified_fact",
                                    "note": "reference_cases.crewed_1mt（baseline_frozen.yaml）"},
}


def compute_travel_and_light_time(baseline) -> dict:
    """§4.1-1 航行时间与光行时（ICD-FML-001）。

    边界：匀速巡航，不含加速、减速与航向修正；"飞离太阳系"与
    "抵达目标恒星系统"为两种不同任务终态（RQ-SCP-004）。
    """
    c = baseline.constants
    v = baseline.cruise_scenarios[0]
    return {
        "id": "travel_and_light_time",
        "formula_ref": "ICD-FML-001",
        "inputs": {"proxima_distance_ly": c["proxima_distance_ly"], "cruise_speed_c": v},
        "outputs": {
            "travel_time_yr": physics.travel_time_yr(c["proxima_distance_ly"], v),
            "light_travel_time_yr": physics.light_travel_time_yr(c["proxima_distance_ly"]),
        },
        "unit": "yr",
        "fact_label": "derived_result",
        "boundary": "匀速巡航；未计加速、减速与航向修正；抵达并驻留还需与加速同量级的减速能力",
        "source": "baseline_frozen.yaml constants/scenarios; acceptance.json reference_checks",
    }


def compute_kinetic_energy_comparison(baseline) -> dict:
    """§4.1-2 非相对论与相对论动能对照（ICD-FML-002，RQ-MDL-003/004）。"""
    c = baseline.constants
    v = baseline.cruise_scenarios[0]
    cases = {}
    for case_id in ("crewed_1mt", "orbital_material_5mt"):
        m = baseline.reference_cases[case_id]["mass_kg"]
        ke_cl = physics.kinetic_energy_classical_j(m, v, c["speed_of_light_m_s"])
        ke_rel = physics.kinetic_energy_relativistic_j(m, v, c["speed_of_light_m_s"])
        cases[case_id] = {
            "mass_kg": m,
            "kinetic_energy_classical_j": ke_cl,
            "kinetic_energy_relativistic_j": ke_rel,
            "relativistic_minus_classical_fraction": (ke_rel - ke_cl) / ke_cl,
        }
    return {
        "id": "kinetic_energy_comparison",
        "formula_ref": "ICD-FML-002",
        "inputs": {"cruise_speed_c": v, "speed_of_light_m_s": c["speed_of_light_m_s"]},
        "outputs": cases,
        "unit": "J",
        "fact_label": "derived_result",
        "boundary": ("仅为飞行器自身动能下限；未计发动机效率、排气动能、推进剂、减速、"
                     "备用和建造损耗；不得用作工程能源预算（RQ-MDL-004）；"
                     "相对论对照单列，不得与非相对论值混用"),
        "source": "baseline_frozen.yaml reference_cases; acceptance.json reference_checks",
    }


def compute_accel_decel_efficiency_mass(baseline) -> dict:
    """§4.1-3 加速、减速、效率和质量情景。

    边界纪律：仅给出能量下限随效率的标度关系；推进效率本身无直接证据，
    标 [unknown]；减速需求按纪要 §二.1 作定性边界保留。
    """
    c = baseline.constants
    v = baseline.cruise_scenarios[0]
    mass_cases = {
        case_id: baseline.reference_cases[case_id]["mass_kg"]
        for case_id in ("crewed_1mt", "orbital_material_5mt")
    }
    efficiency_grid = [1.0, 0.5, 0.25, 0.1]  # 效率演示网格 [assumption]，非效率证据
    mass_scenarios = {}
    for case_id, m in mass_cases.items():
        ke = physics.kinetic_energy_classical_j(m, v, c["speed_of_light_m_s"])
        mass_scenarios[case_id] = {
            "mass_kg": m,
            "kinetic_energy_lower_bound_j": ke,
            "propulsion_input_lower_bound_by_efficiency_j": {
                f"eta_{eta:g}": physics.propulsion_energy_lower_bound_j(ke, eta)
                for eta in efficiency_grid
            },
        }
    return {
        "id": "accel_decel_efficiency_mass",
        "formula_ref": "ICD-FML-002 + propulsion_energy_lower_bound",
        "inputs": {"cruise_speed_c": v, "efficiency_grid": efficiency_grid},
        "outputs": {
            "mass_scenarios": mass_scenarios,
            "decel_requirement": ("若任务终态为进入目标恒星系统并驻留，需要与加速同量级的"
                                  "减速能力，能源与推进剂预算至少翻倍量级"),
        },
        "unit": "J",
        "fact_label": "derived_result",
        "boundary": ("效率网格为演示用假设值 [assumption]；真实推进效率 [unknown]；"
                     "E/η 仍为下限，未计排气动能、推进剂、减速、备用、建造损耗"),
        "source": "纪要 §二.1/§二.2; baseline_frozen.yaml reference_cases",
    }


def compute_artificial_gravity(baseline) -> dict:
    """§4.1-4 人工重力半径与转速（ICD-FML-003，RQ-MDL-005）。"""
    c = baseline.constants
    case = baseline.reference_cases["rotating_habitat"]
    r, rpm = case["radius_m"], case["rotation_rpm"]
    a = physics.artificial_gravity_m_s2(r, rpm)
    return {
        "id": "artificial_gravity",
        "formula_ref": "ICD-FML-003",
        "inputs": {"radius_m": r, "rotation_rpm": rpm,
                   "standard_gravity_m_s2": c["standard_gravity_m_s2"]},
        "outputs": {
            "artificial_gravity_m_s2": a,
            "in_standard_g": a / c["standard_gravity_m_s2"],
            "rpm_for_1g_same_radius": physics.rpm_for_target_gravity(r, c["standard_gravity_m_s2"]),
        },
        "unit": "m/s^2",
        "fact_label": "derived_result",
        "boundary": "舱体半径、转速、允许重力和人体适应性为联合设计变量；人体长期适应性 [unknown]",
        "source": "baseline_frozen.yaml reference_cases.rotating_habitat; acceptance.json reference_checks",
    }


def compute_dust_impact(baseline) -> dict:
    """§4.1-5 高速尘埃撞击能量（ICD-FML-004，RQ-MDL-006）。

    旧稿"1mg≈450MJ"已撤回，本模型不复现该数值。
    """
    c = baseline.constants
    case = baseline.reference_cases["dust_1mg"]
    m, v = case["mass_kg"], case["relative_speed_c"]
    e1 = physics.dust_impact_energy_j(m, v, c["speed_of_light_m_s"])
    e10 = physics.dust_impact_energy_j(10 * m, v, c["speed_of_light_m_s"])
    return {
        "id": "dust_impact",
        "formula_ref": "ICD-FML-004",
        "inputs": {"dust_mass_kg": m, "relative_speed_c": v},
        "outputs": {
            "dust_1mg_energy_j": e1,
            "dust_1mg_tnt_equivalent_kg": physics.tnt_equivalent_kg(e1),
            "dust_10mg_energy_j": e10,
            "dust_10mg_tnt_equivalent_kg": physics.tnt_equivalent_kg(e10),
        },
        "unit": "J",
        "fact_label": "derived_result",
        "boundary": ("TNT 换算约定 1 kg TNT = 4.184e6 J [assumption]；"
                     "尘埃通量、粒径分布、盾体面密度/间距/消耗率/补充方式/总质量均 [unknown]；"
                     "十千米级鞭普尔盾列仅为待评估概念"),
        "source": "baseline_frozen.yaml reference_cases.dust_1mg; acceptance.json reference_checks",
    }


def compute_target_geometry_turning(baseline) -> dict:
    """§4.1-6 目标方向夹角与速度增量转向上限（ICD-FML-005，RQ-MDL-007）。"""
    turn_low = physics.turn_angle_limit_deg(0.10)
    turn_high = physics.turn_angle_limit_deg(0.15)
    separations = dict(TARGET_SEPARATIONS_DEG)
    switchable = {
        pair: (sep <= turn_high) for pair, sep in separations.items()
    }
    return {
        "id": "target_geometry_turning",
        "formula_ref": "ICD-FML-005",
        "inputs": {"delta_v_fraction_range": [0.10, 0.15],
                   "target_separations_deg": separations},
        "outputs": {
            "turn_angle_limit_deg_min": turn_low,
            "turn_angle_limit_deg_max": turn_high,
            "mid_course_target_switch_possible": switchable,
            "conclusion": ("现有 10%–15% 速度增量无法支持三个目标之间的中途切换；"
                           "目标选择应在主加速前完成，航程中速度增量主要用于小角度修正和避险"),
        },
        "unit": "deg",
        "fact_label": "derived_result",
        "boundary": ("目标夹角值源自纪要 §二.5 所载公开天球坐标计算结果 [verified_fact]；"
                     "小角近似 turn_angle_rad ≈ Δv/v；保持巡航速度的理想条件"),
        "source": "纪要 §二.5",
    }


def compute_precursor_return(baseline) -> dict:
    """§4.1-7 先锋探测器到达和数据回传时间（RQ-MDL-008）。"""
    c = baseline.constants
    r = physics.precursor_earliest_return_yr(c["proxima_distance_ly"], PRECURSOR_PROBE_SPEED_C)
    return {
        "id": "precursor_return",
        "formula_ref": "physics.precursor_earliest_return_yr",
        "inputs": {"proxima_distance_ly": c["proxima_distance_ly"],
                   "probe_speed_c": PRECURSOR_PROBE_SPEED_C},
        "outputs": {
            "flight_time_min_yr": r["flight_time_min_yr"],
            "data_return_yr": r["data_return_yr"],
            "earliest_return_yr": r["earliest_return_yr"],
            "decision_constraint": ("先锋数据不得作为第 20 年以前母舰决策条件；"
                                    "考虑系统研制与部署，真实时间更长"),
        },
        "unit": "yr",
        "fact_label": "derived_result",
        "boundary": ("0.2c 为探测器路线自身参数（ICD-RTE-003），非本轮巡航情景扩展；"
                     "最低飞行时间为匀速下限，未计加速段"),
        "source": "纪要 §二.6",
    }


def compute_route_sensitivity(baseline) -> dict:
    """§4.1-8 三路线敏感性比较（锁定 0.03c 情景下）。

    可分离性纪律（RQ-SCP-003）：各路线独立成栏，禁止跨路线可行性外推；
    克级与百吨级质量为代表性假设值，仅用于量级敏感性展示。
    """
    c = baseline.constants
    v = baseline.cruise_scenarios[0]
    routes = {}
    for route in baseline.routes:
        rid = route["id"]
        spec = ROUTE_REFERENCE_MASS_KG[rid]
        m = spec["mass_kg"]
        ke = physics.kinetic_energy_classical_j(m, v, c["speed_of_light_m_s"])
        routes[rid] = {
            "route_name": route["name"],
            "reference_mass_kg": m,
            "reference_mass_fact_label": spec["fact_label"],
            "reference_mass_note": spec["note"],
            "travel_time_yr_at_locked_0_03c": physics.travel_time_yr(c["proxima_distance_ly"], v),
            "kinetic_energy_lower_bound_j": ke,
            "analytic_sensitivity": {
                "travel_time_scaling": "t ∝ 1/v（∂t/∂v = -d/v²，解析关系，非扩展情景）",
                "kinetic_energy_scaling": "KE ∝ m·v²",
            },
        }
    return {
        "id": "route_sensitivity",
        "formula_ref": "ICD-FML-001/002",
        "inputs": {"cruise_speed_c_locked": v},
        "outputs": routes,
        "unit": "mixed",
        "fact_label": "derived_result",
        "boundary": ("速度敏感性为解析标度关系，不构成 0.01c/0.05c 扩展情景（RQ-SCP-001）；"
                     "三路线可分离，禁止用克级探测器可行性替代百万吨级载人飞行器可行性"),
        "source": "baseline_frozen.yaml routes/reference_cases; 纪要 §一",
    }


CATEGORY_COMPUTERS = [
    compute_travel_and_light_time,
    compute_kinetic_energy_comparison,
    compute_accel_decel_efficiency_mass,
    compute_artificial_gravity,
    compute_dust_impact,
    compute_target_geometry_turning,
    compute_precursor_return,
    compute_route_sensitivity,
]


def run_all(baseline) -> list:
    """运行 §4.1 全部八类计算，返回结构化记录列表。"""
    baseline.assert_scope_lock()
    return [fn(baseline) for fn in CATEGORY_COMPUTERS]
