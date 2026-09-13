import { describe, expect, it, vi } from 'vitest'
import { handleCapabilityOperationsRequest } from '../src/capability-operations-control.ts'

describe('capability operations proxy', () => {
  it('returns isolated history and quota without exposing the Host credential', async () => {
    const fetcher = vi.fn<typeof fetch>(async (input) => String(input).endsWith('/v1/capability-quotas')
      ? Response.json({ max_steps_per_invocation: 80, max_active_invocations: 4 })
      : Response.json({ invocations: [{ invocation_id: 'run-1', status: 'running' }] }))
    const response = await handleCapabilityOperationsRequest('http://weave', 'HOST_PRIVATE', new Request('http://host/api/weave.capability-operations', { headers: { cookie: 'dsh-browser=first' } }), fetcher)
    expect(response.status).toBe(200)
    const text = await response.text()
    expect(text).toContain('run-1')
    expect(text).not.toContain('HOST_PRIVATE')
    expect(fetcher).toHaveBeenCalledTimes(2)
    const headers = (fetcher.mock.calls[0]?.[1]?.headers ?? {}) as Record<string, string>
    expect(headers['X-Weave-Actor-ID']).toBeUndefined()
  })

  it('proxies an exact human decision', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ status: 'queued' }, { status: 202 }))
    const response = await handleCapabilityOperationsRequest('http://weave', 'host', new Request('http://host/api/weave.capability-operations', {
      method: 'POST', body: JSON.stringify({ action: 'resume', invocation_id: 'run-1', step_id: 'review', approved: true }),
    }), fetcher)
    expect(response.status).toBe(202)
    expect(fetcher).toHaveBeenCalledWith('http://weave/v1/invocations/run-1/resume', expect.objectContaining({
      method: 'POST', body: JSON.stringify({ step_id: 'review', response: { approved: true } }),
    }))
  })

  it('downloads the final business document', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ invocation: { result: { document_markdown: '# 完成文档' } } }))
    const response = await handleCapabilityOperationsRequest('http://weave', 'host', new Request('http://host/api/weave.capability-operations?invocation_id=run-1&download=1'), fetcher)
    expect(response.headers.get('content-type')).toContain('text/markdown')
    await expect(response.text()).resolves.toBe('# 完成文档')
  })
})
