import { describe, expect, it, vi } from 'vitest'
import { handleWeaveDeliverableRequest } from '../src/deliverable-content.ts'
import { workTaskProjectionDefinition } from '../src/index.ts'

function task(contentType = 'text/markdown') {
  return workTaskProjectionDefinition.wire.viewSchema.parse({
    clientRequestId: 'request-1', teamId: 'team-1', teamName: 'Team', workflowName: '', runId: 'run-1',
    status: 'completed', completedStages: 1, totalStages: 1, latestStage: 'deliver', runtimes: [],
    humanTaskCount: 0, deliverableCount: 1, deliverables: [{ id: 'output-1', title: 'report.md', kind: 'final',
      contentType, preview: 'bounded preview', content: 'bounded preview', truncated: true, createdAt: '' }],
    blocker: 'none', updatedAt: 1,
  })
}
const request = (query = '') => new Request(`http://host/api/weave.deliverable?runId=run-1&id=output-1${query}`)

describe('Workbench complete deliverable content', () => {
  it('streams complete UTF-8 content from the actual content route after the projection was truncated', async () => {
    const complete = '完整正文。'.repeat(60_000)
    const fetcher = vi.fn<typeof fetch>(async (url, init) => {
      expect(url).toBe('http://weave.test/v1/deliverables/output-1/content')
      expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer host-secret')
      return new Response(complete, { headers: { 'Content-Type': 'text/markdown; charset=utf-8', 'Content-Disposition': 'attachment; filename="report.md"' } })
    })
    const response = await handleWeaveDeliverableRequest('http://weave.test', 'host-secret', request(), task(), fetcher)
    expect(await response.text()).toBe(complete)
    expect(response.headers.get('Content-Disposition')).toContain('report.md')
    expect([...response.headers.values()].join(' ')).not.toContain('host-secret')
  })

  it('refuses another run or unknown artifact before reaching Weave', async () => {
    const fetcher = vi.fn<typeof fetch>()
    for (const query of ['runId=other&id=output-1', 'runId=run-1&id=unknown']) {
      const response = await handleWeaveDeliverableRequest('http://weave.test', 'secret', new Request(`http://host/api/weave.deliverable?${query}`), task(), fetcher)
      expect(response.status).toBe(404)
    }
    expect(fetcher).not.toHaveBeenCalled()
  })

  it('serves an SVG preview as an isolated image and never an active HTML document', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => new Response('<svg xmlns="http://www.w3.org/2000/svg"/>'))
    const response = await handleWeaveDeliverableRequest('http://weave.test', 'secret', request('&mode=preview'), task('image/svg+xml'), fetcher)
    expect(response.headers.get('Content-Type')).toBe('image/svg+xml')
    expect(response.headers.get('Content-Security-Policy')).toContain('sandbox')
    const html = await handleWeaveDeliverableRequest('http://weave.test', 'secret', request('&mode=preview'), task('text/html'), fetcher)
    expect(html.status).toBe(415)
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('hides backend errors and refuses a missing Host credential', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => new Response('private database detail', { status: 500 }))
    expect((await handleWeaveDeliverableRequest('http://weave.test', '', request(), task(), fetcher)).status).toBe(503)
    expect(fetcher).not.toHaveBeenCalled()
    const response = await handleWeaveDeliverableRequest('http://weave.test', 'secret', request(), task(), fetcher)
    expect(response.status).toBe(502)
    expect(await response.text()).not.toContain('private database detail')
  })
})
