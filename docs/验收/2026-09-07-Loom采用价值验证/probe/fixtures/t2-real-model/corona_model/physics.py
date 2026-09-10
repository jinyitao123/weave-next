# ============================================================================
# corona_model/physics.py — 规范公式实现 (ICD-FML-001 ~ ICD-FML-005)
# ----------------------------------------------------------------------------
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# 模型↔应用↔图纸注记必须使用同一组公式。应用侧 calc.js 与本文件逐式对应。
# 每个函数返回 (数值, 单位, 公式标识, 事实标签)。
# 事实标签：verified_fact / derived_result / assumption / unknown。
# 边界纪律（RQ-MDL-004）：动能仅为下限，未计效率/排气动能/推进剂/减速/
# 备用/建造损耗，禁止表述为完整推进能源预算。
# ============================================================================
"""Canonical formulas shared by model, app and drawing annotations."""

from __future__ import annotations

import math

TNT_EQUIVALENT_J_PER_KG = 4.184e6  # 常用换算约定 [assumption] (ICD-FML-004)


def travel_time_yr(distance_ly: float, cruise_speed_c: float) -> float:
    """ICD-FML-001: travel_time_yr = distance_ly / cruise_speed_c.

    匀速巡航，不含加速、减速与航向修正（边界须随结果显式标注）。
    [derived_result]
    """
    if cruise_speed_c <= 0:
        raise ValueError("cruise_speed_c must be positive")
    return distance_ly / cruise_speed_c


def light_travel_time_yr(distance_ly: float) -> float:
    """光行时：光跨越该距离所需年数，数值上等于距离（ly）。[derived_result]"""
    return distance_ly


def kinetic_energy_classical_j(mass_kg: float, speed_c: float, c_m_s: float) -> float:
    """ICD-FML-002: KE = 0.5 * mass_kg * (speed_c * c)^2（非相对论下限）。

    仅为飞行器自身动能下限；不得表述为完整推进能源预算。[derived_result]
    """
    v = speed_c * c_m_s
    return 0.5 * mass_kg * v * v


def lorentz_gamma(speed_c: float) -> float:
    """洛伦兹因子 γ = 1/sqrt(1-β²)。"""
    if not 0 <= speed_c < 1:
        raise ValueError("speed_c must be in [0, 1)")
    return 1.0 / math.sqrt(1.0 - speed_c * speed_c)


def kinetic_energy_relativistic_j(mass_kg: float, speed_c: float, c_m_s: float) -> float:
    """ICD-FML-002 相对论对照: KE_rel = (γ-1) * m * c²（单列，不得与非相对论值混用）。"""
    return (lorentz_gamma(speed_c) - 1.0) * mass_kg * c_m_s * c_m_s


def artificial_gravity_m_s2(radius_m: float, rotation_rpm: float) -> float:
    """ICD-FML-003: a = (2π * rpm / 60)^2 * radius_m。[derived_result]"""
    omega = 2.0 * math.pi * rotation_rpm / 60.0
    return omega * omega * radius_m


def rpm_for_target_gravity(radius_m: float, target_m_s2: float) -> float:
    """给定半径与目标加速度求转速（rpm）：rpm = 60/(2π) * sqrt(a/r)。"""
    if radius_m <= 0:
        raise ValueError("radius_m must be positive")
    omega = math.sqrt(target_m_s2 / radius_m)
    return omega * 60.0 / (2.0 * math.pi)


def dust_impact_energy_j(dust_mass_kg: float, relative_speed_c: float, c_m_s: float) -> float:
    """ICD-FML-004: E = 0.5 * dust_mass_kg * (relative_speed_c * c)^2。[derived_result]"""
    v = relative_speed_c * c_m_s
    return 0.5 * dust_mass_kg * v * v


def tnt_equivalent_kg(energy_j: float) -> float:
    """TNT 当量（kg）= E / 4.184e6 J/kg；换算约定来源标注为 [assumption]。"""
    return energy_j / TNT_EQUIVALENT_J_PER_KG


def turn_angle_limit_deg(delta_v_fraction: float) -> float:
    """ICD-FML-005: 小角近似 turn_angle_rad ≈ Δv/v，弧度转度。[derived_result]"""
    return math.degrees(delta_v_fraction)


def precursor_earliest_return_yr(distance_ly: float, probe_speed_c: float) -> dict:
    """先锋探测器：最低飞行时间 + 光行回传时间（纪要 §二.6）。

    0.2c 为探测器路线自身参数（ICD-RTE-003），非本轮巡航情景扩展。
    [derived_result]（输入 0.2c/4.25ly 为 [verified_fact]，源自纪要）
    """
    flight_yr = distance_ly / probe_speed_c
    return_yr = light_travel_time_yr(distance_ly)
    return {
        "flight_time_min_yr": flight_yr,
        "data_return_yr": return_yr,
        "earliest_return_yr": flight_yr + return_yr,
    }


def propulsion_energy_lower_bound_j(kinetic_energy_j: float, efficiency: float) -> float:
    """推进输入能量下限 = KE / η（η 为推进链总效率，0<η≤1）。

    仅为下限估计：仍未计排气动能、推进剂、减速、备用与建造损耗。
    η 的取值无直接证据，属 [assumption]/[unknown]，结果标 [derived_result]。
    """
    if not 0 < efficiency <= 1:
        raise ValueError("efficiency must be in (0, 1]")
    return kinetic_energy_j / efficiency
