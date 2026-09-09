/*
 * params.js —— 日冕计划 Pre-Phase A 统一参数（应用侧副本）。
 * 与 model/params.json 一致（baseline_id / cruise_speed_c / 常量 / 参考案例 / 禁用语 / 真值标签）。
 * 单一事实源：model/params.json（= baseline.yaml）。
 * 若修改，须走变更控制，不得就地覆盖基线。
 */
window.CORONA_PARAMS = {
  schema_version: 1,
  baseline_id: "corona-prephase-a-v1",
  baseline_version: "1.0.0",
  status: "frozen_for_validation",
  scope: "Level-0 / Pre-Phase A conceptual study",
  scenarios: {
    cruise_speed_c: [0.01, 0.03, 0.05],
    default_scenario_c: 0.03,
    scenario_ids: ["S-0.01c", "S-0.03c", "S-0.05c"]
  },
  constants: {
    speed_of_light_m_s: 299792458,
    standard_gravity_m_s2: 9.80665,
    proxima_distance_ly: 4.25,
    ly_m: 9460730472580800.0,
    sec_per_year: 31557600,
    tnt_equivalent_J_per_kg: 4184000,
    au_m: 149597870700.0,
    solar_lens_distance_au: 550.0,
    precursor_distance_au: 1000.0
  },
  reference_cases: [
    { id: "crewed_1mt", mass_kg: 1000000000, cruise_speed_c: 0.03 },
    { id: "orbital_material_5mt", mass_kg: 5000000000, cruise_speed_c: 0.03 },
    { id: "dust_1mg", mass_kg: 0.000001, relative_speed_c: 0.03 },
    { id: "rotating_habitat", radius_m: 1000, rotation_rpm: 2 }
  ],
  starshot_anchor: {
    target_velocity_c: 0.2,
    beam_power_W: 100000000000.0,
    sail_mass_scale: "gram-scale (~1 g)"
  },
  routes: [
    "laser_sail_precursor",
    "uncrewed_civilization_archive",
    "crewed_interstellar_vehicle"
  ],
  truth_labels: ["verified_fact", "derived_result", "assumption", "unknown"],
  forbidden_claims: [
    "construction_ready",
    "manufacturing_ready",
    "flight_certified",
    "whole_program_cost_committed"
  ],
  acceptance_reference_checks: {
    travel_years_at_0_03c: { expected: 141.6666667, relative_tolerance: 0.001 },
    kinetic_energy_1mt_at_0_03c_j: { expected: 4.0443983e22, relative_tolerance: 0.01 },
    kinetic_energy_5mt_at_0_03c_j: { expected: 2.0221991e23, relative_tolerance: 0.01 },
    gravity_1km_2rpm_m_s2: { expected: 43.8649, relative_tolerance: 0.01 },
    dust_1mg_0_03c_j: { expected: 40443983, relative_tolerance: 0.01 }
  }
};
