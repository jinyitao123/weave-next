import { describe, expect, it, vi } from 'vitest'
import { inspectWeaveReadiness } from '../src/readiness.ts'

describe('Workbench Weave readiness', () => {
  it('never probes or returns secret material when the host credential is absent', async () => {
    const fetcher = vi.fn<typeof fetch>()
    const result = await inspectWeaveReadiness('http://weave.test', '', fetcher)
    expect(result.status).toBe('unconfigured')
    expect(fetcher).not.toHaveBeenCalled()
    expect(JSON.stringify(result)).not.toContain('Bearer')
    expect(JSON.stringify(result)).not.toContain('apiKey')
  })

  it('reports a dispatchable team and healthy runtime from authenticated host probes', async () => {
    const fetcher = vi.fn<typeof fetch>(async (input) => {
      const url = String(input)
      if (url.endsWith('/v1/health')) return Response.json({ status: 'ok', version: '1.0.0' })
      if (url.includes('/v1/teams')) return Response.json([{
        id: 'team-1', default_workflow_id: 'workflow-1',
        summary: { published_workflow_count: 1 },
      }])
      if (url.endsWith('/v1/runtimes')) return Response.json({
        runtimes: [{ id: 'runtime-1', online: true, enabled: true, health_status: 'healthy' }],
      })
      return new Response(null, { status: 404 })
    })
    const result = await inspectWeaveReadiness('http://weave.test', 'secret-value', fetcher)
    expect(result).toMatchObject({ status: 'ready', dispatchableTeamCount: 1, healthyRuntimeCount: 1 })
    expect(fetcher).toHaveBeenCalledTimes(3)
    expect(JSON.stringify(result)).not.toContain('secret-value')
  })
})
