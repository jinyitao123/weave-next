/*
 * model.js —— 日冕计划 Pre-Phase A 统一计算内核（应用侧）。
 * 与 model/corona_model.py 逐式同源：相同公式、相同参数来源（window.CORONA_PARAMS）。
 * 真值标签：verified_fact / derived_result / assumption / unknown。
 * 派生量命名：<量>_<对象>(CS)。
 */
"use strict";

const CoronaModel = (function () {
  const P = window.CORONA_PARAMS;

  // --- 冻结常量 ---
  const C_LIGHT = P.constants.speed_of_light_m_s;           // m/s  (verified_fact)
  const G0 = P.constants.standard_gravity_m_s2;             // m/s^2 (verified_fact)
  const DIST_PROXIMA_LY = P.constants.proxima_distance_ly;  // ly   (verified_fact)
  const LY_M = P.constants.ly_m;                            // m    (verified_fact)
  const TNT_J_PER_KG = P.constants.tnt_equivalent_J_per_kg; // J/kg (assumption)
  const AU_M = P.constants.au_m;                            // m    (verified_fact)
  const GLENS_DIST_AU = P.constants.solar_lens_distance_au; // AU
  const PRECURSOR_DIST_AU = P.constants.precursor_distance_au; // AU

  const STARSHOT_V = P.starshot_anchor.target_velocity_c;   // 0.2c (B 级锚点)
  const STARSHOT_P = P.starshot_anchor.beam_power_W;        // 100 GW (B 级锚点)

  const CRUISE_SPEEDS_C = P.scenarios.cruise_speed_c;
  const SCENARIO_IDS = P.scenarios.scenario_ids;

  // 情景 ID <-> c
  const SCENARIO_TO_C = {};
  const C_TO_SCENARIO = {};
  for (let i = 0; i < CRUISE_SPEEDS_C.length; i++) {
    SCENARIO_TO_C[SCENARIO_IDS[i]] = CRUISE_SPEEDS_C[i];
    C_TO_SCENARIO[CRUISE_SPEEDS_C[i]] = SCENARIO_IDS[i];
  }
  const DEFAULT_SCENARIO = P.scenarios.default_scenario_c;

  const PI = Math.PI;

  function scenarioIdForC(c) {
    return C_TO_SCENARIO[c] || null;
  }

  function gravityRotatingHabitat(radius_m, rotation_rpm) {
    const omega = (2 * PI * rotation_rpm) / 60.0;
    return omega * omega * radius_m;
  }

  function rpmForG0(radius_m) {
    const omega = Math.sqrt(G0 / radius_m);
    return (omega * 60.0) / (2 * PI);
  }

  function vKMPS(c) { return (c * C_LIGHT) / 1000.0; }
  function speedMS(c) { return c * C_LIGHT; }

  function timeAlphaYrs(c) { return DIST_PROXIMA_LY / c; }

  function timeGLensYrs(c) { const vAUYr = (c * LY_M) / AU_M; return GLENS_DIST_AU / vAUYr; }

  function timePrecursorYrs(c) { const vAUYr = (c * LY_M) / AU_M; return PRECURSOR_DIST_AU / vAUYr; }

  function keKg(kg, c) { const v = speedMS(c); return 0.5 * kg * v * v; }

  function ke1mtJ(c) { return keKg(1.0e9, c); }
  function ke5mtJ(c) { return keKg(5.0e9, c); }
  function keDust1mgJ(c) { return keKg(1.0e-6, c); }

  function tntTonnes(j) { return j / (TNT_J_PER_KG * 1000.0); }

  function relativisticKe1mtJ(c) {
    const beta = c;
    const gamma = 1.0 / Math.sqrt(1.0 - beta * beta);
    return (gamma - 1.0) * 1.0e9 * C_LIGHT * C_LIGHT;
  }

  function pRel(c) { return (STARSHOT_V / c) * (STARSHOT_V / c); }

  function beamPowerGW(c) { return STARSHOT_P / pRel(c) / 1.0e9; }

  function computeScenario(c) {
    c = +c;
    return {
      scenario_id: scenarioIdForC(c),
      cruise_speed_c: c,
      v_kmps: vKMPS(c),
      time_alpha_yrs: timeAlphaYrs(c),
      time_glens_yrs: timeGLensYrs(c),
      time_glens_days: timeGLensYrs(c) * 365.25,
      time_precursor_yrs: timePrecursorYrs(c),
      time_precursor_months: timePrecursorYrs(c) * 12.0,
      ke_1mt_j: ke1mtJ(c),
      ke_5mt_j: ke5mtJ(c),
      ke_1mt_tnt_tonnes: tntTonnes(ke1mtJ(c)),
      ke_dust_1mg_j: keDust1mgJ(c),
      ke_dust_1mg_tnt_kg: tntTonnes(keDust1mgJ(c)) * 1000.0,
      rel_ke_1mt_j: relativisticKe1mtJ(c),
      rel_correction_pct: ((relativisticKe1mtJ(c) - ke1mtJ(c)) / ke1mtJ(c)) * 100.0,
      p_rel: pRel(c),
      beam_power_gw: beamPowerGW(c)
    };
  }

  function computeAll() {
    const out = {};
    for (let i = 0; i < CRUISE_SPEEDS_C.length; i++) {
      out[SCENARIO_IDS[i]] = computeScenario(CRUISE_SPEEDS_C[i]);
    }
    return out;
  }

  function referenceCheck() {
    const computed = {
      travel_years_at_0_03c: timeAlphaYrs(0.03),
      kinetic_energy_1mt_at_0_03c_j: ke1mtJ(0.03),
      kinetic_energy_5mt_at_0_03c_j: ke5mtJ(0.03),
      gravity_1km_2rpm_m_s2: gravityRotatingHabitat(1000.0, 2.0),
      dust_1mg_0_03c_j: keDust1mgJ(0.03)
    };
    let allOk = true;
    const rows = [];
    const checks = P.acceptance_reference_checks;
    for (const name of Object.keys(checks)) {
      const spec = checks[name];
      const val = computed[name];
      const expected = spec.expected;
      const tol = spec.relative_tolerance;
      const relErr = Math.abs(val - expected) / Math.abs(expected);
      const ok = relErr <= tol;
      if (!ok) allOk = false;
      rows.push({ name: name, computed: val, expected: expected, rel_err: relErr, tol: tol, ok: ok });
    }
    return { all_ok: allOk, rows: rows };
  }

  return {
    P: P,
    C_LIGHT: C_LIGHT,
    G0: G0,
    DIST_PROXIMA_LY: DIST_PROXIMA_LY,
    LY_M: LY_M,
    TNT_J_PER_KG: TNT_J_PER_KG,
    AU_M: AU_M,
    STARSHOT_V: STARSHOT_V,
    STARSHOT_P: STARSHOT_P,
    CRUISE_SPEEDS_C: CRUISE_SPEEDS_C,
    SCENARIO_IDS: SCENARIO_IDS,
    SCENARIO_TO_C: SCENARIO_TO_C,
    DEFAULT_SCENARIO: DEFAULT_SCENARIO,
    scenarioIdForC: scenarioIdForC,
    gravityRotatingHabitat: gravityRotatingHabitat,
    rpmForG0: rpmForG0,
    vKMPS: vKMPS,
    speedMS: speedMS,
    timeAlphaYrs: timeAlphaYrs,
    timeGLensYrs: timeGLensYrs,
    timePrecursorYrs: timePrecursorYrs,
    keKg: keKg,
    ke1mtJ: ke1mtJ,
    ke5mtJ: ke5mtJ,
    keDust1mgJ: keDust1mgJ,
    tntTonnes: tntTonnes,
    relativisticKe1mtJ: relativisticKe1mtJ,
    pRel: pRel,
    beamPowerGW: beamPowerGW,
    computeScenario: computeScenario,
    computeAll: computeAll,
    referenceCheck: referenceCheck
  };
})();

// 供应用与自动化测试统一取用
window.CoronaModel = CoronaModel;
