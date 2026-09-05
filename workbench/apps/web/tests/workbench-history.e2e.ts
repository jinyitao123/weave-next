/** Workbench conversation recovery over accumulated task observations in the shipped composition. */
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { expect, it, vi } from 'vitest'
import { workTaskProjectionDefinition } from '@deepseek-ai/dsh-workbench-app'
import { launchWebScaffold, parseSeedFixture, renderSeedFixture, seedSession } from './scaffold.ts'

it('opens and reloads dialogue with thousands of historical task snapshots', async () => {
  vi.stubEnv('WEAVE_API_KEY', '')
  const scaffold = await launchWebScaffold({
    extraOverlayPath: fileURLToPath(new URL('../../../packages/bundle/workbench-app/cordis.patch.yml', import.meta.url)),
    extraInstallAnchors: [fileURLToPath(new URL('../../../packages/bundle/workbench-app/package.json', import.meta.url))],
  })
  const browser = await chromium.launch()
  try {
    const source = parseSeedFixture(await readFile(new URL('../../../snapshots/web/seeded-history/session.jsonl', import.meta.url), 'utf8'))
    const task = workTaskProjectionDefinition.wire.viewSchema.parse({
      runId: 'history-run', clientRequestId: 'history-request', teamId: 'history-team', teamName: '历史核验团队', workflowName: '',
      status: 'completed', waitKind: '', waitNodeId: '', completedStages: 2, totalStages: 2, latestStage: '核验完成',
      runtimes: [], humanTaskCount: 0, deliverableCount: 1, blocker: 'none', updatedAt: 1,
      deliverables: [{ id: 'history-output', title: '核验结果', kind: 'final', contentType: 'text/plain',
        preview: '历史成果仍然完整', content: '历史成果仍然完整\n'.repeat(2048), truncated: false, createdAt: '' }],
    })
    const ending = source.events.at(-1)!
    const observations = Array.from({ length: 2048 }, (_, index) => ({
      type: 'weave/work-task' as const, seq: ending.seq + index, time: ending.time + index,
      data: { ...task, observedAt: index + 1, updatedAt: index + 1 },
    }))
    const fixture = renderSeedFixture(source.headerLine, [
      ...source.events.slice(0, -1), ...observations, { ...ending, seq: ending.seq + observations.length },
    ])
    const sessionId = await seedSession(scaffold, fixture, 'workbench-large-history')
    const before = (await scaffold.ctx.sessionPersistence.inspect(sessionId)).events.length
    const abort = new AbortController()
    const follower = scaffold.ctx.sessionController.follow({ address: { kind: 'session', sessionId } }, abort.signal)[Symbol.asyncIterator]()
    const opened = await follower.next()
    if (opened.done) throw new Error('Session follow ended without a snapshot')
    const opening = opened.value
    abort.abort()
    await follower.return?.()
    expect(opening.type).toBe('snapshot')
    expect(Buffer.byteLength(JSON.stringify(opening))).toBeLessThan(512_000)
    const page = await browser.newPage({ locale: 'zh-CN', viewport: { width: 1440, height: 900 } })
    const pageErrors: string[] = []
    page.on('pageerror', error => pageErrors.push(error.message))
    await page.goto(scaffold.authenticatedUrl)
    const group = page.getByRole('treeitem').first()
    await group.waitFor()
    await group.click()
    await page.getByRole('treeitem').nth(1).click()
    await page.getByText('DONE', { exact: true }).waitFor({ timeout: 30_000 })
    await page.getByRole('button', { name: /工作现场/ }).click()
    await page.getByRole('tab', { name: '成果', exact: true }).click()
    await page.getByLabel('交付物', { exact: true }).getByText('核验结果', { exact: true }).waitFor()
    await page.reload()
    await page.getByText('DONE', { exact: true }).waitFor({ timeout: 30_000 })
    await page.getByRole('button', { name: /工作现场/ }).click()
    await page.getByLabel('交付物', { exact: true }).getByText('核验结果', { exact: true }).waitFor()
    expect(pageErrors).toEqual([])
    const retained = (await scaffold.ctx.sessionPersistence.inspect(sessionId)).events
    expect(retained.length).toBeGreaterThanOrEqual(before)
    expect(retained.filter(event => event.type === 'weave/work-task')).toHaveLength(observations.length)
  } finally {
    await browser.close()
    await scaffold.close()
    vi.unstubAllEnvs()
  }
})
