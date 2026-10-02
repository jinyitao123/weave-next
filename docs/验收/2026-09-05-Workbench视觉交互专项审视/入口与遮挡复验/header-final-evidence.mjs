/** Final header-only check after the narrow header width correction. */
import { createRequire } from 'node:module'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'
const evidence = dirname(fileURLToPath(import.meta.url))
const repo = resolve(evidence, '../../../../')
const { chromium } = createRequire(join(repo, 'workbench/apps/web/package.json'))('playwright')
const authUrl = (await readFile('<LOCAL_TEMP_PATH>, 'utf8')).match(/http:\/\/127\.0\.0\.1:13081\/\?token=[^\s]+/)?.[0]
assert.ok(authUrl)
const browser = await chromium.launch({ headless: true })
const result = { date: new Date().toISOString(), provenance: '真实历史会话、本地正式构建，只验证最终顶部入口宽度，不重复滚动流程', cases: [], errors: [] }
try {
  for (const width of [390, 1440]) {
    const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
    page.on('pageerror', e => result.errors.push(e.message.replace(/token=[^\s&]+/g, 'token=REDACTED')))
    await page.goto(authUrl)
    await page.getByText('异步派发停止流程验收简报', { exact: true }).click()
    await page.locator('[data-weave-task-card]').waitFor()
    await page.setViewportSize({ width, height: width === 390 ? 844 : 900 })
    await page.waitForTimeout(500)
    const header = page.getByRole('button', { name: '打开工作现场', exact: true })
    assert.ok(await header.isVisible())
    const measurements = await header.evaluate(el => ({ width: innerWidth, documentWidth: document.documentElement.scrollWidth, button: el.getBoundingClientRect().toJSON(), spans: [...el.querySelectorAll('span')].map(s => ({ text: s.innerText, width: s.clientWidth, scrollWidth: s.scrollWidth })) }))
    const title = await page.getByRole('button', { name: '异步派发停止流程验收简报', exact: true }).boundingBox()
    const status = measurements.spans.find(s => s.text === '已停止')
    assert.ok(status && status.width >= status.scrollWidth, 'short stopped status fully visible')
    assert.ok(title && title.width >= 90, 'title retains readable space')
    assert.ok(measurements.documentWidth <= width, 'no horizontal overflow')
    assert.ok(title.x + title.width <= measurements.button.x + 1, 'title and scene button do not overlap')
    await page.screenshot({ path: join(evidence, `H05-${width}-final-header.png`) })
    result.cases.push({ ...measurements, title, passed: true })
    await page.close()
  }
  assert.deepEqual(result.errors, [])
  result.passed = true
} catch (e) {
  result.passed = false
  result.failure = String(e.message).replace(/token=[^\s&]+/g, 'token=REDACTED')
  process.exitCode = 1
} finally {
  await browser.close()
  await writeFile(join(evidence, 'header-final-results.json'), JSON.stringify(result, null, 2) + '\n')
  console.log(JSON.stringify(result, null, 2))
}
