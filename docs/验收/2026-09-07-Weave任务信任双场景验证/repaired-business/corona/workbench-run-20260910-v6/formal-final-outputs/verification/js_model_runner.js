/*
 * js_model_runner.js —— 输出 app/model.js 的派生结果，用于与 model/corona_model.py 交叉比对。
 * 输出 JSON，全程不含 DOM；stdout 为单一 JSON 对象。
 * 用法：node js_model_runner.js <app_dir>
 */
"use strict";

const path = require("path");
const APP_DIR = path.resolve(process.argv[2] || "outputs/app");
global.window = {};
require(path.join(APP_DIR, "params.js"));
require(path.join(APP_DIR, "model.js"));

const M = window.CoronaModel;
const result = {
  baseline_id: M.P.baseline_id,
  cruise_speed_c: M.CRUISE_SPEEDS_C,
  scenario_ids: M.SCENARIO_IDS,
  all: M.computeAll(),
  reference: M.referenceCheck()
};
process.stdout.write(JSON.stringify(result, null, 2));
process.exit(0);
