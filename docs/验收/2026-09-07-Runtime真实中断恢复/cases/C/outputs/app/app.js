// 日冕计划 · 应用 UI 逻辑 (离线; 计算全部委托 core.js = 统一模型同公式)
// 追溯键: corona-baseline-1.0.0 / 1.0.0 / cfb12b78…8b10c2
(function () {
  "use strict";
  var P = window.CORONA_PARAMS;
  var Core = window.CoronaCore;

  // 三路线证据状态 (来源: 纪要/基线; 每条带事实标签)
  var ROUTES = [
    { id: "laser_sail_precursor", cls: "r1", name: "激光光帆先锋探测器",
      evidence: "公开概念研究存在 (如 Starshot 类), 工程验证有限",
      evidenceLabel: "verified_fact",
      conditions: "光帆材料、地面激光阵、星际通信链路成立; 0.2c 工况为路线内部参数 (ICD-RTE-003)",
      termination: "链路预算或材料指标在课题包内无法闭合即终止扩大投入",
      unknowns: "光帆材料性能、激光阵功率与造价、回传链路余量" },
    { id: "uncrewed_civilization_archive", cls: "r2", name: "无人文明档案载荷",
      evidence: "存储介质 (石英/DNA/种子/微生物) 可分别研究; 运输方案另行论证",
      evidenceLabel: "verified_fact",
      conditions: "介质长期稳定性 + 校验/解码说明/读取设备再制造方案成立",
      termination: "任何单一介质被证明不可恢复且冗余方案不成立即降级目标",
      unknowns: "载荷质量规模、运输方案、百年级介质稳定性、行星保护规则" },
    { id: "crewed_interstellar_vehicle", cls: "r3", name: "载人星际飞行器",
      evidence: "依赖推进/封闭生态/辐射防护/在轨工业/长期社会治理等未成熟能力, 仅远期研究对象",
      evidenceLabel: "verified_fact",
      conditions: "推进比较研究、分级生态验证、可维修可再制造体系全部通过阶段门 (6/12/18–24 月)",
      termination: "第十二个月架构比较未给出成立条件闭合路径即停止扩大投入",
      unknowns: "推进效率、封闭生态百年运行、冬眠 (机会技术, 不作基线)、成本数量级" }
  ];

  var $ = function (id) { return document.getElementById(id); };

  function fmtSI(x, unit) {
    if (x === 0) return "0 " + unit;
    var exp = Math.floor(Math.log10(Math.abs(x)));
    if (exp >= 4 || exp <= -3) return x.toExponential(4) + " " + unit;
    return (Math.round(x * 10000) / 10000) + " " + unit;
  }

  function currentInputs() {
    // 质量对数滑杆: 1e0 .. 1e10 kg
    var frac = parseFloat($("in-mass").value) / 100;
    var massKg = Math.pow(10, frac * 10);
    return {
      massKg: massKg,
      efficiency: parseFloat($("in-eta").value) / 100,
      radiusM: parseFloat($("in-radius").value),
      rotationRpm: parseFloat($("in-rpm").value)
    };
  }

  var lastScenario = null;

  function update() {
    var inputs = currentInputs();
    $("v-mass").textContent = fmtSI(inputs.massKg, "kg");
    $("v-eta").textContent = inputs.efficiency.toFixed(2);
    $("v-radius").textContent = inputs.radiusM.toFixed(0) + " m";
    $("v-rpm").textContent = inputs.rotationRpm.toFixed(2) + " rpm";

    var s = Core.computeScenario(P, inputs);
    lastScenario = s;
    var o = s.outputs;
    $("o-travel").textContent = o.travel_time_yr.value.toFixed(4) + " yr (光行时 4.25 yr)";
    $("o-ke").textContent = fmtSI(o.kinetic_energy_nonrel_j.value, "J");
    $("o-kerel").textContent = fmtSI(o.kinetic_energy_rel_j.value, "J");
    $("o-tnt").textContent = o.equivalent_tnt_mt.value.toExponential(3) + " Gt TNT";
    $("o-input").textContent = fmtSI(o.min_input_energy_j.value, "J");
    $("o-grav").textContent = o.artificial_gravity_m_s2.value.toFixed(4) + " m/s² (" +
      o.artificial_gravity_g0.value.toFixed(3) + " g0)";
    $("o-dust").textContent = fmtSI(o.dust_impact_energy_j.value, "J") +
      " ≈ " + o.dust_impact_tnt_kg.value.toFixed(2) + " kg TNT";
  }

  function renderRoutes() {
    var html = "";
    ROUTES.forEach(function (r) {
      var lab = { verified_fact: "vf", derived_result: "dr", assumption: "as", unknown: "uk" };
      html += '<div class="route ' + r.cls + '"><b>' + r.name + "</b> <code>" + r.id + "</code><br>" +
        "证据状态 <span class=\"tag " + lab[r.evidenceLabel] + '">' + r.evidenceLabel + "</span>: " + r.evidence + "<br>" +
        "成立条件: " + r.conditions + "<br>" +
        "终止条件: " + r.termination + "<br>" +
        '主要未知项 <span class="tag uk">unknown</span>: ' + r.unknowns + "</div>";
    });
    $("routes").innerHTML = html;
  }

  function exportJSON() {
    if (!lastScenario) update();
    var payload = {
      schema: "corona-scenario-export/1.0",
      exported_by: "corona-prephase-a decision app (offline)",
      concept_level: "Level-0 / Pre-Phase A — 概念级, 非工程能源预算",
      scope_note: "单一 0.03c 批准情景; 三速度为 CCR-001 待确认变更, 未实现 (DISC-002)",
      scenario: lastScenario
    };
    var blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
    var a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "corona_scenario_0.03c.json";
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(a.href); }, 1000);
    $("export-status").textContent = "已导出 corona_scenario_0.03c.json (含追溯键 " +
      lastScenario.traceability.baseline_id + " / " + lastScenario.traceability.baseline_version + ")";
  }

  ["in-mass", "in-eta", "in-radius", "in-rpm"].forEach(function (id) {
    $(id).addEventListener("input", update);
  });
  $("btn-preset-1mt").addEventListener("click", function () {
    $("in-mass").value = 90; update(); // 10^9 kg
  });
  $("btn-preset-5mt").addEventListener("click", function () {
    $("in-mass").value = (Math.log10(5e9) * 10).toFixed(2); update();
  });
  $("btn-preset-1g").addEventListener("click", function () {
    var r = parseFloat($("in-radius").value);
    var rpm = Core.rpmForTargetGravity(r, P.constants.standard_gravity_m_s2);
    $("in-rpm").value = Math.min(4, Math.max(0.1, rpm)).toFixed(3); update();
  });
  $("btn-export").addEventListener("click", exportJSON);

  renderRoutes();
  update();
})();
