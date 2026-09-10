// 日冕计划 · 应用计算内核 (与 outputs/model/corona_model.py 同公式同参数, RQ-APP-006)
// 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
//         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
// 浏览器与 Node 双环境可用 (供 check_app.js 有界校验)。
(function (root, factory) {
  var api = factory();
  if (typeof module !== "undefined" && module.exports) { module.exports = api; }
  root.CoronaCore = api;
})(typeof self !== "undefined" ? self : globalThis, function () {
  "use strict";

  var TNT_J_PER_KG = 4.184e6; // [assumption] 常用换算约定

  // ICD-FML-001
  function travelTimeYr(distanceLy, cruiseSpeedC) { return distanceLy / cruiseSpeedC; }

  // ICD-FML-002 (非相对论下限; 相对论对照单列)
  function kineticEnergyNonrelJ(massKg, speedC, cMs) {
    var v = speedC * cMs;
    return 0.5 * massKg * v * v;
  }
  function kineticEnergyRelJ(massKg, speedC, cMs) {
    var gamma = 1 / Math.sqrt(1 - speedC * speedC);
    return (gamma - 1) * massKg * cMs * cMs;
  }

  // 效率情景: 最小输入能量下限 (非能源预算)
  function minInputEnergyJ(kineticJ, efficiency) { return kineticJ / efficiency; }

  // ICD-FML-003
  function artificialGravityMs2(radiusM, rotationRpm) {
    var omega = 2 * Math.PI * rotationRpm / 60;
    return omega * omega * radiusM;
  }
  function rpmForTargetGravity(radiusM, targetMs2) {
    return (60 / (2 * Math.PI)) * Math.sqrt(targetMs2 / radiusM);
  }

  // ICD-FML-004
  function dustImpactEnergyJ(dustMassKg, relativeSpeedC, cMs) {
    var v = relativeSpeedC * cMs;
    return 0.5 * dustMassKg * v * v;
  }
  function tntKg(energyJ) { return energyJ / TNT_J_PER_KG; }

  // ICD-FML-005
  function turnAngleLimitDeg(deltaVFraction) { return deltaVFraction * 180 / Math.PI; }

  // 汇总: 给定情景输入, 输出全部显示量 (含事实标签)
  function computeScenario(params, inputs) {
    var c = params.constants.speed_of_light_m_s;
    var g0 = params.constants.standard_gravity_m_s2;
    var distLy = params.constants.proxima_distance_ly;
    var vC = params.scenarios.cruise_speed_c[0]; // 锁定 0.03c
    var dustCase = params.reference_cases.filter(function (r) { return r.id === "dust_1mg"; })[0];

    var ke = kineticEnergyNonrelJ(inputs.massKg, vC, c);
    var dustE = dustImpactEnergyJ(dustCase.mass_kg, dustCase.relative_speed_c, c);
    var grav = artificialGravityMs2(inputs.radiusM, inputs.rotationRpm);

    return {
      traceability: params.traceability,
      scenario: { cruise_speed_c: vC, locked: true, fact_label: "verified_fact" },
      inputs: {
        mass_kg: inputs.massKg,
        efficiency: inputs.efficiency,
        radius_m: inputs.radiusM,
        rotation_rpm: inputs.rotationRpm,
        dust_mass_kg: dustCase.mass_kg
      },
      outputs: {
        travel_time_yr: { value: travelTimeYr(distLy, vC), unit: "yr",
          formula: "proxima_distance_ly / cruise_speed_c", fact_label: "derived_result",
          note: "匀速巡航下限, 不含加速/减速/航向修正" },
        kinetic_energy_nonrel_j: { value: ke, unit: "J",
          formula: "0.5 * mass_kg * (cruise_speed_c * c)^2", fact_label: "derived_result",
          note: "自身动能下限, 非完整推进能源预算" },
        kinetic_energy_rel_j: { value: kineticEnergyRelJ(inputs.massKg, vC, c), unit: "J",
          formula: "(gamma-1) * m * c^2", fact_label: "derived_result",
          note: "相对论对照, 与非相对论分列" },
        equivalent_tnt_mt: { value: tntKg(ke) / 1e9, unit: "Gt TNT",
          formula: "E / 4.184e6 / 1e9", fact_label: "derived_result",
          note: "换算约定 1 kg TNT = 4.184e6 J [assumption]" },
        min_input_energy_j: { value: minInputEnergyJ(ke, inputs.efficiency), unit: "J",
          formula: "kinetic_energy_nonrel_j / efficiency", fact_label: "derived_result",
          note: "单边加速最小输入能量下限; 效率为情景假设 [assumption]" },
        artificial_gravity_m_s2: { value: grav, unit: "m/s^2",
          formula: "(2*pi*rpm/60)^2 * radius_m", fact_label: "derived_result" },
        artificial_gravity_g0: { value: grav / g0, unit: "g0",
          formula: "a / 9.80665", fact_label: "derived_result" },
        dust_impact_energy_j: { value: dustE, unit: "J",
          formula: "0.5 * dust_mass_kg * (relative_speed_c * c)^2", fact_label: "derived_result" },
        dust_impact_tnt_kg: { value: tntKg(dustE), unit: "kg TNT",
          formula: "E / 4.184e6", fact_label: "derived_result" }
      }
    };
  }

  return {
    TNT_J_PER_KG: TNT_J_PER_KG,
    travelTimeYr: travelTimeYr,
    kineticEnergyNonrelJ: kineticEnergyNonrelJ,
    kineticEnergyRelJ: kineticEnergyRelJ,
    minInputEnergyJ: minInputEnergyJ,
    artificialGravityMs2: artificialGravityMs2,
    rpmForTargetGravity: rpmForTargetGravity,
    dustImpactEnergyJ: dustImpactEnergyJ,
    tntKg: tntKg,
    turnAngleLimitDeg: turnAngleLimitDeg,
    computeScenario: computeScenario
  };
});
