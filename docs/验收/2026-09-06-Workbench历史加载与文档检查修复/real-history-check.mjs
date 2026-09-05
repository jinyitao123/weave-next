/** Read-only browser verification against the retained large local Workbench conversation. */
import { createRequire } from 'node:module'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'

const output = dirname(fileURLToPath(import.meta.url))
const repo = resolve(output, '../../..')
const { chromium } = createRequire(join(repo, 'workbench/apps/web/package.json'))('playwright')
const authUrl = (await readFile('/private/tmp/weave-ux-workbench-server.log', 'utf8'))
  .match(/http:\/\/127\.0\.0\.1:13081\/\?token=[^\s]+/)?.[0]
assert.ok(authUrl, 'running local Workbench authentication URL')
const browser = await chromium.launch()
const result = { date: new Date().toISOString(), passed: false, openings: [], views: [], pageErrors: [] }
const page = await browser.newPage({ locale: 'zh-CN', viewport: { width: 1440, height: 900 } })
const findOpening = (value, depth = 0) => {
  if (value === null || typeof value !== 'object' || depth > 8) return undefined
  if (value.type === 'snapshot' && Array.isArray(value.records) && value.projections) return value
  for (const [key, child] of Object.entries(value)) {
    if (key === 'records' || key === 'projections') continue
    const found = findOpening(child, depth + 1)
    if (found) return found
  }
  return undefined
}
page.on('pageerror', error => result.pageErrors.push(error.message.replace(/token=[^\s&]+/g, 'token=REDACTED')))
page.on('websocket', socket => socket.on('framereceived', ({ payload }) => {
  const text = typeof payload === 'string' ? payload : payload.toString('utf8')
  let value
  try { value = JSON.parse(text) } catch { return }
  const opening = findOpening(value)
  if (!opening || opening.projections.values.workTask?.teamName === undefined) return
  const task = opening.projections.values.workTask
  const ranges = opening.records.filter(record => record.type === 'projection')
  result.openings.push({ sessionId: opening.header.id, teamName: task.teamName, status: task.status,
    frameCharacters: text.length, frameUtf8Bytes: Buffer.byteLength(text), records: opening.records.length,
    cursor: opening.cursor, baselineCursor: opening.projections.asOfSeq,
    taskSnapshots: opening.records.filter(record => record.event.type === 'weave/work-task').length,
    representedSnapshots: ranges.filter(record => record.event.data.key === 'workTask')
      .reduce((count, record) => count + record.event.data.throughSeq - record.event.seq + 1, 0),
    ranges: ranges.length, messages: opening.records.filter(record => ['user/message', 'assistant/message'].includes(record.event.type)).length,
    members: task.members.length, attempts: task.attempts.length, deliverables: task.deliverables.length })
}))
const capture = async (label, file) => {
  await page.waitForFunction(() => [...document.querySelectorAll('[data-weave-work-task]')]
    .some(element => element.getBoundingClientRect().width >= 480))
  await page.waitForTimeout(250)
  const view = await page.evaluate(() => ({ documentWidth: document.documentElement.scrollWidth,
    viewportWidth: innerWidth, nodes: document.querySelectorAll('*').length, bodyCharacters: document.body.innerText.length,
    hasDialogue: document.querySelector('[data-conversation-scroll]')?.textContent.includes('已按确认方案完成交接') ?? false,
    sceneVisible: [...document.querySelectorAll('[data-weave-work-task]')].some(element => element.getBoundingClientRect().height > 0),
  }))
  result.views.push({ label, ...view })
  assert.ok(view.hasDialogue, label + ': dialogue rendered')
  assert.equal(view.documentWidth, view.viewportWidth, label + ': no horizontal page overflow')
  await page.screenshot({ path: join(output, file) })
}
try {
  await page.goto(authUrl)
  await page.getByText('上线前本地知识库体验评审', { exact: true }).click()
  await page.waitForFunction(() => document.querySelector('[data-conversation-scroll]')?.textContent.includes('已按确认方案完成交接'))
  await page.getByRole('button', { name: '打开工作现场', exact: true }).click()
  const scene = page.locator('[data-weave-work-task]:visible')
  await scene.waitFor()
  await scene.getByRole('button', { name: /体验验收负责人/ }).first().click()
  await scene.locator('[data-weave-member-reader]').waitFor()
  await capture('large-history-opened', '01-large-history-opened.png')
  await page.reload()
  await page.waitForFunction(() => document.querySelector('[data-conversation-scroll]')?.textContent.includes('已按确认方案完成交接'))
  await page.getByRole('button', { name: '打开工作现场', exact: true }).click()
  await scene.locator('[data-weave-member-reader]').waitFor()
  await capture('large-history-reloaded', '02-large-history-reloaded.png')
  const opening = result.openings.find(item => item.representedSnapshots >= 11_318)
  assert.ok(opening, 'original accumulated snapshot range retained')
  assert.equal(opening.taskSnapshots, 0)
  assert.ok(opening.frameCharacters < 500_000, 'opening no longer repeats full snapshots')
  assert.ok(opening.messages >= 9, 'original messages retained')
  assert.equal(opening.baselineCursor, opening.cursor)
  const cdp = await page.context().newCDPSession(page)
  const heap = await cdp.send('Runtime.getHeapUsage')
  result.heapUsedBytes = heap.usedSize
  await cdp.detach()
  assert.deepEqual(result.pageErrors, [])
  result.passed = true
} finally {
  await writeFile(join(output, 'real-history-results.json'), JSON.stringify(result, null, 2) + '\n')
  await browser.close()
}
