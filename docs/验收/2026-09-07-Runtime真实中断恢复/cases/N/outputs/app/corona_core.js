/* 日冕计划 · 应用计算内核 (corona_core.js)
 * 与 outputs/model/corona_model.py 同公式、同参数（ICD-1.0.0 §3，RQ-APP-006）。
 * 浏览器: window.CoronaCore；Node: module.exports（供有界一致性测试）。
 * 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
 *         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
 */
(function (root, factory) {
  if (typeof module === "object" && module.exports) { module.exports = factory(); }
  else { root.CoronaCore = factory(); }
}(typeof self !== "undefined" ? self : this, function () {
  "use strict";

  var TRACE = {
    baseline_id: "corona-baseline-1.0.0",
    baseline_version: "1.0.0",
    content_digest: "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2"
  };
  var TNT_J_PER_KG = 4.184e6; // 常用换算约定 [assumption]

  // ICD-FML-001 匀速航行时间（不含加减速）
  function travelTimeYr(distanceLy, speedC) { return distanceLy / speedC; }
  function lightTravelTimeYr(distanceLy) { return distanceLy; }

  // ICD-FML-002 非相对论动能下限
  function kineticEnergyClassicalJ(massKg, speedC, c) {
    var v = speedC * c; return 0.5 * massKg * v * v;
  }
  // ICD-FML-002 相对论对照（单列，不得混用）
  function kineticEnergyRelativisticJ(massKg, speedC, c) {
    var gamma = 1 / Math.sqrt(1 - speedC * speedC);
    return (gamma - 1) * massKg * c * c;
  }
  // 能源投入下限 = KE/η（仍为下限，非预算）
  function energyInputLowerBoundJ(keJ, efficiency) {
    if (!(efficiency > 0 && efficiency <= 1)) throw new Error("efficiency must be in (0,1]");
    return keJ / efficiency;
  }
  // ICD-FML-003 人工重力 ω²r
  function artificialGravityMS2(radiusM, rpm) {
    var omega = 2 * Math.PI * rpm / 60; return omega * omega * radiusM;
  }
  function rpmForGravity(radiusM, targetGMS2) {
    return Math.sqrt(targetGMS2 / radiusM) * 60 / (2 * Math.PI);
  }
  // ICD-FML-004 尘埃撞击
  function dustImpactEnergyJ(dustMassKg, relSpeedC, c) {
    var v = relSpeedC * c; return 0.5 * dustMassKg * v * v;
  }
  function tntEquivalentKg(energyJ) { return energyJ / TNT_J_PER_KG; }
  // ICD-FML-005 理想转向上限（小角近似）
  function turnAngleDeg(dvFraction) { return dvFraction * 180 / Math.PI; }

  /* 计算当前情景全部导出量。speedC 锁定 0.03（CCR-001 未纳入）；
   * 三情景比较仅预留接口 compareScenariosStub，不实现 0.01c/0.05c。 */
  function computeScenario(params, inputs) {
    var c = params.constants.speed_of_light_m_s;
    var g0 = params.constants.standard_gravity_m_s2;
    var dist = params.constants.proxima_distance_ly;
    var v = params.scenario.cruise_speed_c; // 0.03 锁定
    if (inputs.cruise_speed_c !== undefined && inputs.cruise_speed_c !== v) {
      throw new Error("cruise_speed_c locked at 0.03 (CCR-001 pending platform confirmation)");
    }
    var ke = kineticEnergyClassicalJ(inputs.mass_kg, v, c);
    var keRel = kineticEnergyRelativisticJ(inputs.mass_kg, v, c);
    var dust = dustImpactEnergyJ(inputs.dust_mass_kg, v, c);
    var ag = artificialGravityMS2(inputs.radius_m, inputs.rotation_rpm);
    return {
      traceability: TRACE,
      scope_note: "Level-0 / Pre-Phase A 概念级；动能/能源均为下限口径，非预算。",
      inputs: {
        cruise_speed_c: v,
        mass_kg: inputs.mass_kg,
        efficiency: inputs.efficiency,
        radius_m: inputs.radius_m,
        rotation_rpm: inputs.rotation_rpm,
        dust_mass_kg: inputs.dust_mass_kg
      },
      outputs: {
        travel_time_yr: { value: travelTimeYr(dist, v), unit: "yr", formula: "ICD-FML-001", fact_label: "derived_result", boundary: "匀速，不含加减速" },
        light_travel_time_yr: { value: lightTravelTimeYr(dist), unit: "yr", fact_label: "verified_fact" },
        kinetic_energy_classical_j: { value: ke, unit: "J", formula: "ICD-FML-002", fact_label: "derived_result", note: "下限，非完整能源预算" },
        kinetic_energy_relativistic_j: { value: keRel, unit: "J", formula: "(γ−1)mc² 对照", fact_label: "derived_result" },
        energy_input_lower_bound_j: { value: energyInputLowerBoundJ(ke, inputs.efficiency), unit: "J", fact_label: "derived_result", note: "η 为情景参数 [assumption]" },
        kinetic_energy_tnt_equivalent_kg: { value: tntEquivalentKg(ke), unit: "kg TNT", fact_label: "derived_result", note: "换算约定 [assumption]" },
        artificial_gravity_m_s2: { value: ag, unit: "m/s²", formula: "ICD-FML-003", fact_label: "derived_result" },
        artificial_gravity_g_multiple: { value: ag / g0, unit: "×g0", fact_label: "derived_result" },
        rpm_for_1g_at_radius: { value: rpmForGravity(inputs.radius_m, g0), unit: "rpm", fact_label: "derived_result" },
        dust_impact_energy_j: { value: dust, unit: "J", formula: "ICD-FML-004", fact_label: "derived_result" },
        dust_tnt_equivalent_kg: { value: tntEquivalentKg(dust), unit: "kg TNT", fact_label: "derived_result" },
        turn_limit_deg_dv_10_15pct: { value: [turnAngleDeg(0.10), turnAngleDeg(0.15)], unit: "deg", formula: "ICD-FML-005", fact_label: "derived_result" }
      }
    };
  }

  /* CCR-001 接口占位：结构预留，功能不实现（DISC-002）。 */
  function compareScenariosStub() {
    return {
      status: "interface_only_not_implemented",
      reason: "CCR-001 (0.01c/0.05c) pending_platform_confirmation；当前基线锁定 0.03c",
      would_accept: { cruise_speed_c_list: ["0.01", "0.03", "0.05"] }
    };
  }

  return {
    TRACE: TRACE, TNT_J_PER_KG: TNT_J_PER_KG,
    travelTimeYr: travelTimeYr, lightTravelTimeYr: lightTravelTimeYr,
    kineticEnergyClassicalJ: kineticEnergyClassicalJ,
    kineticEnergyRelativisticJ: kineticEnergyRelativisticJ,
    energyInputLowerBoundJ: energyInputLowerBoundJ,
    artificialGravityMS2: artificialGravityMS2, rpmForGravity: rpmForGravity,
    dustImpactEnergyJ: dustImpactEnergyJ, tntEquivalentKg: tntEquivalentKg,
    turnAngleDeg: turnAngleDeg, computeScenario: computeScenario,
    compareScenariosStub: compareScenariosStub
  };
}));
