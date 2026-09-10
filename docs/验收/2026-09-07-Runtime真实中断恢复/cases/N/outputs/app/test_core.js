/* 日冕计划 · 应用内核有界一致性测试 (test_core.js)
 * 从 outputs/app 目录执行:  node test_core.js
 * 有界退出（纯本地计算，无服务/无网络），退出码 0 = 全部 PASS。
 * 校验: corona_core.js 计算结果与 outputs/model/results.json（Python 模型产物）
 * 在 acceptance.json 容差内一致 → 证明应用与统一模型同公式同参数（RQ-APP-006）。
 */
"use strict";
const fs = require("fs");
const path = require("path");
const C = require("./corona_core.js");

const params = JSON.parse(fs.readFileSync(path.join(__dirname, "params.json"), "utf8"));
const modelResults = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "model", "results.json"), "utf8"));

let failures = 0;
function check(name, actual, expected, relTol) {
  const rel = Math.abs(actual - expected) / Math.abs(expected);
  const ok = rel <= relTol;
  if (!ok) failures++;
  console.log(`${ok ? "PASS" : "FAIL"} ${name}: app=${actual} model=${expected} rel=${rel.toExponential(3)} tol=${relTol}`);
}

// 参考工况输入（来自 params.json reference_cases）
const cases = Object.fromEntries(params.reference_cases.map((c) => [c.id, c]));
const base = {
  mass_kg: cases.crewed_1mt.mass_kg,
  efficiency: 0.3,
  radius_m: cases.rotating_habitat.radius_m,
  rotation_rpm: cases.rotating_habitat.rotation_rpm,
  dust_mass_kg: cases.dust_1mg.mass_kg
};
const r = C.computeScenario(params, base);

check("travel_time_yr", r.outputs.travel_time_yr.value,
      modelResults.travel_time.travel_time_yr, 1e-9);
check("kinetic_energy_1mt_j", r.outputs.kinetic_energy_classical_j.value,
      modelResults.kinetic_energy.crewed_1mt_classical_j, 1e-9);
check("artificial_gravity_m_s2", r.outputs.artificial_gravity_m_s2.value,
      modelResults.artificial_gravity.centripetal_m_s2, 1e-9);
check("dust_impact_energy_j", r.outputs.dust_impact_energy_j.value,
      modelResults.dust_impact.energy_j, 1e-9);

// 5Mt 工况
const r5 = C.computeScenario(params, { ...base, mass_kg: cases.orbital_material_5mt.mass_kg });
check("kinetic_energy_5mt_j", r5.outputs.kinetic_energy_classical_j.value,
      modelResults.kinetic_energy.orbital_material_5mt_classical_j, 1e-9);

// acceptance.json 期望值（经 params.json.reference_results，源自模型实际运行）
const ref = params.reference_results;
check("ref travel_time_yr", r.outputs.travel_time_yr.value, ref.travel_time_yr, 1e-9);
check("ref kinetic_energy_1mt_j", r.outputs.kinetic_energy_classical_j.value, ref.kinetic_energy_1mt_j, 1e-9);

// 追溯键一致性
const t = r.traceability;
const mt = modelResults.traceability;
const traceOk = t.baseline_id === mt.baseline_id &&
  t.baseline_version === mt.baseline_version && t.content_digest === mt.content_digest;
console.log(`${traceOk ? "PASS" : "FAIL"} traceability triple matches model results`);
if (!traceOk) failures++;

// 情景锁定：尝试 0.05c 必须抛错（CCR-001 未纳入）
let lockOk = false;
try { C.computeScenario(params, { ...base, cruise_speed_c: 0.05 }); }
catch (e) { lockOk = /locked at 0\.03/.test(e.message); }
console.log(`${lockOk ? "PASS" : "FAIL"} scenario lock rejects 0.05c (CCR-001 not in baseline)`);
if (!lockOk) failures++;

// 三情景比较接口仅占位
const stub = C.compareScenariosStub();
const stubOk = stub.status === "interface_only_not_implemented";
console.log(`${stubOk ? "PASS" : "FAIL"} compareScenariosStub is interface-only`);
if (!stubOk) failures++;

// 导出 JSON 结构（应用“导出当前情景 JSON”的同一对象形状）
const exportOk = r.inputs && r.outputs && r.traceability && r.scope_note;
console.log(`${exportOk ? "PASS" : "FAIL"} export JSON contains traceability+inputs+outputs`);
if (!exportOk) failures++;

console.log(failures === 0 ? "ALL APP CORE TESTS PASS" : `${failures} FAILURES`);
process.exit(failures === 0 ? 0 : 1);
