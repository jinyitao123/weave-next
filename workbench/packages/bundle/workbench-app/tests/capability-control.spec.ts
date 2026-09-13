import { describe, it, expect, vi } from 'vitest'
import { handleCapabilityRequest } from '../src/capability-control.ts'

describe('capability Host proxy', () => {
  it('keeps the credential on the Host and only sends the declared invocation', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ invocation_id: 'inv', status: 'queued' }, { status: 202 }))
    const response = await handleCapabilityRequest('http://weave.test', 'private-host-key', new Request('http://host/api/weave.capabilities', {
      method: 'POST', body: JSON.stringify({ action: 'invoke', id: 'cap', revision: 1, requestId: 'retry-id', input: { value: 1 } }),
    }), fetcher)
    expect(fetcher).toHaveBeenCalledWith('http://weave.test/v1/capabilities/cap/versions/1/invocations', expect.objectContaining({
      headers: { Authorization: 'Bearer private-host-key', 'Content-Type': 'application/json' },
      body: JSON.stringify({ request_id: 'retry-id', input: { value: 1 } }), redirect: 'error',
    }))
    expect(response.status).toBe(202)
    expect(await response.text()).not.toContain('private-host-key')
  })
  it('rejects arbitrary paths before forwarding', async () => {
    const fetcher = vi.fn<typeof fetch>()
    const response = await handleCapabilityRequest('http://weave.test', 'key', new Request('http://host/api/weave.capabilities', {
      method: 'POST', body: JSON.stringify({ action: 'fetch', path: '/v1/auth/api-keys' }),
    }), fetcher)
    expect(response.status).toBe(400)
    expect(fetcher).not.toHaveBeenCalled()
  })
})
