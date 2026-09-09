/*
 * app.js —— 本地数字决策应用逻辑。
 * 职责：状态管理 + 渲染 + 情景切换 + 导出 JSON。
 * 与 model/corona_model.py 同源（全部数学来自 window.CoronaModel）。
 * 无网络依赖：全部资源本地化，不加载 github/cdn/字体等外部 http/https。
 */
"use strict";

(function () {
  const M = window.CoronaModel;
  const P = M.P;

  const state = {
    scenarioId: null   // 当前情景 ID，初始由默认情景确定
  };

  // ---- 显示格式化工具 ----
  function fmtSig(x, digits) {
    if (x === null || x === undefined || isNaN(x)) return "—";
    // 大数 / 小数用科学计数
    const a = Math.abs(x);
    if (a !== 0 && (a >= 1e6 || a < 1e-3)) {
      return x.toExponential(digits || 3);
    }
    return x.toLocaleString("en-US", { maximumFractionDigits: digits || 3, useGrouping: true });
  }

  function fmtYears(y) {
    return fmtSig(y, 3) + " yr";
  }

  function fmtDays(d) {
    if (d >= 1) return fmtSig(d, 1) + " 天";
    return fmtSig(d * 24, 1) + " 小时";
  }

  function fmtMonths(m) {
    return fmtSig(m, 1) + " 月";
  }

  function fmtG0(g) {
    return fmtSig(g, 4) + " m/s²（" + (g / M.G0).toFixed(3) + " g₀）";
  }

  function el(id) { return document.getElementById(id); }

  // ---- 计算当前情景的派生结果 ----
  function computeCurrent() {
    const c = M.SCENARIO_TO_C[state.scenarioId];
    return M.computeScenario(c);
  }

  // ---- 渲染 ----
  function render() {
    const r = computeCurrent();
    const c = r.cruise_speed_c;

    // 图注 / 速度轴
    el("scenario-id").textContent = state.scenarioId;
    el("cruise-speed-label").textContent = fmtSig(c * 100, 2) + "% c";
    el("v-kmps").textContent = fmtSig(r.v_kmps, 1) + " km/s";

    // 到各目标航时
    el("time-alpha").textContent = fmtYears(r.time_alpha_yrs);
    el("time-glens").textContent = fmtYears(r.time_glens_yrs) + " ≈ " + fmtDays(r.time_glens_days);
    el("time-precursor").textContent = fmtYears(r.time_precursor_yrs) + " ≈ " + fmtMonths(r.time_precursor_months);

    // 动能下限 / TNT 当量
    el("ke-1mt").textContent = fmtSig(r.ke_1mt_j, 4) + " J";
    el("ke-1mt-tnt").textContent = fmtSig(r.ke_1mt_tnt_tonnes, 3) + " 吨 TNT 当量";
    el("ke-5mt").textContent = fmtSig(r.ke_5mt_j, 4) + " J";
    el("ke-dust").textContent = fmtSig(r.ke_dust_1mg_j, 4) + " J";
    el("ke-dust-tnt").textContent = fmtSig(r.ke_dust_1mg_tnt_kg, 2) + " kg TNT 当量";

    // 相对论修正
    el("rel-corr").textContent = fmtSig(r.rel_correction_pct, 3) + " %";

    // 束动力比（相对 0.2c 锚点）
    el("p-rel").textContent = "× " + fmtSig(r.p_rel, 3);
    el("beam-power").textContent = fmtSig(r.beam_power_gw, 3) + " GW（相对 100 GW @ 0.2c）";

    // 人工重力（速度无关，保留项）
    const g = M.gravityRotatingHabitat(1000.0, 2.0);
    el("gravity").textContent = fmtG0(g);
    el("gravity-rpm").textContent = fmtSig(M.rpmForG0(1000.0), 3) + " rpm（1 g₀ @ r=1000 m）";

    // 激活态
    const btns = document.querySelectorAll(".scenario-btn");
    btns.forEach(function (b) {
      if (b.getAttribute("data-scenario") === state.scenarioId) {
        b.classList.add("active");
        b.setAttribute("aria-pressed", "true");
      } else {
        b.classList.remove("active");
        b.setAttribute("aria-pressed", "false");
      }
    });

    // 数据属性，供自动化断言
    document.body.setAttribute("data-current-scenario", state.scenarioId);
    document.body.setAttribute("data-current-c", String(c));
    document.body.setAttribute("data-time-alpha", String(r.time_alpha_yrs));
    document.body.setAttribute("data-v-kmps", String(r.v_kmps));
    document.body.setAttribute("data-ke-1mt", String(r.ke_1mt_j));
    document.body.setAttribute("data-ke-5mt", String(r.ke_5mt_j));
    document.body.setAttribute("data-ke-dust", String(r.ke_dust_1mg_j));
    document.body.setAttribute("data-p-rel", String(r.p_rel));
  }

  // ---- 情景切换 ----
  function setScenario(id) {
    if (!(id in M.SCENARIO_TO_C)) {
      throw new Error("未知情景: " + id);
    }
    state.scenarioId = id;
    render();
  }

  // ---- 导出当前情景 JSON ----
  function buildExportObject() {
    const r = computeCurrent();
    // 显式保留的未知项（缺直接证据，不填假设）
    const openUnknowns = {
      propulsion_efficiency: "unknown",
      dust_flux_at_velocity: "unknown",
      cost_reduction_evidence: "unknown",
      sail_material_at_scale: "unknown"
    };
    return {
      schema_version: 1,
      baseline_id: P.baseline_id,
      baseline_version: P.baseline_version,
      scenario: {
        id: state.scenarioId,
        cruise_speed_c: r.cruise_speed_c
      },
      routes: P.routes.slice(),
      derived: {
        v_kmps: r.v_kmps,
        time_alpha_yrs: r.time_alpha_yrs,
        time_glens_yrs: r.time_glens_yrs,
        time_precursor_yrs: r.time_precursor_yrs,
        ke_1mt_j: r.ke_1mt_j,
        ke_5mt_j: r.ke_5mt_j,
        ke_dust_1mg_j: r.ke_dust_1mg_j,
        ke_1mt_tnt_tonnes: r.ke_1mt_tnt_tonnes,
        rel_correction_pct: r.rel_correction_pct,
        p_rel: r.p_rel,
        beam_power_gw: r.beam_power_gw
      },
      gravity_habitat: {
        radius_m: 1000,
        rotation_rpm: 2,
        a_m_s2: M.gravityRotatingHabitat(1000.0, 2.0),
        rpm_for_1g0: M.rpmForG0(1000.0)
      },
      forbidden_claims: P.forbidden_claims.slice(),
      open_unknowns: openUnknowns,
      truth_discipline: {
        labels: P.truth_labels.slice(),
        note: "derived_result 为模型推导；0.2c/100GW/克级为 Starshot B 级锚点；缩放律为 assumption；open_unknowns 显式保留 unknown。"
      },
      export_meta: {
        generated_at: new Date().toISOString(),
        conceptual_only: true,
        level: "Level-0 / Pre-Phase A conceptual study"
      }
    };
  }

  function exportScenarioJSON() {
    return JSON.stringify(buildExportObject(), null, 2);
  }

  function exportScenario() {
    const payload = exportScenarioJSON();
    const blob = new Blob([payload], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "corona_" + state.scenarioId + "_scenario.json";
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
    return a.download;
  }

  // ---- 初始化 ----
  function init() {
    // 绑定情景按钮
    const btns = document.querySelectorAll(".scenario-btn");
    btns.forEach(function (b) {
      b.addEventListener("click", function () {
        setScenario(b.getAttribute("data-scenario"));
      });
    });
    // 绑定导出按钮
    el("btn-export").addEventListener("click", function (ev) {
      ev.preventDefault();
      const filename = exportScenario();
      el("export-status").textContent = "已导出 " + filename;
    });
    // 默认情景
    setScenario(P.scenarios.scenario_ids[P.scenarios.cruise_speed_c.indexOf(P.scenarios.default_scenario_c)]);
  }

  // 暴露测试/自动化 API（与按钮共用同一函数，真实逻辑）
  window.AppAPI = {
    getState: function () { return JSON.parse(JSON.stringify(state)); },
    setScenario: setScenario,
    computeCurrent: computeCurrent,
    buildExportObject: buildExportObject,
    exportScenarioJSON: exportScenarioJSON,
    exportScenario: exportScenario,
    render: render
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
