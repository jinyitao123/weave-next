/*
 * gui_verify.js —— 真实浏览器（无头 Chrome）GUI 交互验证器。
 * 验证：应用加载 → 默认情景 → 三情景切换 → 派生结果更新 → 导出 JSON（真实下载）→ 无外部 http/https 资源。
 *
 * 通过 CDP 驱动真实 Chrome；不使用 Node DOM-shim。
 * 输出：JSON（含 per-check 结果），并在全部通过时以 process.exit(0) 退出，否则 exit(1)。
 * 用法：node gui_verify.js <app_dir> [chrome_path]
 */

const { spawn } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const APP_DIR = path.resolve(process.argv[2] || "outputs/app");
const CHROME = process.argv[3] || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const PORT = 9229;
const DL = fs.mkdtempSync(path.join(os.tmpdir(), "corona-dl-"));
const PROFILE = fs.mkdtempSync(path.join(os.tmpdir(), "corona-cdp-"));

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function fail(msg) { return { ok: false, detail: msg }; }
function pass(msg) { return { ok: true, detail: msg }; }

async function main() {
  const report = { app_dir: APP_DIR, chrome: CHROME, checks: [], scenario_switch: {}, export: {}, network: {} };
  const chrome = spawn(CHROME, [
    "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
    "--remote-debugging-port=" + PORT, "--user-data-dir=" + PROFILE,
    "--no-first-run", "--no-default-browser-check", "about:blank"
  ], { stdio: "ignore" });

  let ready = false;
  for (let i = 0; i < 40; i++) {
    try {
      const v = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
      if (v.webSocketDebuggerUrl) { ready = true; break; }
    } catch (e) { /* retry */ }
    await sleep(400);
  }
  if (!ready) {
    try { chrome.kill(); } catch (e) {}
    report.checks.push({ name: "chrome_launch", ok: false, detail: "CDP 未就绪: " + CHROME });
    console.log(JSON.stringify(report));
    process.exit(2);
  }

  const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const page = targets.find((t) => t.type === "page");
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pending = {};
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending[m.id]) { pending[m.id](m); delete pending[m.id]; }
  };
  await new Promise((res) => { ws.onopen = res; });

  const send = (method, params = {}) =>
    new Promise((res) => { const i = ++id; pending[i] = res; ws.send(JSON.stringify({ id: i, method, params })); });

  const requests = [];
  ws.addEventListener("message", (ev) => {
    const m = JSON.parse(ev.data);
    if (m.method === "Network.requestWillBeSent") requests.push(m.params.request.url);
  });

  await send("Page.enable");
  await send("Runtime.enable");
  await send("Network.enable");
  await send("Page.navigate", { url: "file://" + APP_DIR + "/index.html" });
  await sleep(1600);

  // 1) app 加载
  const appLoaded = await send("Runtime.evaluate", { expression: "typeof window.AppAPI === 'object' && typeof window.CoronaModel === 'object'", returnByValue: true });
  report.checks.push({ name: "app_loads_local", ok: !!appLoaded.result.result.value, detail: "AppAPI/CoronaModel 存在" });

  const evalJson = async (expr) => {
    const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true });
    return r.result.result.value;
  };

  // 2) 默认情景应是 0.03c
  const initId = await evalJson("document.body.getAttribute('data-current-scenario')");
  report.checks.push({ name: "default_scenario_0_03c", ok: initId === "S-0.03c", detail: "initial=" + initId });

  // 3) 逐个点击情景按钮并校验派生量
  const expect = {
    "S-0.01c": { c: "0.01", ta: 425.0, ke1mt: 4.493776e21, v: 2997.9 },
    "S-0.03c": { c: "0.03", ta: 141.666666667, ke1mt: 4.0443983e22, v: 8993.8 },
    "S-0.05c": { c: "0.05", ta: 85.0, ke1mt: 1.123444e23, v: 14989.6 }
  };
  for (const sid of Object.keys(expect)) {
    await evalJson(`document.querySelector('[data-scenario="${sid}"]').click()`);
    await sleep(250);
    const got = JSON.parse(await evalJson(
      `JSON.stringify({id:document.body.getAttribute('data-current-scenario'),` +
      `c:document.body.getAttribute('data-current-c'),` +
      `ta:parseFloat(document.body.getAttribute('data-time-alpha')),` +
      `ke:parseFloat(document.body.getAttribute('data-ke-1mt')),` +
      `v:parseFloat(document.body.getAttribute('data-v-kmps'))})`
    ));
    const e = expect[sid];
    const ok = got.id === sid &&
      Math.abs(got.ta - e.ta) / e.ta < 1e-3 &&
      Math.abs(got.ke - e.ke1mt) / e.ke1mt < 1e-3 &&
      Math.abs(got.v - e.v) / e.v < 1e-3;
    report.scenario_switch[sid] = got;
    report.checks.push({ name: "switch_" + sid, ok: ok, detail: JSON.stringify(got) });
  }

  // 4) 导出 JSON（真实下载）——并核对「导出内容 == 页面当前情景」
  await send("Page.setDownloadBehavior", { behavior: "allow", downloadPath: DL });
  // 记录导出前的页面当前情景与派生值
  const pageCurrent = JSON.parse(await evalJson(
    `JSON.stringify({id:document.body.getAttribute('data-current-scenario'),` +
    `ta:parseFloat(document.body.getAttribute('data-time-alpha')),` +
    `v:parseFloat(document.body.getAttribute('data-v-kmps')),` +
    `ke1mt:parseFloat(document.body.getAttribute('data-ke-1mt'))})`
  ));
  await evalJson("document.getElementById('btn-export').click()");
  await sleep(900);
  const files = fs.readdirSync(DL);
  const dlName = files.find((f) => f.endsWith(".json"));
  let exportOk = false, exportDetail = "未找到下载文件";
  if (dlName) {
    try {
      const dj = JSON.parse(fs.readFileSync(path.join(DL, dlName), "utf8"));
      const hasFields = dj.scenario && Array.isArray(dj.routes) &&
        Array.isArray(dj.forbidden_claims) && dj.open_unknowns && dj.truth_discipline;
      // 「与页面一致」：导出情景 ID == 当前页情景，且派生数值与页面渲染值一致
      const rel = (a, b) => Math.abs(a - b) / Math.abs(b || 1);
      const consistent =
        dj.scenario.id === pageCurrent.id &&
        rel(dj.derived.time_alpha_yrs, pageCurrent.ta) < 1e-6 &&
        rel(dj.derived.v_kmps, pageCurrent.v) < 1e-6 &&
        rel(dj.derived.ke_1mt_j, pageCurrent.ke1mt) < 1e-6;
      exportOk = hasFields && consistent &&
        dlName.includes(dj.scenario.id) && dj.baseline_id === "corona-prephase-a-v1";
      exportDetail = JSON.stringify({ filename: dlName, scenario_id: dj.scenario.id, baseline_id: dj.baseline_id, routes: dj.routes.length, unknown_keys: Object.keys(dj.open_unknowns), truth_labels: dj.truth_discipline.labels, consistent_with_page: consistent, page_id: pageCurrent.id });
      report.export = { filename: dlName, baseline_id: dj.baseline_id, scenario_id: dj.scenario.id, consistent_with_page: consistent };
    } catch (e) {
      exportDetail = "JSON 解析失败: " + e.message;
    }
  }
  report.checks.push({ name: "export_scenario_json", ok: exportOk, detail: exportDetail });

  // 5) 无外部 http/https 资源
  const externals = requests.filter((u) => /^https?:/i.test(u));
  report.checks.push({ name: "no_cloud_dependency", ok: externals.length === 0, detail: "requests=" + requests.length + " external=" + externals.length + (externals.length ? " urls=" + externals.slice(0, 5).join(",") : "") });
  report.network = { total: requests.length, external: externals };
  report.checks.push({ name: "gui_harness", ok: true, detail: "CDP 驱动真实 Chrome 完成" });

  ws.close();
  try { chrome.kill(); } catch (e) {}
  report.all_ok = report.checks.every((c) => c.ok);
  console.log(JSON.stringify(report));
  process.exit(report.all_ok ? 0 : 1);
}

main().catch((e) => { console.error("ERR", e); process.exit(3); });
