#!/usr/bin/env node
// 应用有界机器检查 (Node, 自行结束, 无网络, 不起服务器)
// 用法: cd outputs/app && node check_app.js
// 追溯键: corona-baseline-1.0.0 / 1.0.0 / cfb12b78…8b10c2
"use strict";
const fs = require("fs");
const path = require("path");

global.window = {};
require("./params.js");
const P = global.window.CORONA_PARAMS;
const Core = require("./core.js");

let failures = 0;
function check(name, ok, detail) {
  console.log((ok ? "PASS" : "FAIL") + "  " + name + (detail ? "  " + detail : ""));
  if (!ok) failures++;
}
function relClose(a, b, tol) { return Math.abs(a - b) <= tol * Math.abs(b); }

// 1. 参数与情景锁定
check("params_traceability_keys",
  P.traceability.baseline_id === "corona-baseline-1.0.0" &&
  P.traceability.baseline_version === "1.0.0" &&
  P.traceability.content_digest === "cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2",
  "追溯键三元组匹配");
check("scenario_locked_single_0.03c",
  JSON.stringify(P.scenarios.cruise_speed_c) === "[0.03]",
  "cruise_speed_c=" + JSON.stringify(P.scenarios.cruise_speed_c));

// 2. 与模型输出一致性 (RQ-APP-006): 读取模型实际输出文件比对
const rv = JSON.parse(fs.readFileSync(
  path.join(__dirname, "..", "model", "output", "reference_values.json"), "utf8")).reference_values;
const c = P.constants.speed_of_light_m_s;
const dist = P.constants.proxima_distance_ly;
const v = P.scenarios.cruise_speed_c[0];
const cases = Object.fromEntries(P.reference_cases.map(r => [r.id, r]));

check("app_matches_model:travel_time_yr",
  relClose(Core.travelTimeYr(dist, v), rv.travel_time_yr, 1e-12),
  Core.travelTimeYr(dist, v) + " vs model " + rv.travel_time_yr);
check("app_matches_model:kinetic_energy_1mt_j",
  relClose(Core.kineticEnergyNonrelJ(cases.crewed_1mt.mass_kg, v, c), rv.kinetic_energy_1mt_j, 1e-12),
  "model " + rv.kinetic_energy_1mt_j);
check("app_matches_model:kinetic_energy_5mt_j",
  relClose(Core.kineticEnergyNonrelJ(cases.orbital_material_5mt.mass_kg, v, c), rv.kinetic_energy_5mt_j, 1e-12),
  "model " + rv.kinetic_energy_5mt_j);
check("app_matches_model:artificial_gravity_m_s2",
  relClose(Core.artificialGravityMs2(cases.rotating_habitat.radius_m, cases.rotating_habitat.rotation_rpm),
    rv.artificial_gravity_m_s2, 1e-12),
  "model " + rv.artificial_gravity_m_s2);
check("app_matches_model:dust_impact_energy_j",
  relClose(Core.dustImpactEnergyJ(cases.dust_1mg.mass_kg, cases.dust_1mg.relative_speed_c, c),
    rv.dust_impact_energy_j, 1e-12),
  "model " + rv.dust_impact_energy_j);

// 3. 情景导出结构 (等价于浏览器导出内容, RQ-APP-005 的导出 JSON 机器可验证部分)
const scenario = Core.computeScenario(P, {
  massKg: cases.crewed_1mt.mass_kg, efficiency: 0.3,
  radiusM: cases.rotating_habitat.radius_m, rotationRpm: cases.rotating_habitat.rotation_rpm
});
const payload = { schema: "corona-scenario-export/1.0", scenario };
const payloadStr = JSON.stringify(payload);
check("export_payload_has_traceability",
  scenario.traceability && scenario.traceability.content_digest === P.traceability.content_digest,
  "导出含追溯键");
check("export_payload_complete",
  ["travel_time_yr", "kinetic_energy_nonrel_j", "artificial_gravity_m_s2", "dust_impact_energy_j",
   "min_input_energy_j", "kinetic_energy_rel_j", "equivalent_tnt_mt", "dust_impact_tnt_kg",
   "artificial_gravity_g0"].every(k => scenario.outputs[k] && typeof scenario.outputs[k].value === "number"),
  "导出含全部输入/输出字段");
check("export_payload_fact_labels",
  Object.values(scenario.outputs).every(o =>
    ["verified_fact", "derived_result", "assumption", "unknown"].includes(o.fact_label)),
  "导出全部输出带合法事实标签");
fs.writeFileSync(path.join(__dirname, "sample_export_0.03c.json"),
  JSON.stringify(payload, null, 2) + "\n");
console.log("INFO  sample export written: sample_export_0.03c.json (" + payloadStr.length + " chars)");

// 4. 离线性: index.html 无任何外部 URL 资源
const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
const external = html.match(/(src|href)\s*=\s*"https?:\/\/[^"]*"/g);
check("app_no_cloud_dependency", !external,
  external ? "外部引用: " + external.join(",") : "无 http(s) 外部资源");

// 5. 图纸链接存在且文件存在
const links = [...html.matchAll(/href="(\.\.\/drawings\/[^"]+)"/g)].map(m => m[1]);
const missing = links.filter(l => !fs.existsSync(path.join(__dirname, l)));
check("app_drawing_links_resolve", links.length === 4 && missing.length === 0,
  "links=" + links.length + (missing.length ? " missing=" + missing.join(",") : " 全部存在"));

// 6. 禁用声明扫描
const forbidden = ["construction_ready", "manufacturing_ready", "flight_certified",
  "whole_program_cost_committed"];
const blob = html + fs.readFileSync(path.join(__dirname, "app.js"), "utf8") +
  fs.readFileSync(path.join(__dirname, "core.js"), "utf8") + payloadStr;
const found = forbidden.filter(w => blob.includes(w) &&
  // 允许作为"禁止声明"纪律文本出现 (前 120 字符内含否定语境)
  !/禁止|不得|不构成|非/.test(blob.slice(Math.max(0, blob.indexOf(w) - 120), blob.indexOf(w))));
check("app_no_forbidden_claims", found.length === 0,
  found.length ? "found=" + found.join(",") : "仅纪律性引用, 无违规声明");

console.log("\n[summary] " + (failures === 0 ? "ALL CHECKS PASSED" : failures + " CHECK(S) FAILED"));
process.exit(failures === 0 ? 0 : 1);
