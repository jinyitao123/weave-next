#!/usr/bin/env node
import assert from "node:assert/strict";
import { chromium } from "playwright-core";

const APP_URL = (process.env.WEAVE_APP_URL || "http://127.0.0.1:5174").replace(/\/+$/, "");
const token = process.env.WEAVE_ACCEPT_TOKEN?.trim();
const projectId = process.env.WEAVE_ACCEPT_PROJECT_ID?.trim();
const conversationId = process.env.WEAVE_ACCEPT_CONVERSATION_ID?.trim();

if (!token || !projectId || !conversationId) {
  console.error("缺少 WEAVE_ACCEPT_TOKEN、WEAVE_ACCEPT_PROJECT_ID 或 WEAVE_ACCEPT_CONVERSATION_ID");
  process.exit(2);
}

const route = `/conversation?project=${encodeURIComponent(projectId)}&conversation=${encodeURIComponent(conversationId)}`;
const launchOptions = process.env.WEAVE_ACCEPT_BROWSER
  ? { executablePath: process.env.WEAVE_ACCEPT_BROWSER }
  : { channel: "chrome" };
const browser = await chromium.launch(launchOptions);

try {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addInitScript((authToken) => {
    sessionStorage.setItem("weave.session.token", authToken);
  }, token);
  const page = await context.newPage();
  await page.goto(`${APP_URL}${route}`, { waitUntil: "load", timeout: 30_000 });

  const trigger = page.getByRole("button", { name: "查看最终交付物" });
  const panel = page.getByRole("complementary", { name: "最终交付物" });
  const resizer = page.getByRole("separator", { name: "调整最终交付物侧栏宽度" });

  await trigger.waitFor({ state: "visible" });
  await trigger.click();
  assert.equal(await page.locator("dialog.modal[open]").count(), 0, "交付物不应继续使用 modal");
  assert.equal(await panel.count(), 1, "应显示最终交付物侧栏");
  assert.equal(await trigger.getAttribute("aria-expanded"), "true");

  const initialBox = await panel.boundingBox();
  assert.ok(initialBox, "侧栏必须可见");
  assert.ok(Math.abs(initialBox.width - 440) <= 1, `首次宽度应为 440px，实际 ${initialBox.width}px`);

  const handleBox = await resizer.boundingBox();
  assert.ok(handleBox, "桌面端调整手柄必须可见");
  const restingColor = await resizer.evaluate((element) => getComputedStyle(element, "::after").backgroundColor);
  assert.notEqual(restingColor, "transparent", "静止态分隔线不能透明");
  assert.notEqual(restingColor, "rgba(0, 0, 0, 0)", "静止态分隔线必须有可见像素");

  await resizer.focus();
  await page.waitForTimeout(200);
  const focusedColor = await resizer.evaluate((element) => getComputedStyle(element, "::after").backgroundColor);
  assert.notEqual(focusedColor, restingColor, "聚焦态分隔线应显示强调色");
  await page.keyboard.press("ArrowLeft");
  assert.ok(Math.abs(((await panel.boundingBox())?.width || 0) - 464) <= 2, "ArrowLeft 应将右侧栏加宽 24px");
  await page.keyboard.press("ArrowRight");
  assert.ok(Math.abs(((await panel.boundingBox())?.width || 0) - 440) <= 2, "ArrowRight 应将右侧栏缩窄 24px");

  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + 120);
  await page.mouse.down();
  assert.equal(await resizer.evaluate((element) => element.classList.contains("is-dragging")), true, "拖动时应暴露交互状态");
  await page.mouse.move(handleBox.x - 80, handleBox.y + 120, { steps: 5 });
  await page.mouse.up();

  const draggedWidth = (await panel.boundingBox())?.width;
  assert.ok(draggedWidth && Math.abs(draggedWidth - 520) <= 2, `向左拖动 80px 后应约为 520px，实际 ${draggedWidth}px`);

  await page.getByRole("button", { name: "关闭最终交付物" }).click();
  assert.equal(await panel.count(), 0, "关闭按钮应关闭侧栏");
  assert.equal(await trigger.getAttribute("aria-expanded"), "false");
  await trigger.click();
  assert.ok(Math.abs(((await panel.boundingBox())?.width || 0) - draggedWidth) <= 1, "同页重开应保留调整后的宽度");

  await page.reload({ waitUntil: "load" });
  const reloadedTrigger = page.getByRole("button", { name: "查看最终交付物" });
  await reloadedTrigger.waitFor({ state: "visible" });
  await reloadedTrigger.click();
  assert.ok(Math.abs(((await panel.boundingBox())?.width || 0) - 440) <= 1, "刷新后应恢复 440px");

  await context.close();
  console.log("PASS deliverable side panel acceptance");
} finally {
  await browser.close();
}
