/*
 * digital_engineering_selfcheck.js
 * 数字工程节点（digital-engineering）· 应用侧自检（app/）
 *
 * 检查内容（对应验收门槛 G2/G3/G4/G5 的应用侧逻辑）：
 *   G2 三速度情景存在（0.01c / 0.03c / 0.05c）
 *   G3 参考计算在容差内（5 个 reference_check 锚点）
 *   G4 应用无云/外网依赖（无 http/https、无 fetch/XHR）
 *   G5 应用导出情景 JSON（真实 buildExportObject 路径）
 *
 * 约束：
 *   - 所有计数在运行时由实际对象动态得出（不得硬编码计数）。
 *   - 有界退出：正常输出后 exit 0；任一断言失败 exit 1。
 *   - 仅读取本节点 outputs/ 下交付的 app/，不修改任何文件。
 *
 * 运行：node outputs/verification/digital_engineering_selfcheck.js
 */
"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const APP_DIR = path.join(__dirname, "..", "app");
const ok = [];
const bad = [];

function assert(cond, msg) {
  if (cond) { ok.push(msg); } else { bad.push(msg); }
}

// ---- 最小 DOM 桩，使 app.js 可加载并暴露 AppAPI ----
function stubEl() {
  return {
    textContent: "",
    setAttribute: () => {},
    getAttribute: () => "S-0.03c",
    style: {},
    classList: { add: () => {}, remove: () => {} },
    addEventListener: () => {},
    appendChild: () => {},
    removeChild: () => {},
    click: () => {}
  };
}
const bodyStub = {
  setAttribute: () => {}
};
global.window = global;
global.document = {
  readyState: "complete",
  getElementById: () => stubEl(),
  querySelectorAll: () => [],
  querySelector: () => stubEl(),
  body: bodyStub,
  createElement: () => stubEl()
};
global.URL = { createObjectURL: () => "blob:stub", revokeObjectURL: () => {} };

// ---- 载入 params.js + model.js + app.js（与浏览器加载顺序一致）----
for (const f of ["params.js", "model.js", "app.js"]) {
  const code = fs.readFileSync(path.join(APP_DIR, f), "utf8");
  vm.runInThisContext(code, { filename: f });
}

const M = global.CoronaModel;
assert(!!M, "model.js 暴露 window.CoronaModel");
const P = M.P;

// ---- 运行时常量（动态计数）----
const speeds = M.CRUISE_SPEEDS_C.slice();          // [0.01,0.03,0.05]
const scenarioIds = M.SCENARIO_IDS.slice();
assert(speeds.length === 3, "三速度情景数量（运行时动态）: " + speeds.length);
assert(
  JSON.stringify(speeds) === JSON.stringify([0.01, 0.03, 0.05]),
  "速度值（G2）: " + JSON.stringify(speeds)
);
assert(
  JSON.stringify(scenarioIds) === JSON.stringify(["S-0.01c", "S-0.03c", "S-0.05c"]),
  "情景 ID（G2）: " + JSON.stringify(scenarioIds)
);

// ---- 三情景完整计算 ----
const all = M.computeAll();
const allKeys = Object.keys(all);
assert(allKeys.length === speeds.length, "computeAll 覆盖情景数（动态）: " + allKeys.length);
for (const id of scenarioIds) {
  const c = M.SCENARIO_TO_C[id];
  const r = M.computeScenario(c);
  assert(r.cruise_speed_c === c, "computeScenario(" + id + ") 巡航速度一致");
  assert(isFinite(r.ke_1mt_j) && r.ke_1mt_j > 0, id + " 1mt 动能有限且>0");
  assert(isFinite(r.time_alpha_yrs) && r.time_alpha_yrs > 0, id + " 到比邻星航时有限");
}

// ---- G3 参考计算（动态遍历 acceptance_reference_checks）----
const ref = M.referenceCheck();
const refRows = ref.rows;
assert(refRows.length === Object.keys(P.acceptance_reference_checks).length,
  "reference_check 行列数（运行时）: " + refRows.length);
let refAllOk = ref.all_ok;
for (const row of refRows) {
  const within = row.rel_err <= row.tol;
  if (!within) refAllOk = false;
  assert(within,
    "参考锚点 " + row.name + " 计算=" + row.computed.toPrecision(6) +
    " 期望=" + row.expected + " 相对误差=" + row.rel_err.toExponential(3) +
    " 容差=" + row.tol + (within ? " PASS" : " FAIL"));
}
assert(refAllOk, "G3 全部参考锚点均在容差内");

// ---- G4 无云依赖（对交付 app/ 静态扫描）----
let cloudRefs = [];
for (const f of fs.readdirSync(APP_DIR).filter(x => /\.(js|html)$/.test(x))) {
  const text = fs.readFileSync(path.join(APP_DIR, f), "utf8");
  const m = text.match(/https?:\/\//g);
  if (m) cloudRefs.push(f + ":" + JSON.stringify(m));
  if (/XMLHttpRequest|\bfetch\s*\(/.test(text)) cloudRefs.push(f + ":fetch/XHR");
}
assert(cloudRefs.length === 0, "G4 无外网/云依赖（动态扫描交付 app/）: " +
  (cloudRefs.length === 0 ? "none" : cloudRefs.join(", ")));

// ---- G5 真实导出路径（AppAPI.buildExportObject -> JSON.stringify）----
const api = global.AppAPI;
assert(!!api, "app.js 暴露 window.AppAPI");
const exportObj = api.buildExportObject();
const json = api.exportScenarioJSON();
const parsed = JSON.parse(json);
assert(parsed.schema_version === 1, "G5 导出 JSON schema_version=1");
assert(parsed.scenario && parsed.scenario.id, "G5 导出含 scenario.id: " + parsed.scenario.id);
assert(parsed.derived && isFinite(parsed.derived.ke_1mt_j), "G5 导出含 derived.ke_1mt_j");
assert(exportObj.forbidden_claims.length === P.forbidden_claims.length,
  "G5 禁用语条数（运行时）: " + exportObj.forbidden_claims.length);
assert(Array.isArray(exportObj.truth_discipline.labels), "G5 含真值标签集合（运行时）");

// ---- FD-01 相关内容：drawn 值 vs 运行时导出值的一致性检测（供 G10 参考）----
// 运行时 1mt 动能（0.01c/0.03c/0.05c）与 FD-01 面板 B 文本量级对照
const fd01_claimed = { "0.01c": 4.49e21, "0.03c": 4.04e22, "0.05c": 1.12e23 };
for (const key of Object.keys(fd01_claimed)) {
  const c = parseFloat(key);
  const run = M.ke1mtJ(c);
  const claim = fd01_claimed[key];
  const rel = Math.abs(run - claim) / claim;
  assert(rel < 0.02, "FD-01 面板 B '" + key + "' 1mt 动能 运行时=" +
    run.toExponential(3) + " 标注=" + claim + " 相对差=" + rel.toExponential(3));
}

// ---- 汇总（动态计数）----
console.log("=== digital-engineering 应用侧自检（app/）===");
console.log("检查数: " + (ok.length + bad.length) + "（动态）  通过: " + ok.length + "  失败: " + bad.length);
for (const line of ok) console.log("  [PASS] " + line);
for (const line of bad) console.log("  [FAIL] " + line);
if (bad.length > 0) {
  console.error("RESULT: FAIL (" + bad.length + " 项未通过)");
  process.exit(1);
}
console.log("RESULT: PASS（全部 " + ok.length + " 项应用侧检查通过）");
process.exit(0);
