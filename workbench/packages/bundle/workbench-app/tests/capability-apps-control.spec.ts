import { describe, expect, it, vi } from 'vitest'
import { handleCapabilityAppsRequest } from '../src/capability-apps-control.ts'

describe('application management proxy', () => {
  it('returns an issued application key without exposing the Host key', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ key: 'wv_cap_new_application' }, { status: 201 }))
    const response = await handleCapabilityAppsRequest('http://weave', 'HOST_PRIVATE', new Request('http://host/api/weave.capability-apps', {
      method: 'POST', body: JSON.stringify({ action: 'issue', app_id: 'app', name: 'Key', scopes: ['invoke', 'read'] }),
    }), fetcher)
    expect(fetcher).toHaveBeenCalledWith('http://weave/v1/capability-apps/actions', expect.objectContaining({ headers: { Authorization: 'Bearer HOST_PRIVATE', 'Content-Type': 'application/json' } }))
    const result = await response.text()
    expect(result).toContain('wv_cap_new_application')
    expect(result).not.toContain('HOST_PRIVATE')
    expect(response.headers.get('cache-control')).toBe('no-store')
  })
  it('rejects a management scope on an application credential', async () => {
    const fetcher = vi.fn<typeof fetch>()
    const response = await handleCapabilityAppsRequest('http://weave', 'key', new Request('http://host/api/weave.capability-apps', {
      method: 'POST', body: JSON.stringify({ action: 'issue', app_id: 'app', name: 'Escalation', scopes: ['manage'] }),
    }), fetcher)
    expect(response.status).toBe(400)
    expect(fetcher).not.toHaveBeenCalled()
  })
})
