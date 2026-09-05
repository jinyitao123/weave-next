/** Read-only UI QA against the locally served production build and an existing stopped session. */
import { createRequire } from 'node:module'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'

const evidence = dirname(fileURLToPath(import.meta.url))
const repo = resolve(evidence, '../../../../')
const { chromium } = createRequire(join(repo, 'workbench/apps/web/package.json'))('playwright')
const log = await readFile('/private/tmp/weave-ux-workbench-server.log', 'utf8')
const authUrl = log.match(/http:\/\/127\.0\.0\.1:13081\/\?token=[^\s]+/)?.[0]
assert.ok(authUrl, 'local authenticated entry exists')
const result = { date: new Date().toISOString(), provenance: '真实历史会话与本地正式构建；仅浏览、展开既有记录、滚动，未发送消息或执行业务操作', session: '异步派发停止流程验收简报', cases: [], errors: [] }
const browser = await chromium.launch({ headless: true })
const measure = page => page.evaluate(() => {
  const scroll = document.querySelector('[data-conversation-scroll]')
  const card = document.querySelector('[data-weave-task-card]')
  const seat = document.querySelector('[data-composer-seat]')
  const rect = el => { const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, height: r.height, bottom: r.bottom, right: r.right } }
  const input = seat?.querySelector('textarea,[contenteditable=true]')
  return { viewport: { width: innerWidth, height: innerHeight }, document: { width: document.documentElement.clientWidth, scrollWidth: document.documentElement.scrollWidth }, scroll: { ...rect(scroll), top: scroll.scrollTop, scrollHeight: scroll.scrollHeight, clientHeight: scroll.clientHeight }, card: rect(card), composer: rect(seat), input: input ? rect(input) : null, cardInsideComposer: !!card.closest('[data-composer-seat]'), cardInsideFlowDock: !!card.closest('[data-conversation-flow-dock]'), composerPosition: getComputedStyle(seat).position, conversationReceipts: [...scroll.querySelectorAll('summary')].filter(e => e.textContent === '操作回执').length }
})
try {
  for (const width of [1440, 390]) {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const page = await context.newPage()
    page.on('pageerror', e => result.errors.push(e.message.replace(/token=[^\s&]+/g, 'token=REDACTED')))
    await page.goto(authUrl)
    await page.getByText(result.session, { exact: true }).click()
    await page.locator('[data-weave-task-card]').waitFor()
    await page.setViewportSize({ width, height: width === 390 ? 844 : 900 })
    await page.waitForTimeout(500)
    const header = page.getByRole('button', { name: '打开工作现场', exact: true })
    assert.ok(await header.isVisible(), 'scene header visible')
    assert.match(await header.innerText(), /工作现场\s*已停止/)
    assert.ok(await header.locator('svg').count() > 0, 'scene header has arrow icon')
    const headerSnapshot = await header.evaluate(el => ({ text: el.innerText, width: el.getBoundingClientRect().width, spans: [...el.querySelectorAll('span')].map(s => ({ text: s.innerText, width: s.clientWidth, scrollWidth: s.scrollWidth, textOverflow: getComputedStyle(s).textOverflow })) }))
    const scroll = page.locator('[data-conversation-scroll]')
    await scroll.evaluate(el => { el.scrollTop = el.scrollHeight })
    await page.waitForTimeout(300)
    const compact = await measure(page)
    assert.equal(compact.cardInsideComposer, false)
    assert.equal(compact.cardInsideFlowDock, true)
    assert.equal(compact.composerPosition, 'sticky')
    assert.equal(compact.conversationReceipts, 0)
    assert.ok(compact.card.height <= 70, 'stopped card stays one row')
    assert.ok(compact.document.scrollWidth <= width, 'compact document has no horizontal overflow')
    await page.screenshot({ path: join(evidence, `H01-${width}-compact.png`) })

    // Expand existing tool records only; this changes local presentation, not business state.
    await page.getByRole('button', { name: '3 次工具调用', exact: true }).click()
    await scroll.evaluate(el => { el.scrollTop = el.scrollHeight })
    await page.waitForTimeout(400)
    const bottom = await measure(page)
    assert.ok(bottom.scroll.scrollHeight > bottom.scroll.clientHeight + 200, 'real transcript scrolls')
    assert.ok(bottom.card.bottom <= bottom.composer.y + 1, 'card does not overlap composer at bottom')
    await page.screenshot({ path: join(evidence, `H02-${width}-long-bottom.png`) })
    await page.mouse.move(bottom.scroll.x + bottom.scroll.width / 2, bottom.scroll.y + bottom.scroll.height / 2)
    await page.mouse.wheel(0, -30000)
    await page.waitForFunction(() => document.querySelector('[data-conversation-scroll]').scrollTop === 0)
    await page.waitForTimeout(350)
    const top = await measure(page)
    assert.ok(top.card.y >= top.viewport.height, 'card scrolls below viewport when reading earlier content')
    assert.ok(Math.abs(top.composer.y - bottom.composer.y) < 2, 'composer remains fixed during scroll')
    assert.ok(top.input && top.input.y >= 0 && top.input.bottom <= top.viewport.height, 'input stays visible')
    assert.ok(top.document.scrollWidth <= width, 'long transcript has no horizontal overflow')
    await page.screenshot({ path: join(evidence, `H03-${width}-reading-top.png`) })

    await header.click()
    const scene = page.locator('[data-weave-work-task]:visible')
    await scene.waitFor()
    const receipt = scene.locator('details').filter({ has: page.locator('summary', { hasText: /^操作回执$/ }) }).first()
    assert.ok(await receipt.count() === 1, 'receipt exists in work scene')
    assert.equal(await receipt.getAttribute('open'), null, 'receipt collapsed by default')
    await receipt.locator('summary').first().click()
    assert.notEqual(await receipt.getAttribute('open'), null, 'receipt expands')
    const receiptText = await receipt.innerText()
    assert.ok(receiptText.length > '操作回执'.length, 'receipt contains historical outcome')
    const sceneMetrics = await page.evaluate(() => ({ width: innerWidth, scrollWidth: document.documentElement.scrollWidth }))
    assert.ok(sceneMetrics.scrollWidth <= width, 'scene has no horizontal overflow')
    await page.screenshot({ path: join(evidence, `H04-${width}-scene-receipt.png`) })
    result.cases.push({ width, compact, bottom, top, scene: { ...sceneMetrics, receiptText, receiptExpandable: true }, header: { ...headerSnapshot, arrow: true, opensScene: true }, passed: true })
    await context.close()
  }
  assert.deepEqual(result.errors, [])
  result.passed = true
} catch (error) {
  result.passed = false
  result.failure = String(error.message).replace(/token=[^\s&]+/g, 'token=REDACTED')
  process.exitCode = 1
} finally {
  await browser.close()
  await writeFile(join(evidence, 'real-session-results.json'), JSON.stringify(result, null, 2) + '\n')
  console.log(JSON.stringify(result, null, 2))
}
