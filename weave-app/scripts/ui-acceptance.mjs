#!/usr/bin/env node
/*
 * Weave 页面验收器（ui-acceptance）。
 *
 * 用法：
 *   node scripts/ui-acceptance.mjs /projects [/activity ...]
 *   WEAVE_APP_URL=http://127.0.0.1:5173 （默认）
 *   WEAVE_API_URL=http://127.0.0.1:8081  （取 token 用，默认同源）
 *   WEAVE_ACCEPT_TOKEN=<token>           （可选，跳过自动取 token）
 *   WEAVE_ACCEPT_USER_ID=<uuid>          （自动取 token 时的用户）
 *   WEAVE_ACCEPT_BROWSER=<chrome 可执行路径>（可选，默认尝试 channel: chrome）
 *
 * 每个路由在 390/768/1280/1440 × light/dark 下检查：
 *   1. 无 pageerror / console error
 *   2. 无横向溢出
 *   3. 交互元素有可访问名称
 *   4. 表单控件有关联 label
 *   5. 文本与实色按钮对比度 ≥ 4.5:1（计算样式实测）
 *   6. 390px 下 main 内按钮/链接触控目标 ≥ 44px
 * 截图存 weave-app/.acceptance/shots/（已 gitignore）。任一检查失败以非零退出。
 */
import { chromium } from "playwright-core";
import { mkdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

const APP_URL = (process.env.WEAVE_APP_URL || "http://127.0.0.1:5173").replace(/\/+$/, "");
const API_URL = (process.env.WEAVE_API_URL || APP_URL).replace(/\/+$/, "");
const ROOT = fileURLToPath(new URL("..", import.meta.url));
const SHOTS = `${ROOT}.acceptance/shots`;
mkdirSync(SHOTS, { recursive: true });

const VIEWPORTS = [
  { name: "390", width: 390, height: 844 },
  { name: "768", width: 768, height: 1024 },
  { name: "1280", width: 1280, height: 800 },
  { name: "1440", width: 1440, height: 900 },
];

const routes = process.argv.slice(2);
if (!routes.length) {
  console.error("用法: node scripts/ui-acceptance.mjs <route> [更多路由]");
  process.exit(2);
}

async function getToken() {
  if (process.env.WEAVE_ACCEPT_TOKEN) return process.env.WEAVE_ACCEPT_TOKEN.trim();
  const res = await fetch(`${API_URL}/v1/auth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(process.env.WEAVE_ACCEPT_USER_ID ? { user_id: process.env.WEAVE_ACCEPT_USER_ID } : {}),
  });
  if (!res.ok) throw new Error(`取 token 失败: ${res.status}`);
  return (await res.json()).token;
}

const CHECKS_JS = String.raw`(() => {
  const failures = [];
  // 2. 横向溢出
  const overflow = document.documentElement.scrollWidth - document.documentElement.clientWidth;
  if (overflow > 1) failures.push("横向溢出 " + overflow + "px");

  // 3. 交互元素可访问名称
  const interactive = [...document.querySelectorAll('button, a[href], summary, [role="button"], input, select, textarea')];
  for (const el of interactive) {
    if (el.closest('[aria-hidden="true"]') || el.offsetParent === null) continue;
    const name = (el.getAttribute('aria-label') || el.textContent || el.getAttribute('title') || el.getAttribute('placeholder') || el.value || '').trim();
    const labelled = el.getAttribute('aria-labelledby');
    if (!name && !labelled) {
      const desc = el.tagName.toLowerCase() + '.' + String(el.className).split(' ')[0];
      failures.push('交互元素缺可访问名称: ' + desc + ' @ ' + (el.getBoundingClientRect().left|0) + ',' + (el.getBoundingClientRect().top|0));
    }
  }

  // 4. 表单控件 label 关联
  for (const el of document.querySelectorAll('input:not([type=checkbox]):not([type=radio]):not([type=hidden]):not([type=search]), select, textarea')) {
    if (el.offsetParent === null) continue;
    const id = el.id;
    const ok = (id && document.querySelector('label[for="' + id + '"]')) || el.closest('label') || el.getAttribute('aria-label') || el.getAttribute('aria-labelledby');
    if (!ok) failures.push('表单控件缺 label 关联: ' + el.tagName.toLowerCase() + '#' + (id || '(无id)'));
  }

  // 5. 对比度（文本与实色按钮）
  function parseRGB(s) { const m = s.match(/[\d.]+/g); return m ? m.slice(0, 3).map(Number) : null; }
  function lum([r, g, b]) {
    const f = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
  }
  function bgOf(el) {
    let node = el;
    while (node && node !== document.documentElement) {
      const bg = parseRGB(getComputedStyle(node).backgroundColor);
      const alpha = parseFloat((getComputedStyle(node).backgroundColor.match(/[\d.]+\)$/) || ['1'])[0].replace(')', ''));
      if (bg && alpha > 0.85) return bg;
      node = node.parentElement;
    }
    return parseRGB(getComputedStyle(document.body).backgroundColor) || [255, 255, 255];
  }
  const samples = [...document.querySelectorAll('main h1, main h2, main p, main span, main td, main th, main a, main button, main label, main small, main strong')].slice(0, 400);
  for (const el of samples) {
    if (el.offsetParent === null || !el.textContent.trim()) continue;
    const cs = getComputedStyle(el);
    const fg = parseRGB(cs.color);
    if (!fg) continue;
    const fgA = cs.color.includes('rgba') ? parseFloat(cs.color.match(/[\d.]+\)$/)[0]) : 1;
    if (fgA < 0.5) continue;
    const bg = bgOf(el);
    const l1 = lum(fg), l2 = lum(bg);
    const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
    if (ratio < 4.5) {
      failures.push('对比度 ' + ratio.toFixed(2) + ':1 < 4.5: "' + el.textContent.trim().slice(0, 20) + '" (' + String(el.className).split(' ')[0] + ')');
    }
  }

  // 6. 390px 触控目标（仅窄屏执行）
  if (window.innerWidth <= 480) {
    for (const el of document.querySelectorAll('main button, main a, main summary')) {
      if (el.offsetParent === null) continue;
      const h = el.getBoundingClientRect().height;
      if (h > 0 && h < 43.5) failures.push('触控目标 ' + Math.round(h) + 'px < 44px: "' + el.textContent.trim().slice(0, 16) + '"');
    }
  }
  return failures;
})()`;

const token = await getToken();
const launchOpts = process.env.WEAVE_ACCEPT_BROWSER
  ? { executablePath: process.env.WEAVE_ACCEPT_BROWSER }
  : { channel: "chrome" };
const browser = await chromium.launch(launchOpts);

let totalFailures = 0;
for (const route of routes) {
  for (const scheme of ["light", "dark"]) {
    for (const v of VIEWPORTS) {
      const context = await browser.newContext({ viewport: { width: v.width, height: v.height }, colorScheme: scheme });
      await context.addInitScript((t) => { try { sessionStorage.setItem("weave.session.token", t); } catch {} }, token);
      const page = await context.newPage();
      const pageErrors = [];
      page.on("pageerror", (e) => pageErrors.push(String(e).slice(0, 120)));
      page.on("console", (m) => { if (m.type() === "error") pageErrors.push("console.error: " + m.text().slice(0, 120)); });
      await page.goto(`${APP_URL}${route}`, { waitUntil: "load", timeout: 30000 });
      await page.waitForTimeout(1500);
      const failures = await page.evaluate(CHECKS_JS);
      for (const e of pageErrors) failures.push(e);
      const tag = `${route} ${scheme}@${v.name}`;
      if (failures.length) {
        totalFailures += failures.length;
        console.log(`FAIL ${tag}`);
        for (const f of failures.slice(0, 12)) console.log(`  - ${f}`);
        if (failures.length > 12) console.log(`  …另 ${failures.length - 12} 条`);
      } else {
        console.log(`PASS ${tag}`);
      }
      const safe = route.replace(/[^a-z0-9]+/gi, "_");
      await page.screenshot({ path: `${SHOTS}/${safe}_${scheme}_${v.name}.png` });
      await context.close();
    }
  }
}
await browser.close();
console.log(totalFailures ? `\n验收失败：${totalFailures} 条` : "\n验收全部通过");
process.exit(totalFailures ? 1 : 0);
