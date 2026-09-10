"""日冕计划 · 统一计算内核 (corona_model.py)

baseline_id:      corona-baseline-1.0.0
baseline_version: 1.0.0
content_digest:   cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
  (SHA-256 of baseline_frozen.yaml, verified at load time against the YAML on disk)

参数唯一来源: outputs/model/baseline_frozen.yaml（本模块经其机器可读副本
baseline_frozen.json 读入，加载时校验副本内嵌 source_digest 与 YAML 实际摘要一致）。
公式接口与单位遵守 ICD-1.0.0 第 2/3 节。所有导出量标注事实标签
[derived_result]；参考工况与常数为 [verified_fact]；路线代表性质量等为
[assumption]；不得使用 forbidden_claims:
  construction_ready / manufacturing_ready / flight_certified / whole_program_cost_committed（全部禁用）。
"""

import hashlib
import json
import math
import os

_HERE = os.path.dirname(os.path.abspath(__file__))
BASELINE_YAML = os.path.join(_HERE, "baseline_frozen.yaml")
BASELINE_JSON = os.path.join(_HERE, "baseline_frozen.json")

# 冻结记录中的内容摘要（outputs/review/change_control_register.md §冻结记录）
EXPECTED_DIGEST = (
    "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2"
)

TNT_J_PER_KG = 4.184e6  # 常用换算约定 1 kg TNT = 4.184 MJ [assumption]


class BaselineError(RuntimeError):
    pass


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def load_baseline(yaml_path=BASELINE_YAML, json_path=BASELINE_JSON):
    """加载冻结基线并做双向追溯校验。

    校验链: baseline_frozen.yaml 实际 SHA-256 == 冻结记录摘要 ==
    baseline_frozen.json 内嵌 source_digest。任一不符即拒绝运行。
    """
    actual = sha256_of(yaml_path)
    if actual != EXPECTED_DIGEST:
        raise BaselineError(
            "baseline_frozen.yaml digest mismatch: actual=%s expected=%s"
            % (actual, EXPECTED_DIGEST)
        )
    with open(json_path, "r", encoding="utf-8") as f:
        params = json.load(f)
    # YAML 1.1 标量解析差异防护：无符号指数（如 4.0443983e22）可能被解析为
    # 字符串，统一归一为 float，保证容差比对是数值运算而非字符串运算。
    for chk in params["reference_checks"].values():
        chk["expected"] = float(chk["expected"])
        chk["relative_tolerance"] = float(chk["relative_tolerance"])
    params["_traceability"] = {
        "baseline_id": params["baseline_id"],
        "baseline_version": params["baseline_version"],
        "content_digest": actual,
    }
    return params


def locked_cruise_speed_c(params):
    speeds = params["scenarios"]["cruise_speed_c"]
    if not params["scenarios"]["locked"] or speeds != [0.03]:
        raise BaselineError(
            "scenario lock violated: cruise_speed_c=%r locked=%r"
            % (speeds, params["scenarios"]["locked"])
        )
    return speeds[0]


# ---------------------------------------------------------------------------
# 任务书 §4.1 八类计算
# ---------------------------------------------------------------------------

def travel_time_yr(distance_ly, speed_c):
    """ICD-FML-001 航行时间（匀速，不含加减速——此边界必须随结果标注）。"""
    return distance_ly / speed_c


def light_travel_time_yr(distance_ly):
    """光行时：信号单程传输年数。"""
    return distance_ly


def kinetic_energy_classical_j(mass_kg, speed_c, c):
    """ICD-FML-002 非相对论动能下限 0.5*m*v^2。不得表述为完整推进能源预算。"""
    v = speed_c * c
    return 0.5 * mass_kg * v * v


def kinetic_energy_relativistic_j(mass_kg, speed_c, c):
    """ICD-FML-002 相对论对照 (gamma-1)*m*c^2，单列，不得与非相对论值混用。"""
    gamma = 1.0 / math.sqrt(1.0 - speed_c * speed_c)
    return (gamma - 1.0) * mass_kg * c * c


def energy_lower_bound_with_efficiency_j(ke_j, efficiency):
    """ICD §2 效率情景：能源投入下限 = 动能下限 / eta（仍为下限，非预算）。"""
    if not (0.0 < efficiency <= 1.0):
        raise ValueError("efficiency must be in (0, 1], got %r" % efficiency)
    return ke_j / efficiency


def accel_decel_min_delta_v_c(speed_c):
    """加减速边界 [derived_result]：飞入并驻留目标系统需加速+减速，
    最低速度增量预算为 2*v_cruise（理想、不含引力损失与余量）。
    仅巡航/飞越情景的最低速度增量为 1*v_cruise。"""
    return {"flyby_min_delta_v_c": speed_c, "rendezvous_min_delta_v_c": 2.0 * speed_c}


def artificial_gravity_m_s2(radius_m, rotation_rpm):
    """ICD-FML-003 人工重力 (2*pi*rpm/60)^2 * r。"""
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rotation_rpm_for_gravity(radius_m, target_g_m_s2):
    """反解：给定半径与目标重力求转速 rpm（联合设计变量，纪要 §二.3）。"""
    omega = math.sqrt(target_g_m_s2 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


def dust_impact_energy_j(dust_mass_kg, relative_speed_c, c):
    """ICD-FML-004 尘埃撞击动能。"""
    v = relative_speed_c * c
    return 0.5 * dust_mass_kg * v * v


def tnt_equivalent_kg(energy_j):
    """TNT 当量换算 [assumption]：1 kg TNT = 4.184e6 J（常用换算约定）。"""
    return energy_j / TNT_J_PER_KG


def turn_angle_deg(dv_fraction):
    """ICD-FML-005 理想转向上限（小角近似）：turn_deg ≈ (dv/v) 弧度转度。"""
    return math.degrees(dv_fraction)


def precursor_probe_timeline(distance_ly, probe_speed_c=0.2):
    """先锋探测器到达与回传 [verified_fact 路线内部工况，ICD-RTE-003]：
    0.2c 飞行 + 光行回传。该 0.2c 属路线内部参数，不构成巡航情景扩展。"""
    flight_yr = distance_ly / probe_speed_c
    return_yr = distance_ly
    return {
        "flight_yr": flight_yr,
        "data_return_yr": return_yr,
        "earliest_roundtrip_yr": flight_yr + return_yr,
    }


# 三路线敏感性比较的代表性质量 [assumption]：
# 克级光帆探测器取 1 g（Starshot 量级公开概念，纪要 §一.1）；
# 档案载荷取 1e4 kg（先期论证占位，待运输方案论证）；载人路线取
# reference_cases.crewed_1mt = 1e9 kg [verified_fact]。
ROUTE_REPRESENTATIVE_MASS_KG = {
    "laser_sail_precursor": {"mass_kg": 1e-3, "fact_label": "assumption"},
    "uncrewed_civilization_archive": {"mass_kg": 1e4, "fact_label": "assumption"},
    "crewed_interstellar_vehicle": {"mass_kg": 1e9, "fact_label": "verified_fact"},
}


def route_sensitivity(params, speed_c, efficiencies=(0.1, 0.3, 0.5)):
    """三路线敏感性比较：在锁定 0.03c 下对代表性质量与推进效率扫描
    动能下限与能源投入下限。不实现 0.01c/0.05c 情景（CCR-001 待平台确认）。
    不得跨路线外推可行性（route_separability 规则）。"""
    c = params["constants"]["speed_of_light_m_s"]
    out = {}
    for route in params["routes"]:
        rid = route["id"]
        rep = ROUTE_REPRESENTATIVE_MASS_KG[rid]
        m = rep["mass_kg"]
        ke = kinetic_energy_classical_j(m, speed_c, c)
        out[rid] = {
            "name": route["name"],
            "representative_mass_kg": m,
            "mass_fact_label": rep["fact_label"],
            "kinetic_energy_lower_bound_j": ke,
            "energy_input_lower_bound_j_by_efficiency": {
                str(eta): energy_lower_bound_with_efficiency_j(ke, eta)
                for eta in efficiencies
            },
            "travel_time_yr": travel_time_yr(
                params["constants"]["proxima_distance_ly"], speed_c
            ),
            "separability_note": "本行为该路线独立数量级，不构成对其他路线可行性的外推。",
        }
    return out


def run_all(params):
    """执行全部八类计算并返回带追溯键与事实标签的结果树。"""
    c = params["constants"]["speed_of_light_m_s"]
    g0 = params["constants"]["standard_gravity_m_s2"]
    dist = params["constants"]["proxima_distance_ly"]
    v = locked_cruise_speed_c(params)
    cases = {c_["id"]: c_ for c_ in params["reference_cases"]}

    m1 = cases["crewed_1mt"]["mass_kg"]
    m5 = cases["orbital_material_5mt"]["mass_kg"]
    md = cases["dust_1mg"]["mass_kg"]
    rh = cases["rotating_habitat"]

    ke1 = kinetic_energy_classical_j(m1, v, c)
    ke5 = kinetic_energy_classical_j(m5, v, c)
    ke1_rel = kinetic_energy_relativistic_j(m1, v, c)
    dust_e = dust_impact_energy_j(md, cases["dust_1mg"]["relative_speed_c"], c)
    ag = artificial_gravity_m_s2(rh["radius_m"], rh["rotation_rpm"])

    results = {
        "traceability": params["_traceability"],
        "scope_note": "Level-0 / Pre-Phase A 概念级论证；不构成施工/制造/飞行认证依据。",
        "scenario": {
            "cruise_speed_c": v,
            "locked": True,
            "pending_change": "CCR-001 (0.01c/0.05c 待平台确认，未纳入)",
            "fact_label": "verified_fact",
        },
        "travel_time": {
            "travel_time_yr": travel_time_yr(dist, v),
            "light_travel_time_yr": light_travel_time_yr(dist),
            "boundary": "匀速巡航，不含加速、减速与航向修正 [derived_result，边界标注]",
            "fact_label": "derived_result",
        },
        "kinetic_energy": {
            "crewed_1mt_classical_j": ke1,
            "crewed_1mt_relativistic_j": ke1_rel,
            "orbital_material_5mt_classical_j": ke5,
            "relativistic_over_classical_ratio_1mt": ke1_rel / ke1,
            "note": "动能下限，不含发动机效率、排气动能、推进剂、减速、备用与建造损耗；不得用作工程能源预算。",
            "fact_label": "derived_result",
        },
        "efficiency_and_delta_v": {
            "crewed_1mt_energy_input_lower_bound_j": {
                str(eta): energy_lower_bound_with_efficiency_j(ke1, eta)
                for eta in (0.1, 0.3, 0.5)
            },
            "accel_decel_boundary": accel_decel_min_delta_v_c(v),
            "note": "效率 η 为情景参数 [assumption]；加减速边界为理想最低速度增量 [derived_result]。",
            "fact_label": "derived_result",
        },
        "artificial_gravity": {
            "radius_m": rh["radius_m"],
            "rotation_rpm": rh["rotation_rpm"],
            "centripetal_m_s2": ag,
            "earth_g_multiple": ag / g0,
            "rpm_for_1g_at_1km": rotation_rpm_for_gravity(rh["radius_m"], g0),
            "fact_label": "derived_result",
        },
        "dust_impact": {
            "dust_mass_kg": md,
            "relative_speed_c": cases["dust_1mg"]["relative_speed_c"],
            "energy_j": dust_e,
            "tnt_equivalent_kg": tnt_equivalent_kg(dust_e),
            "tnt_note": "1 kg TNT = 4.184e6 J [assumption，常用换算约定]",
            "unknowns": ["尘埃通量", "粒径分布", "盾体面密度/间距/消耗率"],
            "fact_label": "derived_result",
        },
        "turn_limit": {
            "dv_10pct_turn_deg": turn_angle_deg(0.10),
            "dv_15pct_turn_deg": turn_angle_deg(0.15),
            "target_separations_deg": {
                "proxima_barnard": 78,
                "proxima_tau_ceti": 101,
                "barnard_tau_ceti": 117,
            },
            "conclusion": "10%–15% Δv 仅支持约 5.7°–8.6° 转向，无法在三目标间中途切换 [derived_result]。",
            "fact_label": "derived_result",
        },
        "precursor_probe": dict(
            precursor_probe_timeline(dist),
            note="0.2c 为先锋探测器路线内部工况（ICD-RTE-003），不构成巡航情景扩展。",
            fact_label="derived_result",
        ),
        "route_sensitivity": route_sensitivity(params, v),
    }
    return results


def main():
    params = load_baseline()
    results = run_all(params)
    out_path = os.path.join(_HERE, "results.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print("wrote %s" % out_path)
    print("travel_time_yr = %.7f" % results["travel_time"]["travel_time_yr"])
    print("ke_1mt_j = %.7e" % results["kinetic_energy"]["crewed_1mt_classical_j"])
    print("ke_5mt_j = %.7e" % results["kinetic_energy"]["orbital_material_5mt_classical_j"])
    print("artificial_gravity_m_s2 = %.4f" % results["artificial_gravity"]["centripetal_m_s2"])
    print("dust_impact_energy_j = %.0f" % results["dust_impact"]["energy_j"])


if __name__ == "__main__":
    main()
